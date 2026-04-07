package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// gitLockTimeout is how long to wait for the git lock before giving up.
const gitLockTimeout = 15 * time.Second

// acquireGitLock takes an exclusive file lock on a .st-git.lock file
// in the item directory. Returns an unlock function. If the lock can't
// be acquired within gitLockTimeout, returns an error.
func acquireGitLock(dir string) (func(), error) {
	lockPath := filepath.Join(dir, ".st-git.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	deadline := time.Now().Add(gitLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("git lock timeout after %s (another st process is syncing)", gitLockTimeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// GitPull pulls latest changes from remote before reading items.
// Never destroys local work: uses git pull --ff-only, which refuses to
// advance when it would overwrite uncommitted changes or when history
// has diverged. Both failure modes are silent and safe — we just skip
// the pull and continue with current state; any subsequent GitSync
// will reconcile local and remote.
//
// History: earlier versions auto-committed uncommitted changes before
// pulling, which caused rebase conflicts when remote had archived a
// file locally still present in issues/. The fix for that was to
// aggressively `git checkout -- . && git clean -fd` before pulling,
// but that silently discarded in-progress mutations from state-changing
// commands (e.g. `st close` writes the move, the next command's pre-run
// GitPull then reverts it — the item pops back into issues/ and the
// close is lost). --ff-only is the conservative middle: no rebase,
// no destructive cleanup; just fetch and fast-forward when it's safe.
func GitPull(cfg *config.Config) error {
	if cfg.Git == nil || !cfg.Git.AutoPush {
		return nil
	}

	unlock, err := acquireGitLock(cfg.ItemDir())
	if err != nil {
		return nil
	}
	defer unlock()

	root := cfg.ItemDir()

	// Snapshot locked item files before any git operations.
	// These are items currently being worked on by a pipeline —
	// we must not let a pull overwrite their state even if ff-only
	// would cleanly advance through them.
	lockedSnapshots := snapshotLockedItems(cfg, root)

	// Fast-forward-only pull. Git refuses the fast-forward if:
	//   - history has diverged (local commits not on remote), or
	//   - uncommitted working-tree changes conflict with files the
	//     merge would touch.
	// In either case we silently skip; the next sync will reconcile.
	_ = gitCmdQuiet(root, "pull", "--ff-only")

	// Restore locked item files if the pull changed them.
	restoreLockedItems(lockedSnapshots)

	return nil
}

// snapshotLockedItems reads the content of all locked item files.
// Returns a map of file path -> content for restoration after pull.
func snapshotLockedItems(cfg *config.Config, root string) map[string][]byte {
	locked := LockedItems(cfg)
	if len(locked) == 0 {
		return nil
	}

	// Build a set for fast lookup
	lockedSet := make(map[string]bool, len(locked))
	for _, id := range locked {
		lockedSet[id] = true
	}

	snapshots := make(map[string][]byte)
	// Scan all type directories for files matching locked IDs.
	// Filenames are like "T-103-title-slug.md" — match by ID prefix.
	for _, tc := range cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirPath := filepath.Join(root, dir)
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				// Extract ID: everything before the second hyphen-delimited segment
				// e.g., "T-103-title-slug.md" -> check if "T-103" is locked
				name := strings.TrimSuffix(e.Name(), ".md")
				for id := range lockedSet {
					if name == id || strings.HasPrefix(name, id+"-") {
						path := filepath.Join(dirPath, e.Name())
						if data, err := os.ReadFile(path); err == nil {
							snapshots[path] = data
						}
						break
					}
				}
			}
		}
	}
	return snapshots
}

// restoreLockedItems writes back snapshotted content for any files
// that were changed by a git pull.
func restoreLockedItems(snapshots map[string][]byte) {
	for path, originalData := range snapshots {
		currentData, err := os.ReadFile(path)
		if err != nil || string(currentData) != string(originalData) {
			os.WriteFile(path, originalData, 0644)
		}
	}
}

