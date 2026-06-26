package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// gitBF runs git in dir with a deterministic identity and returns trimmed
// stdout. Fatals on error. Local to the base-freshness test (I-1299).
func gitBF(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
		"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// I-1299: st start used to branch a new per-item worktree from local main,
// which is frequently behind origin/main, manufacturing phantom e2e /
// openapi-drift failures. createWorktrees must fetch origin and base the new
// branch on FRESH origin/main even when local main is stale.
func TestCreateWorktrees_BasesNewBranchOnFreshOriginMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()

	// Bare origin.
	gitBF(t, base, "init", "--bare", "-b", "main", "origin.git")
	origin := filepath.Join(base, "origin.git")

	// Seed clone: commit1 → push to origin/main.
	gitBF(t, base, "clone", origin, "seed")
	seed := filepath.Join(base, "seed")
	os.WriteFile(filepath.Join(seed, "f.txt"), []byte("one\n"), 0644)
	gitBF(t, seed, "add", "-A")
	gitBF(t, seed, "commit", "-m", "commit1")
	gitBF(t, seed, "push", "origin", "main")

	// The repo st operates on. Its local main now points at commit1.
	gitBF(t, base, "clone", origin, "repo")
	localClone := filepath.Join(base, "repo")
	gitBF(t, localClone, "config", "user.email", "test@test.com")
	gitBF(t, localClone, "config", "user.name", "Test")

	// Give localClone a DIVERGENT local commit on main so `pull --ff-only`
	// cannot fast-forward and gets skipped (the real-world failure mode the
	// old code hit). Without the fetch+origin/main fix the worktree would be
	// based on this divergent local HEAD.
	os.WriteFile(filepath.Join(localClone, "local.txt"), []byte("divergent\n"), 0644)
	gitBF(t, localClone, "add", "-A")
	gitBF(t, localClone, "commit", "-m", "divergent local commit")
	divergentLocal := gitBF(t, localClone, "rev-parse", "main")

	// Advance origin/main to commit2 via seed WITHOUT touching localClone,
	// so localClone's local main is both divergent AND stale.
	os.WriteFile(filepath.Join(seed, "f.txt"), []byte("two\n"), 0644)
	gitBF(t, seed, "add", "-A")
	gitBF(t, seed, "commit", "-m", "commit2")
	gitBF(t, seed, "push", "origin", "main")
	want := gitBF(t, seed, "rev-parse", "HEAD")

	if divergentLocal == want {
		t.Fatalf("precondition failed: local main %s should differ from origin commit2 %s", divergentLocal, want)
	}

	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "wt",
		ParentDir: base,
		Repos:     []string{"repo"},
		RepoMap:   map[string]string{"repo": "repo"},
	}
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase() empty; cannot place worktree")
	}
	t.Cleanup(func() { os.RemoveAll(wtBase) })

	opts := StartOpts{
		Slug:    "freshbase",
		Repos:   []string{"repo"},
		PRFetch: func(*config.Config, string) (string, []string) { return "", nil },
	}
	if _, err := createWorktrees(cfg, "I-9999", "issue", opts); err != nil {
		t.Fatalf("createWorktrees: %v", err)
	}

	wtRepo := filepath.Join(wtBase, "I-9999", "repo")
	gotHEAD := gitBF(t, wtRepo, "rev-parse", "HEAD")
	if gotHEAD == divergentLocal {
		t.Errorf("worktree HEAD = %s == divergent local main; worktree was based on stale local main", gotHEAD)
	}
	if gotHEAD != want {
		t.Errorf("worktree HEAD = %s, want fresh origin/main %s (branched from stale local main)", gotHEAD, want)
	}

	// AC: merge-base(worktree HEAD, origin/main) == origin/main.
	originMain := gitBF(t, localClone, "rev-parse", "origin/main")
	if originMain != want {
		t.Fatalf("fetch did not advance localClone origin/main: %s != %s", originMain, want)
	}
	mb := gitBF(t, wtRepo, "merge-base", "HEAD", want)
	if mb != originMain {
		t.Errorf("merge-base(HEAD, origin/main) = %s, want origin/main %s", mb, originMain)
	}
}
