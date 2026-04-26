package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

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
