package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRepoDirForItemDoesNotCrossItemWorktrees is a regression test for
// I-986: Pattern 3 must not return a sibling item's worktree when the target
// item has no worktree of its own.
func TestResolveRepoDirForItemDoesNotCrossItemWorktrees(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase empty — worktree integration not enabled")
	}

	// Seed a sibling item's as/ checkout (the interfering directory).
	sibling := filepath.Join(wtBase, "I-985", "as")
	if err := os.MkdirAll(filepath.Join(sibling, ".git"), 0755); err != nil {
		t.Fatalf("mkdir sibling worktree: %v", err)
	}

	// Target item I-986 has no worktree on disk.
	got := resolveRepoDirForItem(cfg, "I-986", "as")
	if strings.Contains(got, "I-985") {
		t.Errorf("Pattern 3 leaked into sibling worktree: got %q (must not reference I-985)", got)
	}
}

// TestResolveItemWorktreePrefersOwnWorktree verifies Pattern 1 returns the
// target item's own worktree even when a sibling also has an as/ checkout.
func TestResolveItemWorktreePrefersOwnWorktree(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase empty — worktree integration not enabled")
	}

	for _, id := range []string{"I-985", "I-986"} {
		dir := filepath.Join(wtBase, id, "as")
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
			t.Fatalf("mkdir worktree %s: %v", id, err)
		}
	}

	want := filepath.Join(wtBase, "I-986", "as")
	if got := resolveRepoDirForItem(cfg, "I-986", "as"); got != want {
		t.Errorf("expected I-986's own worktree %q, got %q", want, got)
	}
}

// TestResolveItemWorktreeExactRepoMatch verifies Pattern 3 does not match a
// directory whose name merely contains the repo string ("theraprac-as" ≠ "as").
func TestResolveItemWorktreeExactRepoMatch(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase empty — worktree integration not enabled")
	}

	// "theraprac-as" contains the string "as" but is not an exact match.
	dir := filepath.Join(wtBase, "I-985", "theraprac-as")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir theraprac-as: %v", err)
	}

	if got := resolveItemWorktree(cfg, "I-986", "as"); got != "" {
		t.Errorf("substring-only match should not resolve: got %q", got)
	}
}
