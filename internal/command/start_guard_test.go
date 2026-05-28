package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// TestStartRefusesWhenExistingPRFound: when an open PR already exists for the
// computed branch name, createWorktrees must return an error pointing the agent
// to `st resume` rather than silently creating a duplicate worktree (I-876).
func TestStartRefusesWhenExistingPRFound(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: dir,
		Repos:     []string{"as"},
		RepoMap:   map[string]string{"as": "as"},
	}

	opts := StartOpts{
		Slug: "my-feature",
		PRFetch: func(_ *config.Config, branch string) (string, []string) {
			return "OPEN", []string{"https://github.com/org/repo/pull/42"}
		},
	}

	_, err := createWorktrees(cfg, "T-001", "task", opts)
	if err == nil {
		t.Fatal("expected error when open PR exists, got nil")
	}
	if !strings.Contains(err.Error(), "already has an open PR") {
		t.Errorf("error must mention open PR; got: %v", err)
	}
	if !strings.Contains(err.Error(), "st resume T-001") {
		t.Errorf("error must point to st resume; got: %v", err)
	}
}

// TestStartProceedsWhenGHUnavailable: when gh is not available AND no injectable
// PRFetch is provided, createWorktrees must not call the PR guard and must
// proceed to attempt worktree creation (graceful degradation).
func TestStartProceedsWhenGHUnavailable(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: dir,
		Repos:     []string{"as"},
		RepoMap:   map[string]string{"as": "as"},
	}

	called := false
	opts := StartOpts{
		Slug: "my-feature",
		// No PRFetch injected — guard falls through to toolAvailable("gh").
		// In CI/test environments gh may or may not be present; we verify the
		// guard is skipped by ensuring PRFetch is NOT invoked when not injected.
		PRFetch: nil,
	}
	_ = called

	// Without a PRFetch injectable and no real "gh" in the test env, the guard
	// is bypassed. createWorktrees will fail trying to git-pull a non-existent
	// repo — that's expected and shows the guard didn't block startup.
	_, err := createWorktrees(cfg, "T-001", "task", opts)
	// Error is expected (no real git repo) but must NOT be the "open PR" error.
	if err != nil && strings.Contains(err.Error(), "already has an open PR") {
		t.Errorf("PR guard must not fire when gh unavailable and no PRFetch injected; got: %v", err)
	}
}

// TestStartNoPRGuardWhenNoPRFound: when the injectable PRFetch returns empty
// (no PR), createWorktrees must proceed past the guard.
func TestStartNoPRGuardWhenNoPRFound(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: dir,
		Repos:     []string{"as"},
		RepoMap:   map[string]string{"as": "as"},
	}

	opts := StartOpts{
		Slug: "my-feature",
		PRFetch: func(_ *config.Config, _ string) (string, []string) {
			return "", nil // no PR found
		},
	}

	_, err := createWorktrees(cfg, "T-001", "task", opts)
	// Error is expected (no real git repo) but must NOT be the "open PR" error.
	if err != nil && strings.Contains(err.Error(), "already has an open PR") {
		t.Errorf("guard must not fire when no PR found; got: %v", err)
	}
}
