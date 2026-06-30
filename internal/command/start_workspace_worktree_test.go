package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// seedWSClone creates a bare origin and a working clone named `name` under
// base, with one committed file pushed to origin/main. Returns the clone path.
// Mirrors the git-fixture style of TestCreateWorktrees_BasesNewBranchOnFreshOriginMain.
func seedWSClone(t *testing.T, base, name string) string {
	t.Helper()
	originDir := name + "-origin.git"
	gitBF(t, base, "init", "--bare", "-b", "main", originDir)
	origin := filepath.Join(base, originDir)
	gitBF(t, base, "clone", origin, name)
	clone := filepath.Join(base, name)
	gitBF(t, clone, "config", "user.email", "test@test.com")
	gitBF(t, clone, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(clone, "seed.txt"), []byte("seed\n"), 0644)
	gitBF(t, clone, "add", "-A")
	gitBF(t, clone, "commit", "-m", "seed")
	gitBF(t, clone, "push", "origin", "main")
	return clone
}

func wsTestConfig(t *testing.T, base string, repos []string) *config.Config {
	t.Helper()
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "wt",
		ParentDir: base,
		Repos:     repos,
		RepoMap:   map[string]string{},
	}
	if cfg.WorktreeBase() == "" {
		t.Fatal("WorktreeBase() empty; cannot place worktree")
	}
	t.Cleanup(func() { os.RemoveAll(cfg.WorktreeBase()) })
	return cfg
}

func noPRFetch() func(*config.Config, string) (string, []string) {
	return func(*config.Config, string) (string, []string) { return "", nil }
}

// I-769: when a theraprac-workspace clone exists in the repo parent, st start
// gives it a per-item worktree on the feature branch, while the primary clone
// stays on main as the agent-state store.
func TestStartWorkspaceWorktree_CreatedWhenCloneExists(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	seedWSClone(t, base, "repo")
	wsClone := seedWSClone(t, base, workspaceRepo)

	cfg := wsTestConfig(t, base, []string{"repo"})
	opts := StartOpts{Slug: "ws", Repos: []string{"repo", workspaceRepo}, PRFetch: noPRFetch()}
	if _, err := createWorktrees(cfg, "I-9001", "issue", opts); err != nil {
		t.Fatalf("createWorktrees: %v", err)
	}

	wtWS := filepath.Join(cfg.WorktreeBase(), "I-9001", workspaceRepo)
	if _, err := os.Stat(wtWS); err != nil {
		t.Fatalf("expected workspace worktree at %s: %v", wtWS, err)
	}
	if got := gitBF(t, wtWS, "rev-parse", "--abbrev-ref", "HEAD"); got != "fix/I-9001-ws" {
		t.Errorf("workspace worktree branch = %q, want fix/I-9001-ws", got)
	}
	// Primary clone must stay on main (it is the agent-state store).
	if got := gitBF(t, wsClone, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("primary workspace clone branch = %q, want main", got)
	}
}

