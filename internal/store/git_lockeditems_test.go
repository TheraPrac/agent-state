package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// I-1722: restoreLockedItems previously reverted a locked item whenever its
// current bytes differed from the pre-pull snapshot — for ANY reason,
// including a concurrent legitimate Mutate by the item's owner. Observed
// live 2026-07-02: plan_approved and testing_evidence.uat writes on I-1718
// were silently destroyed by peer GitPull/GitSync straddles. These tests pin
// the corrected contract: restore fires only for paths the pull itself
// changed (per git's own diff between the pre- and post-pull HEADs), and
// never touches the tree when HEAD did not move.

// setupLockedItemRepo builds a git repo with the standard layout, one task
// item, and a pipeline lock (.locks/<id>) on it, returning the cfg, the
// item's absolute path, and a git runner.
func setupLockedItemRepo(t *testing.T) (*config.Config, string, func(...string)) {
	t.Helper()
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	if err := LockItem(cfg, "T-001", "test-session"); err != nil {
		t.Fatalf("LockItem: %v", err)
	}

	// setupTestDir seeds tasks/T-001-first-task.md (tracked + committed by
	// initGitRepo).
	itemPath := filepath.Join(root, "tasks", "T-001-first-task.md")
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("fixture item missing: %v", err)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return cfg, itemPath, run
}

// TestRestoreSkipsConcurrentWriteOnNoopPull reproduces the exact I-1718
// incident shape: a peer's snapshot→(no-op pull)→restore straddles the
// owner's write. HEAD never moves, so restore must leave the concurrent
// write intact. Under the pre-I-1722 code this test fails: the write is
// reverted to the snapshot.
func TestRestoreSkipsConcurrentWriteOnNoopPull(t *testing.T) {
	cfg, itemPath, _ := setupLockedItemRepo(t)
	root := cfg.ItemDir()

	snap := snapshotLockedItems(cfg, root)
	if len(snap) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snap))
	}
	head := headSHA(root)
	if head == "" {
		t.Fatal("no HEAD sha")
	}

	// Concurrent legitimate write lands between snapshot and restore
	// (in production: the owner's Store.Mutate, e.g. plan_approved: true).
	concurrent := "id: T-001\ntype: task\nstatus: queued\ntitle: First task\nplan_approved: true\n"
	if err := os.WriteFile(itemPath, []byte(concurrent), 0644); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// The pull was a no-op: oldHead == newHead.
	restoreLockedItems(root, snap, head, head)

	got, _ := os.ReadFile(itemPath)
	if string(got) != concurrent {
		t.Errorf("restore reverted a concurrent write on a no-op pull (I-1722 regression):\ngot:\n%s", got)
	}
}

// TestRestoreSkipsUntouchedPathWhenHeadMoves: the pull advances HEAD but
// changes a DIFFERENT file; the locked item's concurrent write must survive.
func TestRestoreSkipsUntouchedPathWhenHeadMoves(t *testing.T) {
	cfg, itemPath, gitRun := setupLockedItemRepo(t)
	root := cfg.ItemDir()

	snap := snapshotLockedItems(cfg, root)
	oldHead := headSHA(root)

	// Concurrent write to the locked item.
	concurrent := "id: T-001\ntype: task\nstatus: queued\ntitle: First task\nuat: pass\n"
	os.WriteFile(itemPath, []byte(concurrent), 0644)

	// Simulate the pull: a new commit that touches an unrelated file only.
	other := filepath.Join(root, "tasks", "T-999-other.md")
	os.WriteFile(other, []byte("id: T-999\ntype: task\nstatus: queued\ntitle: other\n"), 0644)
	gitRun("add", "tasks/T-999-other.md")
	gitRun("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "unrelated upstream change")
	newHead := headSHA(root)
	if newHead == oldHead {
		t.Fatal("HEAD did not move")
	}

	restoreLockedItems(root, snap, oldHead, newHead)

	got, _ := os.ReadFile(itemPath)
	if string(got) != concurrent {
		t.Errorf("restore reverted a concurrent write to a path the pull never touched (I-1722 regression):\ngot:\n%s", got)
	}
}

// TestRestoreRestoresPullChangedFile pins the mechanism's original purpose:
// when the pull DID rewrite a locked item, its pre-pull local state is put
// back.
func TestRestoreRestoresPullChangedFile(t *testing.T) {
	cfg, itemPath, gitRun := setupLockedItemRepo(t)
	root := cfg.ItemDir()

	preData, _ := os.ReadFile(itemPath)
	snap := snapshotLockedItems(cfg, root)
	oldHead := headSHA(root)

	// Simulate the pull rewriting the locked item: commit an upstream
	// version of the same file (moves HEAD and changes the path in the
	// old..new diff, and the working tree now holds upstream's content).
	upstream := "id: T-001\ntype: task\nstatus: active\ntitle: First task\nupstream: edit\n"
	os.WriteFile(itemPath, []byte(upstream), 0644)
	gitRun("add", "tasks/T-001-first-task.md")
	gitRun("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "upstream change to locked item")
	newHead := headSHA(root)

	restoreLockedItems(root, snap, oldHead, newHead)

	got, _ := os.ReadFile(itemPath)
	if string(got) != string(preData) {
		t.Errorf("pull-changed locked item was not restored to its pre-pull state:\ngot:\n%s\nwant:\n%s", got, preData)
	}
}

// TestGitPullNoopLeavesDirtyLockedItemIntact drives the real GitPull entry
// point end to end against a bare origin: with no upstream changes, a dirty
// locked item must come through untouched.
func TestGitPullNoopLeavesDirtyLockedItemIntact(t *testing.T) {
	cfg, itemPath, gitRun := setupLockedItemRepo(t)

	// Wire a bare origin so pull --ff-only has a remote to talk to.
	bare := t.TempDir()
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	gitRun("remote", "add", "origin", bare)
	gitRun("push", "-u", "origin", "HEAD")

	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}

	// The owner's in-flight write sits uncommitted in the shared tree.
	dirty := "id: T-001\ntype: task\nstatus: queued\ntitle: First task\nplan_approved: true\n"
	os.WriteFile(itemPath, []byte(dirty), 0644)

	if err := GitPull(cfg); err != nil {
		t.Fatalf("GitPull: %v", err)
	}

	got, _ := os.ReadFile(itemPath)
	if string(got) != dirty {
		t.Errorf("GitPull with no upstream changes reverted a dirty locked item (I-1722 regression):\ngot:\n%s", got)
	}
	if !strings.Contains(string(got), "plan_approved: true") {
		t.Error("plan_approved field lost")
	}
}
