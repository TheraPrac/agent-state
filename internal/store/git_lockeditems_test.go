package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// I-1728: the locked-item snapshot/restore mechanism around GitPull/GitSync/
// RefreshWorkspace (I-1722's scoped version of it) has been removed
// entirely. Its only surviving fire-path after I-1722 was harmful: a locked
// item that was CLEAN locally (no in-flight uncommitted state to protect —
// the dirty case is already covered by git pull --ff-only's own refusal)
// had a legitimate upstream update silently reverted back to its pre-pull
// content, and the owner's next GitSync then committed and pushed that
// revert over the peer's write, with no conflict or error surfaced.
//
// These tests pin the corrected contract end to end: a peer's pushed change
// to a clean locked item survives the owner's pull (and their next sync's
// push), and a dirty locked item is left untouched because --ff-only
// refuses to advance rather than merge.

// setupLockedItemRepo builds a git repo with the standard layout, one task
// item, and a pipeline lock (.locks/<id>) on it, returning the cfg and the
// item's absolute path.
func setupLockedItemRepo(t *testing.T) (*config.Config, string) {
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
	return cfg, itemPath
}

// pushUpstreamChange clones bare into a scratch dir standing in for a peer
// agent, rewrites the locked item's file there, and pushes the commit to
// origin — simulating a peer's legitimate GitSync landing on main while the
// owner still holds the item lock locally with a clean working tree.
func pushUpstreamChange(t *testing.T, bare, content string) {
	t.Helper()
	peer := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = peer
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cloneCmd := exec.Command("git", "clone", bare, ".")
	cloneCmd.Dir = peer
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(peer, "tasks", "T-001-first-task.md"), []byte(content), 0644); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	run("add", "tasks/T-001-first-task.md")
	run("-c", "user.email=peer@t", "-c", "user.name=peer", "commit", "-m", "peer: upstream change to locked item")
	run("push", "origin", "HEAD:main")
}

// TestGitPullTakesUpstreamChangeToCleanLockedItem is the core I-1728 fix:
// a peer's legitimate upstream change to an item the owner has locked but
// not touched must come through the pull intact, not get reverted back to
// the pre-pull snapshot.
func TestGitPullTakesUpstreamChangeToCleanLockedItem(t *testing.T) {
	cfg, itemPath := setupLockedItemRepo(t)
	root := cfg.ItemDir()
	bare := gitBareRemote(t, root)
	gitRun(t, root, "branch", "--set-upstream-to=origin/main", "main")
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}

	upstream := "id: T-001\ntype: task\nstatus: active\ntitle: First task\nupstream: edit\n"
	pushUpstreamChange(t, bare, upstream)

	if err := GitPull(cfg); err != nil {
		t.Fatalf("GitPull: %v", err)
	}

	got, _ := os.ReadFile(itemPath)
	if string(got) != upstream {
		t.Errorf("GitPull did not take the peer's upstream change to a clean locked item (I-1728 regression):\ngot:\n%s\nwant:\n%s", got, upstream)
	}
}

// TestGitPullLeavesDirtyLockedItemIntactWhenUpstreamChanges confirms the
// case the mechanism's removal still relies on: a locked item with genuine
// uncommitted local state is protected by --ff-only's own refusal to
// advance, not by any snapshot/restore step.
func TestGitPullLeavesDirtyLockedItemIntactWhenUpstreamChanges(t *testing.T) {
	cfg, itemPath := setupLockedItemRepo(t)
	root := cfg.ItemDir()
	bare := gitBareRemote(t, root)
	gitRun(t, root, "branch", "--set-upstream-to=origin/main", "main")
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}

	upstream := "id: T-001\ntype: task\nstatus: active\ntitle: First task\nupstream: edit\n"
	pushUpstreamChange(t, bare, upstream)

	dirty := "id: T-001\ntype: task\nstatus: queued\ntitle: First task\nplan_approved: true\n"
	if err := os.WriteFile(itemPath, []byte(dirty), 0644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	if err := GitPull(cfg); err != nil {
		t.Fatalf("GitPull: %v", err)
	}

	got, _ := os.ReadFile(itemPath)
	if string(got) != dirty {
		t.Errorf("GitPull altered a dirty locked item instead of refusing the non-ff-only merge:\ngot:\n%s\nwant:\n%s", got, dirty)
	}
}

// TestGitSyncDoesNotRevertPeersUpstreamChangeToLockedItem reproduces the
// I-1728 incident shape end to end through the real GitSync entry point:
// the owner has an unrelated change of their own to sync (so GitSync
// actually commits and pushes), while a peer has already pushed a change to
// the owner's locked-but-clean item. The owner's sync must not push a
// revert of the peer's write.
func TestGitSyncDoesNotRevertPeersUpstreamChangeToLockedItem(t *testing.T) {
	cfg, itemPath := setupLockedItemRepo(t)
	root := cfg.ItemDir()
	bare := gitBareRemote(t, root)
	gitRun(t, root, "branch", "--set-upstream-to=origin/main", "main")
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	upstream := "id: T-001\ntype: task\nstatus: active\ntitle: First task\nupstream: edit\n"
	pushUpstreamChange(t, bare, upstream)

	// Owner's own unrelated change, so GitSync has something to commit and
	// push (and therefore actually runs its pre-pull step).
	other := filepath.Join(root, "tasks", "T-999-other.md")
	if err := os.WriteFile(other, []byte("id: T-999\ntype: task\nstatus: queued\ntitle: other\n"), 0644); err != nil {
		t.Fatalf("write other item: %v", err)
	}

	if err := s.GitSync("owner change", other); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	got, _ := os.ReadFile(itemPath)
	if string(got) != upstream {
		t.Errorf("GitSync reverted the peer's upstream change to a clean locked item (I-1728 regression):\ngot:\n%s\nwant:\n%s", got, upstream)
	}

	// Confirm the remote itself reflects the peer's content — proving the
	// owner's sync didn't commit-and-push a revert on top of it.
	clone := t.TempDir()
	cloneCmd := exec.Command("git", "clone", bare, ".")
	cloneCmd.Dir = clone
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	remoteData, err := os.ReadFile(filepath.Join(clone, "tasks", "T-001-first-task.md"))
	if err != nil {
		t.Fatalf("read remote item: %v", err)
	}
	if string(remoteData) != upstream {
		t.Errorf("remote T-001 content was reverted by the owner's sync push (I-1728 regression):\ngot:\n%s\nwant:\n%s", remoteData, upstream)
	}
}
