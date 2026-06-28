package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupPullConflictRepos builds a local "origin" bare-ish clone relationship:
// an upstream repo that has ADDED extra paths beyond what the local clone's HEAD
// has, with `origin/main` fetched into the local clone. It returns the local
// clone dir, on `main`, with origin/main ahead by the given added files.
func setupPullConflictRepos(t *testing.T, addedByOrigin map[string]string) string {
	t.Helper()
	upstream := t.TempDir()
	initGitRepo(t, upstream)
	// Rename default branch to main for determinism.
	mustGit(t, upstream, "branch", "-M", "main")
	commitFile(t, upstream, "README.md", "base\n")

	local := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", upstream, local).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	mustGit(t, local, "config", "user.email", "test@example.com")
	mustGit(t, local, "config", "user.name", "Test")
	mustGit(t, local, "checkout", "-q", "main")

	// Upstream adds the new paths and commits them — these are the "incoming" files.
	for rel, content := range addedByOrigin {
		commitFile(t, upstream, rel, content)
	}
	// Fetch so origin/main reflects the new commits in the local clone (mirrors
	// the StashPullConflicts fetch; tests don't depend on network).
	mustGit(t, local, "fetch", "-q", "origin", "main")
	return local
}

func stashList(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "stash", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("git stash list: %v\n%s", err, out)
	}
	return string(out)
}

// (a) origin/main adds P; P also exists locally as an untracked file → stash
// EXACTLY P with the st-nonstate-residue label, P leaves the worktree, and the
// ff-merge then succeeds.
func TestStashPullConflicts_StashesCollidingUntracked(t *testing.T) {
	local := setupPullConflictRepos(t, map[string]string{"docs/new.md": "from origin\n"})

	// Local untracked file at the SAME path the incoming merge adds → blocks ff-pull.
	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}

	// A stash with the I-1594 label must exist, naming the colliding path.
	sl := stashList(t, local)
	if !strings.Contains(sl, "st-nonstate-residue:") || !strings.Contains(sl, "docs/new.md") {
		t.Fatalf("expected st-nonstate-residue stash for docs/new.md; got:\n%s", sl)
	}

	// The colliding untracked file must be gone from the worktree.
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); !os.IsNotExist(err) {
		t.Errorf("docs/new.md must be removed from the worktree after stash; stat err=%v", err)
	}

	// The ff-merge must now succeed (the whole point).
	if out, err := exec.Command("git", "-C", local, "merge", "--ff-only", "origin/main").CombinedOutput(); err != nil {
		t.Fatalf("ff-merge should succeed after stash; got: %v\n%s", err, out)
	}
}

// (b) A second untracked file Q that origin does NOT add is LEFT untouched —
// the I-1594 never-blanket-stash regression.
func TestStashPullConflicts_LeavesNonCollidingUntrackedAlone(t *testing.T) {
	local := setupPullConflictRepos(t, map[string]string{"docs/new.md": "from origin\n"})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")        // collides
	addUntrackedFile(t, local, "agent-memory/MEMORY.md", "legit notes\n") // does NOT collide
	addUntrackedFile(t, local, "docs/wip.md", "work in progress\n")       // does NOT collide

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}

	// The non-colliding untracked files must remain on disk, untouched.
	for _, rel := range []string{"agent-memory/MEMORY.md", "docs/wip.md"} {
		if _, err := os.Stat(filepath.Join(local, rel)); err != nil {
			t.Errorf("non-colliding untracked %s must be left untouched; stat err=%v", rel, err)
		}
	}
	// And they must NOT appear in the stash label.
	sl := stashList(t, local)
	if strings.Contains(sl, "agent-memory/MEMORY.md") || strings.Contains(sl, "docs/wip.md") {
		t.Errorf("non-colliding untracked must never be stashed; stash list:\n%s", sl)
	}
	// Only the colliding path is in the label.
	if !strings.Contains(sl, "docs/new.md") {
		t.Errorf("expected colliding docs/new.md in stash; got:\n%s", sl)
	}
}

// (c) Untracked file that does NOT collide with any incoming-added path → no
// stash created, return 0 (the pull failed for some other reason).
func TestStashPullConflicts_NoCollisionNoStash(t *testing.T) {
	local := setupPullConflictRepos(t, map[string]string{"docs/new.md": "from origin\n"})

	// Untracked file at a DIFFERENT path than anything origin adds.
	addUntrackedFile(t, local, "docs/unrelated.md", "local only\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	if sl := strings.TrimSpace(stashList(t, local)); sl != "" {
		t.Errorf("no stash should be created when nothing collides; got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/unrelated.md")); err != nil {
		t.Errorf("non-colliding untracked must be left untouched; stat err=%v", err)
	}
}

// (d) non-main branch → strict no-op, return 0.
func TestStashPullConflicts_NoopOffMain(t *testing.T) {
	local := setupPullConflictRepos(t, map[string]string{"docs/new.md": "from origin\n"})
	checkoutBranch(t, local, "fix/I-1620-feature")

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	if sl := strings.TrimSpace(stashList(t, local)); sl != "" {
		t.Errorf("off-main must be a strict no-op (no stash); got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); err != nil {
		t.Errorf("off-main must leave the untracked file in place; stat err=%v", err)
	}
}