// GitSync stages only the files modified by this Store instance, commits,
// and pushes. This prevents unrelated dirty working-tree files from being
// swept into the commit — the root cause of cross-item state clobbering
// when parallel st processes modify different items.
//
// Use GitSyncAll for the explicit "commit everything" case (st sync).
func (s *Store) GitSync(message string) error {
	if s.cfg.Git == nil || !s.cfg.Git.AutoCommit {
		return nil
	}

	files := s.DirtyFiles()
	if len(files) == 0 {
		// Nothing tracked as dirty — fall through to check if there are
		// staged changes from a prior add (e.g., index.md regeneration).
		return s.gitSyncInternal(message, nil)
	}

	return s.gitSyncInternal(message, files)
}

// GitSyncAll stages ALL changes in the item root (git add -A) and commits.
// This is the broad "commit everything" mode used by `st sync`.
func (s *Store) GitSyncAll(message string) error {
	if s.cfg.Git == nil || !s.cfg.Git.AutoCommit {
		return nil
	}

	// Clear dirty set since we're committing everything
	s.DirtyFiles()

	return s.gitSyncInternal(message, nil)
}

// gitSyncInternal is the shared commit/push logic.
// If files is non-nil, only those files are staged (git add <files>).
// If files is nil, all changes are staged (git add -A).
func (s *Store) gitSyncInternal(message string, files []string) error {
	root := s.cfg.ItemDir()

	// Acquire lock to prevent concurrent git operations from parallel st processes
	unlock, err := acquireGitLock(root)
	if err != nil {
		return fmt.Errorf("git lock: %w", err)
	}
	defer unlock()

	// Pre-pull: fetch and integrate remote changes before committing.
	// Snapshot locked items so the pull can't overwrite active work.
	if s.cfg.Git.AutoPush {
		snap := snapshotLockedItems(s.cfg, root)
		_ = gitCmdQuiet(root, "pull", "--ff-only")
		restoreLockedItems(snap)
	}

	// Stage changes
	if files != nil {
		// Scoped: only stage the files this Store instance modified.
		// Convert absolute paths to relative paths from the git root.
		for _, f := range files {
			rel, err := filepath.Rel(root, f)
			if err != nil {
				rel = f // fallback to absolute
			}
			if err := gitCmd(root, "add", "--", rel); err != nil {
				return fmt.Errorf("git add %s: %w", rel, err)
			}
		}
	} else {
		// Broad: stage everything (used by st sync / fallback)
		if err := gitCmd(root, "add", "-A"); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
	}

	// Check if there's anything to commit
	out, err := gitOutput(root, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil // nothing to commit
	}

	// Commit
	if err := gitCmd(root, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push with retry
	if s.cfg.Git.AutoPush {
		if err := s.pushWithRetry(root, 3); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}

	return nil
}

func (s *Store) pushWithRetry(root string, maxRetries int) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := gitCmd(root, "push")
		if err == nil {
			return nil
		}

		if attempt == maxRetries {
			return err
		}

		// Pull with rebase to avoid merge commits in agent-state.
		// Protect locked items across the rebase.
		snap := snapshotLockedItems(s.cfg, root)
		if pullErr := gitCmdQuiet(root, "pull", "--rebase"); pullErr != nil {
			restoreLockedItems(snap)
			// Check for active rebase (indicates conflict)
			conflictOut, _ := gitOutput(root, "ls-files", "-u")
			if strings.TrimSpace(conflictOut) != "" {
				// Abort the rebase and report
				_ = gitCmdQuiet(root, "rebase", "--abort")
				return fmt.Errorf("rebase conflict detected (aborted rebase, manual resolution needed)")
			}
			return fmt.Errorf("pull failed during retry: %w", pullErr)
		}
		restoreLockedItems(snap)
	}
	return nil
}

func gitCmd(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitCmdQuiet runs a git command silently (no stdout/stderr forwarding).
func gitCmdQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
