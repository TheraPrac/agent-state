package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrMidRebase is returned when the workspace clone has a leftover
// rebase-in-progress state. Any st operation that touches git in this
// state would compound the corruption — refuse and surface a recovery
// hint instead. I-501.
var ErrMidRebase = errors.New("workspace is mid-rebase")

// ErrMidMerge is returned when the workspace clone has a leftover
// merge-in-progress state (MERGE_HEAD present). Same intent as
// ErrMidRebase. I-501.
var ErrMidMerge = errors.New("workspace is mid-merge")

// ErrStaleIndexLock is returned when .git/index.lock exists older than
// staleLockThreshold — a sign of a previous st (or git) process that
// died holding the lock. We don't auto-delete because the lock might be
// held by a still-running operator git command; surface the path + age
// so the operator can investigate. I-501.
var ErrStaleIndexLock = errors.New("stale .git/index.lock detected")

// staleLockThreshold is how old .git/index.lock must be before we treat
// it as abandoned. 30 seconds covers any reasonable in-flight git
// operation; longer than that and we should not silently ignore it.
const staleLockThreshold = 30 * time.Second

// PreFlightGitState inspects the canonical clone at root for leftover
// state from a previous failed git operation. Returns one of the
// sentinel errors above (with detailed message) when the clone is in a
// state where any further write would compound corruption. Returns nil
// when the clone is clean.
//
// Called at the top of every Store write path (Mutate, Create, GitSync)
// to honor the I-501 "stop making it worse" invariant. Cheap — three
// stat calls and one mtime read.
func PreFlightGitState(root string) error {
	gitDir, err := resolveGitDir(root)
	if err != nil {
		// No .git at root — agent-state isn't git-tracked here. Silently
		// pass; downstream calls will surface any write failure.
		return nil
	}

	// Mid-rebase: either .git/rebase-merge/ (interactive / merge-style)
	// or .git/rebase-apply/ (am-style) signal a paused rebase.
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		p := filepath.Join(gitDir, sub)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return fmt.Errorf("%w at %s — run `git -C %s rebase --abort` and retry", ErrMidRebase, p, root)
		}
	}

	// Mid-merge: MERGE_HEAD signals a paused merge (we don't initiate
	// these but defense-in-depth catches operator-driven merges).
	if p := filepath.Join(gitDir, "MERGE_HEAD"); fileExists(p) {
		return fmt.Errorf("%w at %s — run `git -C %s merge --abort` and retry", ErrMidMerge, p, root)
	}

	// Stale index.lock: present > staleLockThreshold ⇒ probably abandoned.
	// Don't auto-delete; the operator might still have a real git process
	// running. Surface enough detail to make the call.
	if p := filepath.Join(gitDir, "index.lock"); fileExists(p) {
		info, err := os.Stat(p)
		if err == nil {
			age := time.Since(info.ModTime())
			if age > staleLockThreshold {
				return fmt.Errorf("%w at %s (mtime %s, age %s) — investigate before deleting (no live git process should be running)",
					ErrStaleIndexLock, p, info.ModTime().Format(time.RFC3339), age.Round(time.Second))
			}
		}
	}

	return nil
}

// resolveGitDir returns the absolute path to the .git directory for a
// repo at root. Handles the common case (.git is a directory) and the
// worktree-pointer case (.git is a file containing "gitdir: <path>").
// Returns os.ErrNotExist when there is no .git at root.
func resolveGitDir(root string) (string, error) {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return dotGit, nil
	}
	// .git as a file means we're inside a worktree; the file points to
	// the real gitdir. Read it and resolve.
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir:"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			path := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(dotGit), path)
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("%s: malformed .git pointer (no gitdir line)", dotGit)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
