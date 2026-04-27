package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// RefreshOutcome describes how a RefreshWorkspace call resolved.
type RefreshOutcome int

const (
	RefreshDisabled RefreshOutcome = iota // git config nil — feature off
	RefreshUpToDate                       // fetch ok, no new commits
	RefreshPulled                         // fast-forwarded N commits
	RefreshDiverged                       // local has commits not on remote (ff-only refused)
	RefreshBlocked                        // uncommitted changes prevented ff
	RefreshOffline                        // fetch failed (network/auth/timeout)
)

// RefreshResult is the structured outcome of RefreshWorkspace, used by
// callers (e.g. st status) to render a banner explaining what happened
// and to decide whether to reload the store.
type RefreshResult struct {
	Outcome     RefreshOutcome
	PulledCount int   // commits fast-forwarded when Outcome == RefreshPulled
	Err         error // last underlying error (for diagnostics; not always set)
}

// refreshFetchTimeout caps the network round-trip so a flaky remote
// can't hang `st status`. Fetch failure → RefreshOffline; the caller
// renders against last-known-good local state.
const refreshFetchTimeout = 10 * time.Second

// RefreshWorkspace updates the workspace clone from origin, returning a
// structured outcome that callers can render. Unlike GitPull it always
// attempts the refresh (gated only by `cfg.Git != nil`, not AutoPush) —
// the contract of `st status` is "show me the current state," and an
// operator running it has explicitly asked for the network round-trip.
//
// Sequence:
//  1. Acquire the workspace git lock (concurrent st processes serialize).
//  2. Snapshot any locked-item files (in-flight pipeline state) so the
//     pull cannot overwrite them, mirroring GitPull's protection.
//  3. `git fetch origin` with a timeout — fetch failure → RefreshOffline.
//  4. Inspect ahead/behind counts:
//       - ahead > 0: RefreshDiverged (caller must `git pull --rebase`).
//       - behind == 0: RefreshUpToDate (silent success).
//       - behind > 0 with uncommitted blockers: RefreshBlocked.
//       - behind > 0 otherwise: `git pull --ff-only`. Success →
//         RefreshPulled (PulledCount = behind). Failure → RefreshBlocked.
//  5. Restore locked-item snapshots.
//
// Never auto-stashes, auto-rebases, or pushes. Operator intervention
// required for divergence and blocked states — banners surface them.
func RefreshWorkspace(cfg *config.Config) RefreshResult {
	if cfg.Git == nil {
		return RefreshResult{Outcome: RefreshDisabled}
	}

	root := cfg.ItemDir()

	unlock, err := acquireGitLock(root)
	if err != nil {
		// Another st process holds the lock — treat as offline rather than
		// blocking the user; they'll get fresh state on the next call.
		return RefreshResult{Outcome: RefreshOffline, Err: err}
	}
	defer unlock()

	lockedSnapshots := snapshotLockedItems(cfg, root)
	defer restoreLockedItems(lockedSnapshots)

	// 1. Fetch with timeout. Network/auth failure → offline.
	ctx, cancel := context.WithTimeout(context.Background(), refreshFetchTimeout)
	defer cancel()
	if err := gitCmdContext(ctx, root, "fetch", "--quiet", "origin"); err != nil {
		return RefreshResult{Outcome: RefreshOffline, Err: err}
	}

	// 2. Ahead/behind counts vs. upstream.
	behind, behindErr := gitCountCommits(root, "HEAD..@{upstream}")
	ahead, aheadErr := gitCountCommits(root, "@{upstream}..HEAD")
	if behindErr != nil || aheadErr != nil {
		// No upstream configured (or branch not tracking). Treat as up-to-date
		// rather than scaring the user — there's nothing to pull from.
		return RefreshResult{Outcome: RefreshUpToDate}
	}

	if ahead > 0 {
		return RefreshResult{Outcome: RefreshDiverged}
	}
	if behind == 0 {
		return RefreshResult{Outcome: RefreshUpToDate}
	}

	// 3. Pull. ff-only refuses if uncommitted changes conflict with
	// the merge or any other non-ff condition surfaces — surface as Blocked.
	if err := gitCmdQuiet(root, "pull", "--ff-only"); err != nil {
		return RefreshResult{Outcome: RefreshBlocked, Err: err}
	}

	return RefreshResult{Outcome: RefreshPulled, PulledCount: behind}
}

// gitCountCommits runs `git rev-list --count <range>` and parses the
// result. Returns 0 + error if the range can't be evaluated (e.g. no
// upstream configured).
func gitCountCommits(dir, revRange string) (int, error) {
	out, err := gitOutput(dir, "rev-list", "--count", revRange)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, err
	}
	return n, nil
}

// gitCmdContext runs git with a context (timeout-capable). Uses
// exec.CommandContext so the process is killed when the context expires.
func gitCmdContext(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
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
