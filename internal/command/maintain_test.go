package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupMaintainRepo builds a temp clone with an `origin` bare remote and an
// origin/main remote-tracking ref, so branchMerged's ancestry check is real.
func setupMaintainRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := t.TempDir()
	runGitTest(t, bare, "init", "--bare", "-b", "main")
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "t@t")
	runGitTest(t, root, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", "-A")
	runGitTest(t, root, "commit", "-m", "base")
	runGitTest(t, root, "remote", "add", "origin", bare)
	runGitTest(t, root, "push", "-u", "origin", "main")
	runGitTest(t, root, "fetch", "origin", "main") // ensure refs/remotes/origin/main
	return root
}

func commitOn(t *testing.T, root, branch, file, content string) {
	t.Helper()
	runGitTest(t, root, "checkout", "-b", branch, "main")
	if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", "-A")
	runGitTest(t, root, "commit", "-m", "work on "+branch)
	runGitTest(t, root, "checkout", "main")
}

func localBranches(t *testing.T, root string) string {
	return gitOutput(t, root, "branch", "--format=%(refname:short)")
}

func TestBranchMerged(t *testing.T) {
	root := setupMaintainRepo(t)
	// merged: a branch pointing at main's tip is an ancestor of origin/main.
	runGitTest(t, root, "branch", "merged-ff", "main")
	// unmerged: has a commit not in origin/main.
	commitOn(t, root, "feature-x", "f.txt", "x")

	if !branchMerged(root, "merged-ff", nil) {
		t.Error("merged-ff should be detected as merged (ancestor of origin/main)")
	}
	if branchMerged(root, "feature-x", nil) {
		t.Error("feature-x has an unmerged commit; should NOT be merged")
	}
	// Squash case: gh reports feature-x's CURRENT tip as a merged PR head.
	tip := strings.TrimSpace(gitOutput(t, root, "rev-parse", "feature-x"))
	if !branchMerged(root, "feature-x", map[string][]string{"feature-x": {tip}}) {
		t.Error("feature-x should be merged when its tip matches a merged PR head")
	}
}

// Regression for the name-reuse data-loss bug: a NEW branch that reuses a
// previously-merged name but carries different commits must NOT be treated as
// merged just because the name appears in gh's merged-PR list.
func TestBranchMergedRejectsReusedName(t *testing.T) {
	root := setupMaintainRepo(t)
	commitOn(t, root, "reused-name", "new.txt", "fresh unmerged work")
	tip := strings.TrimSpace(gitOutput(t, root, "rev-parse", "reused-name"))
	oldMergedOID := strings.Repeat("a", 40) // a DIFFERENT (historical) head sha
	if tip == oldMergedOID {
		t.Fatal("precondition: tips must differ")
	}
	if branchMerged(root, "reused-name", map[string][]string{"reused-name": {oldMergedOID}}) {
		t.Error("a reused name with different commits must NOT be considered merged")
	}
}

func TestPruneMergedBranches(t *testing.T) {
	root := setupMaintainRepo(t)
	runGitTest(t, root, "branch", "merged-ff", "main")
	runGitTest(t, root, "push", "origin", "merged-ff") // exists on remote too
	runGitTest(t, root, "fetch", "origin")
	commitOn(t, root, "feature-x", "f.txt", "x") // unmerged
	// on main (so the current-branch skip doesn't hide merged-ff)

	pruneMergedBranches(root, nil, MaintainOpts{DryRun: false})

	branches := localBranches(t, root)
	if strings.Contains(branches, "merged-ff") {
		t.Errorf("merged-ff should be pruned locally; branches=%q", branches)
	}
	if !strings.Contains(branches, "feature-x") {
		t.Errorf("unmerged feature-x must be kept; branches=%q", branches)
	}
	if !strings.Contains(branches, "main") {
		t.Errorf("main must never be pruned; branches=%q", branches)
	}
	// remote prune (best-effort) should have removed it from origin.
	if rem := gitOutput(t, root, "ls-remote", "origin", "merged-ff"); strings.TrimSpace(rem) != "" {
		t.Errorf("merged-ff should be deleted from origin; ls-remote=%q", rem)
	}
}

