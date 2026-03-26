package store

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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

	// Pre-pull: fetch and integrate remote changes before committing
	// to minimize push conflicts. Use --ff-only to avoid merge commits;
	// if it fails (diverged), we'll handle it after commit via retry.
	if s.cfg.Git.AutoPush {
		_ = gitCmdQuiet(root, "pull", "--ff-only")
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

		// Pull with rebase to avoid merge commits in agent-state
		if pullErr := gitCmdQuiet(root, "pull", "--rebase"); pullErr != nil {
			// Check for active rebase (indicates conflict)
			conflictOut, _ := gitOutput(root, "ls-files", "-u")
			if strings.TrimSpace(conflictOut) != "" {
				// Abort the rebase and report
				_ = gitCmdQuiet(root, "rebase", "--abort")
				return fmt.Errorf("rebase conflict detected (aborted rebase, manual resolution needed)")
			}
			return fmt.Errorf("pull failed during retry: %w", pullErr)
		}
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
