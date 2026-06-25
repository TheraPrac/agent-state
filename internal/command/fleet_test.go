package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCheckoutBranch makes dir's working tree a feature branch (off main).
func gitCheckoutBranch(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -b %s in %s: %v\n%s", branch, dir, err, out)
	}
}

func TestAgentDirsUnder(t *testing.T) {
	parent := t.TempDir()
	for _, n := range []string{"theraprac-agent-a", "theraprac-agent-c", "theraprac-agent-b", "not-an-agent", "theraprac-workspace"} {
		if err := os.MkdirAll(filepath.Join(parent, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file matching the prefix must be ignored (only dirs count).
	os.WriteFile(filepath.Join(parent, "theraprac-agent-file"), []byte("x"), 0o644)

	got, err := agentDirsUnder(parent)
	if err != nil {
		t.Fatalf("agentDirsUnder: %v", err)
	}
	want := []string{
		filepath.Join(parent, "theraprac-agent-a"),
		filepath.Join(parent, "theraprac-agent-b"),
		filepath.Join(parent, "theraprac-agent-c"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestClassifyClone_Current(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	if s.missing || !s.onMain || s.dirty {
		t.Fatalf("unexpected state: %+v", s)
	}
	if s.behind != 0 {
		t.Errorf("behind=%d, want 0", s.behind)
	}
}

func TestClassifyClone_Behind(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	addExtraCommit(t, origin) // origin advances; clone now behind by 1
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	if !s.onMain || s.dirty || s.missing {
		t.Fatalf("unexpected state: %+v", s)
	}
	if s.behind != 1 {
		t.Errorf("behind=%d, want 1", s.behind)
	}
}

func TestClassifyClone_NotOnMain(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	gitCheckoutBranch(t, clone, "fix/wip")
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	if s.onMain {
		t.Errorf("onMain=true, want false for feature branch")
	}
	if s.branch != "fix/wip" {
		t.Errorf("branch=%q, want fix/wip", s.branch)
	}
}

func TestClassifyClone_Dirty(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	os.WriteFile(filepath.Join(clone, "dirty.txt"), []byte("x"), 0o644)
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	if !s.dirty {
		t.Error("dirty=false, want true (untracked file present)")
	}
}

func TestClassifyClone_Missing(t *testing.T) {
	s := classifyClone(filepath.Join(t.TempDir(), "nope"), "as", "", "")
	if !s.missing {
		t.Error("missing=false, want true for a non-existent clone")
	}
}

func TestSyncClone_FastForwards(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	addExtraCommit(t, origin)
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	action, _, advanced := syncClone(clone, s)
	if action != "ff'd" || !advanced {
		t.Fatalf("action=%q advanced=%v, want ff'd/true", action, advanced)
	}
	// Clone HEAD must now equal origin tip.
	if got := trim(fleetGit(t, clone, "rev-parse", "HEAD")); got != authFull {
		t.Errorf("clone HEAD=%s, want %s after ff", got, authFull)
	}
}

func TestSyncClone_SkipsDirty(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	addExtraCommit(t, origin)
	os.WriteFile(filepath.Join(clone, "dirty.txt"), []byte("x"), 0o644)
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))
	before := trim(fleetGit(t, clone, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	action, _, advanced := syncClone(clone, s)
	if action != "skip" || advanced {
		t.Errorf("action=%q advanced=%v, want skip/false for dirty clone", action, advanced)
	}
	if after := trim(fleetGit(t, clone, "rev-parse", "HEAD")); after != before {
		t.Errorf("dirty clone HEAD moved: %s → %s", before, after)
	}
}

func TestSyncClone_SkipsOffMain(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	clone := filepath.Join(tmp, "clone")
	initBareGitRepo(t, origin)
	cloneGitRepo(t, origin, clone)
	gitCheckoutBranch(t, clone, "fix/wip")
	addExtraCommit(t, origin)
	authFull := trim(fleetGit(t, origin, "rev-parse", "HEAD"))

	s := classifyClone(clone, "as", authFull, origin)
	action, _, advanced := syncClone(clone, s)
	if action != "skip" || advanced {
		t.Errorf("action=%q advanced=%v, want skip/false for off-main clone", action, advanced)
	}
}

// --- small git helpers local to this test file ---

func fleetGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

func trim(s string) string {
	return strings.TrimSpace(s)
}