func TestPruneSkipsCurrentBranch(t *testing.T) {
	root := setupMaintainRepo(t)
	// current branch is itself merged, but we're standing on it — must be skipped
	// by pruneMergedBranches (returnToCleanMain handles the checked-out case).
	runGitTest(t, root, "checkout", "-b", "merged-current", "main")

	pruneMergedBranches(root, nil, MaintainOpts{DryRun: false})

	if !strings.Contains(localBranches(t, root), "merged-current") {
		t.Error("the current branch must never be deleted out from under us")
	}
}

func TestPruneDryRunMutatesNothing(t *testing.T) {
	root := setupMaintainRepo(t)
	runGitTest(t, root, "branch", "merged-ff", "main")

	pruneMergedBranches(root, nil, MaintainOpts{DryRun: true})

	if !strings.Contains(localBranches(t, root), "merged-ff") {
		t.Error("dry-run must not delete branches")
	}
}

func TestReturnToCleanMainChurnOnly(t *testing.T) {
	root := setupMaintainRepo(t)
	runGitTest(t, root, "checkout", "-b", "merged-current", "main") // merged (== main tip)
	// dirty the tree with ONLY churn (untracked agent-state file)
	if err := os.MkdirAll(filepath.Join(root, "agent-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent-state", "x.md"), []byte("churn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	returnToCleanMain(root, nil, MaintainOpts{DryRun: false})

	if cur := currentBranch(root); cur != "main" {
		t.Errorf("should return to main when only churn is dirty; on %q", cur)
	}
}

func TestReturnToCleanMainKeepsRealWIP(t *testing.T) {
	root := setupMaintainRepo(t)
	runGitTest(t, root, "checkout", "-b", "merged-current", "main")
	// dirty the tree with a NON-churn file — real WIP, must not be abandoned.
	if err := os.WriteFile(filepath.Join(root, "code.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	returnToCleanMain(root, nil, MaintainOpts{DryRun: false})

	if cur := currentBranch(root); cur != "merged-current" {
		t.Errorf("must stay on the branch when non-churn WIP is present; on %q", cur)
	}
}

func TestReturnToCleanMainSkipsUnmergedBranch(t *testing.T) {
	root := setupMaintainRepo(t)
	commitOn(t, root, "feature-x", "f.txt", "x")
	runGitTest(t, root, "checkout", "feature-x") // unmerged, checked out

	returnToCleanMain(root, nil, MaintainOpts{DryRun: false})

	if cur := currentBranch(root); cur != "feature-x" {
		t.Errorf("must not yank the agent off an active unmerged branch; on %q", cur)
	}
}

func TestMaintainIsChurn(t *testing.T) {
	churn := []string{"agent-state/issues/I-1.md", ".as/x.yaml", ".plans/T-1.md",
		"agent-memory/n.md", "deploy-dashboard.html", "x/deploy-dashboard-history.jsonl"}
	for _, p := range churn {
		if !maintainIsChurn(p) {
			t.Errorf("%q should be churn", p)
		}
	}
	code := []string{"scripts/agent-state-helper.sh", "internal/command/maintain.go", "README.md"}
	for _, p := range code {
		if maintainIsChurn(p) {
			t.Errorf("%q should NOT be churn", p)
		}
	}
}

func TestPorcelainPath(t *testing.T) {
	cases := map[string]string{
		" M agent-state/x.md":           "agent-state/x.md",
		"?? scripts/new.sh":             "scripts/new.sh",
		"R  old/path.go -> new/path.go": "new/path.go",
		"A  code.go":                    "code.go",
	}
	for line, want := range cases {
		if got := porcelainPath(line); got != want {
			t.Errorf("porcelainPath(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRepoSlug(t *testing.T) {
	root := setupMaintainRepo(t)
	// local bare origin → no github.com → empty slug (keeps gh out of tests)
	if slug := repoSlug(root); slug != "" {
		t.Errorf("local origin should yield empty slug, got %q", slug)
	}
}
