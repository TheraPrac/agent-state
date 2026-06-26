package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// TestTryAutoFinishWorktreeReadsWorkinfo — extra repos registered via
// `st worktree add` live in .workinfo but NOT in cfg.Worktree.Repos.
// TryAutoFinishWorktree must read .workinfo so those repos are included
// in the safety pre-check and are not silently deleted by os.RemoveAll.
func TestTryAutoFinishWorktreeReadsWorkinfo(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"}, // repo-b is NOT in global config
	}

	// Worktree dir under legacy path (inside temp dir).
	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	// Create repo-b — an extra repo added via st worktree add, present in
	// .workinfo but absent from cfg.Worktree.Repos.
	repoB := filepath.Join(wtDir, "repo-b")
	if err := os.MkdirAll(repoB, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Write .workinfo listing both repo-a and repo-b.
	workinfo := "name: T-001\nbranch: fix/T-001\nrepos:\n- repo-a\n- repo-b\n"
	if err := os.WriteFile(filepath.Join(wtDir, ".workinfo"), []byte(workinfo), 0o644); err != nil {
		t.Fatalf("WriteFile .workinfo: %v", err)
	}

	// TryAutoFinishWorktree must discover repo-b via .workinfo and include
	// it in the safety check. Because repo-b is not a real git checkout,
	// git worktree remove fails → retained=true (not cleaned).
	// Before the fix, TryAutoFinishWorktree would skip repo-b entirely
	// and call os.RemoveAll, silently destroying its contents.
	cleaned, _ := TryAutoFinishWorktree(cfg, "T-001")
	if cleaned {
		t.Error("workinfo fix: got cleaned=true — extra repo-b was not checked and wtDir was removed; want retained")
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("workinfo fix: wtDir was removed despite repo-b having uncommitted work (not a real git checkout)")
	}
}

// TestTryAutoFinishWorktreeDisabled — when worktree integration is off,
// the helper must be a silent no-op. Returning anything truthy here
// would cause Close() to print spurious "also finished worktree" lines
// for repos that don't even use worktrees.
func TestTryAutoFinishWorktreeDisabled(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// cfg.Worktree is nil by default in setupTestEnv
	cleaned, retained := TryAutoFinishWorktree(cfg, "T-001")
	if cleaned || retained {
		t.Errorf("disabled config: got cleaned=%v retained=%v, want both false", cleaned, retained)
	}
}

// TestTryAutoFinishWorktreeNoDir — when worktree config is enabled but
// the item never had a worktree (most issues, items closed before this
// hook existed), the helper must skip silently. This is the common case
// once the hook is shipped and old items get closed.
func TestTryAutoFinishWorktreeNoDir(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}
	cleaned, retained := TryAutoFinishWorktree(cfg, "T-999")
	if cleaned || retained {
		t.Errorf("missing wtDir: got cleaned=%v retained=%v, want both false", cleaned, retained)
	}
}

// TestTryAutoFinishWorktreeRetainsWhenCleanupFails — wtDir exists but
// the per-repo dir is not a real git checkout (no .git, no main repo
// resolvable). The auto path must NOT silently swallow this — operator
// needs the retention warning so they can run `st finish --force`.
func TestTryAutoFinishWorktreeRetainsWhenCleanupFails(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	repoDir := filepath.Join(wtDir, "repo-a")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	cleaned, _ := TryAutoFinishWorktree(cfg, "T-001")
	if cleaned {
		t.Errorf("non-git cleanup path: got cleaned=true, want false")
	}
	// wtDir should still exist on the retention path so the operator
	// can run `st finish --force` against it.
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("retention path removed wtDir; should be preserved for force-finish")
	}
}

// TestTryAutoFinishWorktreeRetainsWhenNoUpstream — a real git repo with local
// commits but no upstream tracking branch configured. Before the I-1469 fix,
// `git log @{u}..HEAD` failed (err != nil) and the guard was bypassed, so the
// worktree was removed despite having local-only work. After the fix, a failing
// @{u} query retains conservatively.
func TestTryAutoFinishWorktreeRetainsWhenNoUpstream(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	repoDir := filepath.Join(wtDir, "repo-a")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Init a real git repo with a commit and NO upstream tracking branch.
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "Test"},
	} {
		if out, err := runGit(repoDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "work.txt"), []byte("local work"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, args := range [][]string{
		{"add", "work.txt"},
		{"commit", "-m", "local work not on any remote"},
	} {
		if out, err := runGit(repoDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Branch 'main' has no upstream — `git log @{u}..HEAD` will fail.

	cleaned, retained := TryAutoFinishWorktree(cfg, "T-001")
	if cleaned {
		t.Error("no-upstream: got cleaned=true, want false — worktree with no upstream must be retained to prevent data loss")
	}
	if !retained {
		t.Error("no-upstream: got retained=false, want true")
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("no-upstream: wtDir was removed; must be preserved for operator to run `st finish --force`")
	}
}
