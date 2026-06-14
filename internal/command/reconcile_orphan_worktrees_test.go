package command

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// legacyWorktreeDir returns the legacy (pre-I-407) worktree path for an item —
// <cfg.Root()>/worktrees/<id>. In tests, the temp dir IS cfg.Root(), so this
// is always inside the test's isolated temp tree and safe to create.
func legacyWorktreeDir(cfg *config.Config, id string) string {
	return filepath.Join(cfg.Root(), "worktrees", id)
}

// TestReconcileOrphanWorktrees_Disabled — when worktree integration is not
// enabled the phase must be a silent no-op.
func TestReconcileOrphanWorktrees_Disabled(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// cfg.Worktree is nil from setupTestEnv
	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 0 {
		t.Errorf("disabled: got %d, want 0", n)
	}
}

// TestReconcileOrphanWorktrees_NoBaseDirIsNoOp — worktree enabled but base
// dir doesn't exist yet → silently returns 0.
func TestReconcileOrphanWorktrees_NoBaseDirIsNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}
	// Neither WorktreeBase() nor WorktreeBaseLegacy() dirs exist
	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 0 {
		t.Errorf("no base dir: got %d, want 0", n)
	}
}

// TestReconcileOrphanWorktrees_TerminalItemClean — a terminal item's worktree
// dir with no repo subdirs is auto-pruned and counted.
func TestReconcileOrphanWorktrees_TerminalItemClean(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	// T-004 is done (terminal) in the test fixture archive/.
	wtDir := legacyWorktreeDir(cfg, "T-004")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 1 {
		t.Errorf("clean terminal worktree: got %d, want 1", n)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("wtDir should have been removed; still exists")
	}
}

// TestReconcileOrphanWorktrees_ActiveItemSkipped — an active item's worktree
// dir is left alone.
func TestReconcileOrphanWorktrees_ActiveItemSkipped(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	// T-003 is active in the test fixture.
	wtDir := legacyWorktreeDir(cfg, "T-003")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 0 {
		t.Errorf("active item: got %d, want 0", n)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("active item's wtDir was incorrectly removed")
	}
}

// TestReconcileOrphanWorktrees_UnknownDirSkipped — a worktree dir whose name
// doesn't match any tracked item ID is silently skipped.
func TestReconcileOrphanWorktrees_UnknownDirSkipped(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	unknown := legacyWorktreeDir(cfg, "X-999")
	if err := os.MkdirAll(unknown, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 0 {
		t.Errorf("unknown dir: got %d, want 0", n)
	}
	if _, err := os.Stat(unknown); os.IsNotExist(err) {
		t.Error("unknown dir was incorrectly removed")
	}
}

// TestReconcileOrphanWorktrees_DryRun — dry-run reports the orphan but does
// not remove the directory.
func TestReconcileOrphanWorktrees_DryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	wtDir := legacyWorktreeDir(cfg, "T-004")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Capture stdout to verify dry-run message.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{DryRun: true})

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r) //nolint:errcheck
	out := buf.String()

	if n != 1 {
		t.Errorf("dry-run: got %d, want 1", n)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("dry-run: expected '[dry-run]' in output, got: %q", out)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("dry-run: wtDir should NOT be removed")
	}
}

// TestReconcileOrphanWorktrees_RetainedWarning — when a terminal item's
// worktree contains a repo subdir that blocks auto-cleanup (not a real git
// checkout so git worktree remove fails), a warning is printed and no count
// increment happens.
func TestReconcileOrphanWorktrees_RetainedWarning(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	wtDir := legacyWorktreeDir(cfg, "T-004")
	repoDir := filepath.Join(wtDir, "repo-a")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	n := reconcileOrphanWorktrees(s, cfg, ReconcileOpts{})
	if n != 0 {
		t.Errorf("retained: got %d, want 0", n)
	}
	// wtDir must be preserved so the operator can force-clean manually.
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Error("retained: wtDir was incorrectly removed")
	}
}