// Setups without a theraprac-workspace clone must be unaffected — no workspace
// worktree is created and the explicit repos are still provisioned.
func TestStartWorkspaceWorktree_SkippedWhenAbsent(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	seedWSClone(t, base, "repo")

	cfg := wsTestConfig(t, base, []string{"repo"})
	opts := StartOpts{Slug: "noworkspace", Repos: []string{"repo"}, PRFetch: noPRFetch()}
	if _, err := createWorktrees(cfg, "I-9002", "issue", opts); err != nil {
		t.Fatalf("createWorktrees: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfg.WorktreeBase(), "I-9002", workspaceRepo)); !os.IsNotExist(err) {
		t.Errorf("workspace worktree should not exist when no clone is present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorktreeBase(), "I-9002", "repo")); err != nil {
		t.Errorf("expected repo worktree to still be provisioned: %v", err)
	}
}

// The ff-only merge is skipped for the workspace clone, so a dirty/auto-committed
// agent-state working tree neither blocks worktree creation nor gets disturbed,
// and the primary clone's main pointer is not advanced. The worktree is still
// based on fresh origin/main via the fetch path.
func TestStartWorkspaceWorktree_ProvisionsWithDirtyPrimary(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	seedWSClone(t, base, "repo")
	wsClone := seedWSClone(t, base, workspaceRepo)

	staleLocal := gitBF(t, wsClone, "rev-parse", "main")

	// Advance origin/main ahead of the primary clone's local main via a second
	// clone, so an ff-only (if it ran) would move main forward.
	wsOrigin := filepath.Join(base, workspaceRepo+"-origin.git")
	gitBF(t, base, "clone", wsOrigin, "ws-seed")
	wsSeed := filepath.Join(base, "ws-seed")
	gitBF(t, wsSeed, "config", "user.email", "test@test.com")
	gitBF(t, wsSeed, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(wsSeed, "seed.txt"), []byte("advanced\n"), 0644)
	gitBF(t, wsSeed, "add", "-A")
	gitBF(t, wsSeed, "commit", "-m", "advance origin")
	gitBF(t, wsSeed, "push", "origin", "main")
	want := gitBF(t, wsSeed, "rev-parse", "HEAD")
	if want == staleLocal {
		t.Fatalf("precondition: origin main %s should differ from stale local %s", want, staleLocal)
	}

	// Make the primary clone's working tree dirty (simulating agent-state churn).
	dirty := filepath.Join(wsClone, "agent-state-churn.txt")
	os.WriteFile(dirty, []byte("uncommitted\n"), 0644)

	cfg := wsTestConfig(t, base, []string{"repo"})
	opts := StartOpts{Slug: "dirty", Repos: []string{"repo", workspaceRepo}, PRFetch: noPRFetch()}
	if _, err := createWorktrees(cfg, "I-9003", "issue", opts); err != nil {
		t.Fatalf("createWorktrees with dirty primary: %v", err)
	}

	// ff-only skipped: primary clone main pointer NOT advanced, still on main.
	if got := gitBF(t, wsClone, "rev-parse", "main"); got != staleLocal {
		t.Errorf("primary main = %s, want unchanged %s (ff-only should be skipped)", got, staleLocal)
	}
	if got := gitBF(t, wsClone, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("primary clone branch = %q, want main", got)
	}
	// Dirty working-tree file untouched.
	if b, err := os.ReadFile(dirty); err != nil || string(b) != "uncommitted\n" {
		t.Errorf("dirty file disturbed: content=%q err=%v", string(b), err)
	}
	// Worktree still based on fresh origin/main.
	wtWS := filepath.Join(cfg.WorktreeBase(), "I-9003", workspaceRepo)
	if got := gitBF(t, wtWS, "rev-parse", "HEAD"); got != want {
		t.Errorf("workspace worktree HEAD = %s, want fresh origin/main %s", got, want)
	}
}

// When theraprac-workspace is already in the repos list, it is provisioned once,
// not appended a second time.
func TestStartWorkspaceWorktree_NoDuplicateWhenAlreadyListed(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	seedWSClone(t, base, workspaceRepo)

	cfg := wsTestConfig(t, base, []string{workspaceRepo})
	opts := StartOpts{Slug: "listed", Repos: []string{workspaceRepo}, PRFetch: noPRFetch()}
	if _, err := createWorktrees(cfg, "I-9004", "issue", opts); err != nil {
		t.Fatalf("createWorktrees: %v", err)
	}

	workinfo, err := os.ReadFile(filepath.Join(cfg.WorktreeBase(), "I-9004", ".workinfo"))
	if err != nil {
		t.Fatalf("read .workinfo: %v", err)
	}
	if n := strings.Count(string(workinfo), "- "+workspaceRepo+"\n"); n != 1 {
		t.Errorf("theraprac-workspace listed %d times in .workinfo, want 1\n%s", n, workinfo)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}
