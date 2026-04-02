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
// Preserves any uncommitted local changes (e.g., test evidence written
// but not yet synced) by committing them first, then pulling with rebase.
// Item-level locks: files for locked items are snapshotted before the pull
// and restored after, so concurrent pulls can't revert active item state.
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
	// we must not let a pull overwrite their state.
	lockedSnapshots := snapshotLockedItems(cfg, root)

	// Commit any uncommitted local changes BEFORE pulling,
	// so they don't get overwritten by the remote.
	out, err := gitOutput(root, "status", "--porcelain")
	if err == nil && strings.TrimSpace(out) != "" {
		_ = gitCmdQuiet(root, "add", "-A")
		_ = gitCmdQuiet(root, "commit", "-m", "auto-save: preserve local changes before pull")
	}

	// Pull with rebase to avoid merge commits
	if err := gitCmdQuiet(root, "pull", "--rebase"); err != nil {
		// If rebase fails (conflict), abort and continue with local state
		_ = gitCmdQuiet(root, "rebase", "--abort")
		restoreLockedItems(lockedSnapshots)
		return nil
	}

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

// GitSync stages, commits, and pushes changes in the item root directory.
// Message is the commit message. Pre-pulls with --ff-only before committing
// to minimize conflicts. If push fails (remote ahead), retries with
// pull --rebase + re-push up to maxRetries times. Detects rebase conflicts
// and aborts cleanly with an error.
func (s *Store) GitSync(message string) error {
	if s.cfg.Git == nil || !s.cfg.Git.AutoCommit {
		return nil
	}

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

	// Stage all changes in the item root
	if err := gitCmd(root, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
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
