package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// initBareGitRepo creates a git repo at dir with a single empty commit.
func initBareGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "commit", "--allow-empty", "-m", "init")
}

// cloneGitRepo clones src into dst so both start at the same HEAD SHA.
func cloneGitRepo(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("git", "clone", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone %s → %s: %v\n%s", src, dst, err, out)
	}
}

// addExtraCommit advances the repo's HEAD so its SHA differs from the source.
func addExtraCommit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "extra")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:01:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:01:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extra commit in %s: %v\n%s", dir, err, out)
	}
}

// ---- worktreeRepoOnSameCommit unit tests ----------------------------------

func TestWorktreeRepoOnSameCommit_Equal(t *testing.T) {
	tmp := t.TempDir()
	repoA := filepath.Join(tmp, "repo-a")
	repoB := filepath.Join(tmp, "repo-b")
	initBareGitRepo(t, repoA)
	cloneGitRepo(t, repoA, repoB)

	if !worktreeRepoOnSameCommit(repoA, repoB) {
		shaA, _ := runGit(repoA, "rev-parse", "HEAD")
		shaB, _ := runGit(repoB, "rev-parse", "HEAD")
		t.Errorf("expected same commit; repoA=%q repoB=%q",
			strings.TrimSpace(shaA), strings.TrimSpace(shaB))
	}
}

func TestWorktreeRepoOnSameCommit_Differ(t *testing.T) {
	tmp := t.TempDir()
	repoA := filepath.Join(tmp, "repo-a")
	repoB := filepath.Join(tmp, "repo-b")
	initBareGitRepo(t, repoA)
	cloneGitRepo(t, repoA, repoB)
	addExtraCommit(t, repoB)

	if worktreeRepoOnSameCommit(repoA, repoB) {
		t.Error("expected different commits, got same=true")
	}
}

func TestWorktreeRepoOnSameCommit_MissingDir(t *testing.T) {
	if worktreeRepoOnSameCommit("/nonexistent/a", "/nonexistent/b") {
		t.Error("missing dirs: expected false, got true")
	}
}

// ---- cloneFreshness / stale-clone guard (I-1588) -------------------------

func TestCloneFreshness_Current(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)

	head, behind, ok := cloneFreshness(clone)
	if !ok {
		t.Fatal("ok=false, want true for a healthy clone")
	}
	if head == "" {
		t.Error("head is empty")
	}
	if behind != "0" {
		t.Errorf("behind=%q, want %q (clone is current with origin)", behind, "0")
	}
}

func TestCloneFreshness_Behind(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	// Advance origin/main after the clone; cloneFreshness must fetch the ref
	// and report the clone as 1 commit behind (the I-1588 stale case).
	addExtraCommit(t, origin)

	head, behind, ok := cloneFreshness(clone)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if head == "" {
		t.Error("head is empty")
	}
	if behind != "1" {
		t.Errorf("behind=%q, want %q after origin advanced one commit", behind, "1")
	}
}

func TestCloneFreshness_NonRepo(t *testing.T) {
	if _, _, ok := cloneFreshness(filepath.Join(t.TempDir(), "not-a-repo")); ok {
		t.Error("ok=true for a non-git path, want false")
	}
	// warnIfCloneStale must not panic on the same degraded input.
	warnIfCloneStale("as", filepath.Join(t.TempDir(), "not-a-repo"))
}

// ---- rewriteSuiteForWorktree warm-checkout integration -------------------

// setupWarmCheckoutCfg returns a config with worktree enabled, parent_dir set
// to parentDir, and BaseDir pointing at a subdir of agentRoot so
// WorktreeBase() resolves correctly. Caller provides the absolute paths.
func setupWarmCheckoutCfg(t *testing.T, agentRoot, parentDir string, repos []string) *config.Config {
	t.Helper()
	// Create a minimal workspace under agentRoot so setupTestEnv-like config
	// loading can succeed. We build a Config directly via config.Load using
	// a temp workspace dir inside agentRoot.
	wsDir := filepath.Join(agentRoot, "ws")
	for _, d := range []string{filepath.Join(wsDir, "tasks"), filepath.Join(wsDir, "issues"),
		filepath.Join(wsDir, "archive"), filepath.Join(wsDir, ".as")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(wsDir, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0o644)
	os.WriteFile(filepath.Join(agentRoot, ".as", "agent-workspace.yaml"),
		[]byte("path: "+wsDir+"\nagent_id: agent-test\n"), 0o644)

	cfg, err := config.Load(wsDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.ResetAgentRootCache()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: parentDir,
		Repos:     repos,
	}
	return cfg
}

// TestRewriteSuiteForWorktree_UsesMainWhenSameCommit — when the worktree
// repo and main checkout are on the same commit, the suite is rewritten to
// use the main checkout path (warm cache). I-998.
func TestRewriteSuiteForWorktree_UsesMainWhenSameCommit(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := tmp
	parentDir := filepath.Join(tmp, "repos")

	// Main checkout: parentDir/theraprac-api
	mainRepo := filepath.Join(parentDir, "theraprac-api")
	initBareGitRepo(t, mainRepo)

	cfg := setupWarmCheckoutCfg(t, agentRoot, parentDir, []string{"theraprac-api"})

	// Worktree repo: WorktreeBase()/T-001/theraprac-api (cloned from main — same SHA)
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase() returned empty — worktree config not applied correctly")
	}
	wtRepo := filepath.Join(wtBase, "T-001", "theraprac-api")
	cloneGitRepo(t, mainRepo, wtRepo)

	suiteCmd := "cd ../theraprac-api && make test-unit"
	rewritten := rewriteSuiteForWorktree(cfg, "T-001", suiteCmd)

	// Must use the main checkout, not the worktree.
	if strings.Contains(rewritten, "worktrees") {
		t.Errorf("expected main checkout path, got worktree path: %s", rewritten)
	}
	if !strings.Contains(rewritten, mainRepo) {
		t.Errorf("expected main repo path %q in rewritten cmd: %s", mainRepo, rewritten)
	}
}

// TestRewriteSuiteForWorktree_UsesWorktreeWhenDifferentCommit — when the
// worktree has diverged from main (item committed changes), the suite must
// still target the worktree. I-998 must not regress the I-400 behavior.
func TestRewriteSuiteForWorktree_UsesWorktreeWhenDifferentCommit(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := tmp
	parentDir := filepath.Join(tmp, "repos")

	mainRepo := filepath.Join(parentDir, "theraprac-api")
	initBareGitRepo(t, mainRepo)

	cfg := setupWarmCheckoutCfg(t, agentRoot, parentDir, []string{"theraprac-api"})

	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase() returned empty")
	}
	wtRepo := filepath.Join(wtBase, "T-001", "theraprac-api")
	cloneGitRepo(t, mainRepo, wtRepo)
	// Advance the worktree so its SHA differs from main.
	addExtraCommit(t, wtRepo)

	suiteCmd := "cd ../theraprac-api && make test-unit"
	rewritten := rewriteSuiteForWorktree(cfg, "T-001", suiteCmd)

	// Must use the worktree path when commits differ.
	if !strings.Contains(rewritten, wtRepo) {
		t.Errorf("expected worktree path %q in rewritten cmd: %s", wtRepo, rewritten)
	}
	if strings.Contains(rewritten, mainRepo) {
		t.Errorf("should not use main checkout when worktree differs: %s", rewritten)
	}
}
