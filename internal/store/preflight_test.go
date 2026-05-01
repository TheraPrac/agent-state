package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo creates a minimal git repo at root and returns the path.
// Used by preflight tests that need a real .git tree to manipulate.
func initRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "-b", "main", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return root
}

func TestPreFlightCleanRepoReturnsNil(t *testing.T) {
	root := initRepo(t)
	if err := PreFlightGitState(root); err != nil {
		t.Errorf("clean repo should pass pre-flight, got: %v", err)
	}
}

func TestPreFlightDetectsMidRebaseMerge(t *testing.T) {
	root := initRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".git", "rebase-merge"), 0755); err != nil {
		t.Fatal(err)
	}
	err := PreFlightGitState(root)
	if !errors.Is(err, ErrMidRebase) {
		t.Fatalf("expected ErrMidRebase, got %v", err)
	}
	if !strings.Contains(err.Error(), "rebase --abort") {
		t.Errorf("error should mention recovery command; got: %s", err)
	}
}

func TestPreFlightDetectsMidRebaseApply(t *testing.T) {
	root := initRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".git", "rebase-apply"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := PreFlightGitState(root); !errors.Is(err, ErrMidRebase) {
		t.Errorf("expected ErrMidRebase, got %v", err)
	}
}

func TestPreFlightDetectsMidMerge(t *testing.T) {
	root := initRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".git", "MERGE_HEAD"), []byte("deadbeef\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := PreFlightGitState(root)
	if !errors.Is(err, ErrMidMerge) {
		t.Fatalf("expected ErrMidMerge, got %v", err)
	}
	if !strings.Contains(err.Error(), "merge --abort") {
		t.Errorf("error should mention recovery command; got: %s", err)
	}
}

func TestPreFlightDetectsStaleIndexLock(t *testing.T) {
	root := initRepo(t)
	lockPath := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("stale\n"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	err := PreFlightGitState(root)
	if !errors.Is(err, ErrStaleIndexLock) {
		t.Fatalf("expected ErrStaleIndexLock, got %v", err)
	}
	if !strings.Contains(err.Error(), "investigate") {
		t.Errorf("error should advise investigation; got: %s", err)
	}
}

func TestPreFlightFreshIndexLockPasses(t *testing.T) {
	root := initRepo(t)
	lockPath := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("fresh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Default mtime is now — within staleLockThreshold, should pass.
	if err := PreFlightGitState(root); err != nil {
		t.Errorf("fresh index.lock should pass pre-flight, got: %v", err)
	}
}

func TestPreFlightNoGitDirIsSilentPass(t *testing.T) {
	dir := t.TempDir()
	if err := PreFlightGitState(dir); err != nil {
		t.Errorf("non-git dir should pass silently, got: %v", err)
	}
}

func TestPreFlightGitFileWorktreePointer(t *testing.T) {
	// Simulate a worktree: replace .git directory with a .git file
	// pointing at a real gitdir. PreFlight must follow the pointer to
	// find leftover state.
	root := t.TempDir()
	realGitDir := filepath.Join(root, "real-gitdir")
	if err := os.MkdirAll(realGitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"),
		[]byte("gitdir: real-gitdir\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// No leftover state — should pass.
	if err := PreFlightGitState(root); err != nil {
		t.Errorf("clean worktree pointer should pass, got: %v", err)
	}

	// Plant rebase-merge in the resolved gitdir.
	if err := os.MkdirAll(filepath.Join(realGitDir, "rebase-merge"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := PreFlightGitState(root); !errors.Is(err, ErrMidRebase) {
		t.Errorf("expected ErrMidRebase via worktree pointer, got %v", err)
	}
}
