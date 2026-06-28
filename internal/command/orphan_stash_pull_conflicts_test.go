package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pullRepoSpec drives setupPullRepos: it builds an upstream repo + a local clone
// whose origin/<branch> is ahead, so StashPullConflicts can be exercised against
// real git repos.
type pullRepoSpec struct {
	branch      string            // default "main"
	base        map[string]string // committed in upstream BEFORE the clone (present at the clone's HEAD)
	added       map[string]string // committed in upstream AFTER the clone (the "incoming" adds)
	renames     map[string]string // upstream renames old->new AFTER the clone (old must be in base)
	localCommit map[string]string // committed on the LOCAL clone (diverges HEAD from origin)
	diffRenames bool              // set diff.renames=true in the local clone's config
}

// setupPullRepos returns the local clone dir, checked out on spec.branch, with
// origin/<branch> fetched and ahead per the spec.
func setupPullRepos(t *testing.T, spec pullRepoSpec) string {
	t.Helper()
	branch := spec.branch
	if branch == "" {
		branch = "main"
	}

	upstream := t.TempDir()
	initGitRepo(t, upstream)
	mustGit(t, upstream, "branch", "-M", branch)
	commitFile(t, upstream, "README.md", "base\n")
	for rel, content := range spec.base {
		commitFile(t, upstream, rel, content)
	}

	local := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", upstream, local).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	mustGit(t, local, "config", "user.email", "test@example.com")
	mustGit(t, local, "config", "user.name", "Test")
	mustGit(t, local, "checkout", "-q", branch)
	if spec.diffRenames {
		mustGit(t, local, "config", "diff.renames", "true")
	}

	// Local diverging commit(s) — applied before the upstream advances so the two
	// histories genuinely fork.
	for rel, content := range spec.localCommit {
		commitFile(t, local, rel, content)
	}

	// Upstream advances: plain adds, then renames.
	for rel, content := range spec.added {
		commitFile(t, upstream, rel, content)
	}
	for old, newp := range spec.renames {
		if err := os.MkdirAll(filepath.Join(upstream, filepath.Dir(newp)), 0755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, upstream, "mv", old, newp)
		mustGit(t, upstream, "commit", "-m", "rename "+old+" -> "+newp)
	}

	mustGit(t, local, "fetch", "-q", "origin", branch)
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
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})

	// Local untracked file at the SAME path the incoming merge adds → blocks ff-pull.
	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}

	sl := stashList(t, local)
	if !strings.Contains(sl, "st-nonstate-residue:") || !strings.Contains(sl, "docs/new.md") {
		t.Fatalf("expected st-nonstate-residue stash for docs/new.md; got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); !os.IsNotExist(err) {
		t.Errorf("docs/new.md must be removed from the worktree after stash; stat err=%v", err)
	}
	if out, err := exec.Command("git", "-C", local, "merge", "--ff-only", "origin/main").CombinedOutput(); err != nil {
		t.Fatalf("ff-merge should succeed after stash; got: %v\n%s", err, out)
	}
}

// (b) A second untracked file Q that origin does NOT add is LEFT untouched —
// the I-1594 never-blanket-stash regression.
func TestStashPullConflicts_LeavesNonCollidingUntrackedAlone(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")        // collides
	addUntrackedFile(t, local, "agent-memory/MEMORY.md", "legit notes\n") // does NOT collide
	addUntrackedFile(t, local, "docs/wip.md", "work in progress\n")       // does NOT collide

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}

	for _, rel := range []string{"agent-memory/MEMORY.md", "docs/wip.md"} {
		if _, err := os.Stat(filepath.Join(local, rel)); err != nil {
			t.Errorf("non-colliding untracked %s must be left untouched; stat err=%v", rel, err)
		}
	}
	sl := stashList(t, local)
	if strings.Contains(sl, "agent-memory/MEMORY.md") || strings.Contains(sl, "docs/wip.md") {
		t.Errorf("non-colliding untracked must never be stashed; stash list:\n%s", sl)
	}
	if !strings.Contains(sl, "docs/new.md") {
		t.Errorf("expected colliding docs/new.md in stash; got:\n%s", sl)
	}
}

// (c) Untracked file that does NOT collide with any incoming-added path → no
// stash created, return 0 (the pull failed for some other reason).
func TestStashPullConflicts_NoCollisionNoStash(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})

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
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})
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

// #2 (--no-renames): origin renames a → docs/new.md with diff.renames ON in the
// local config; a local untracked docs/new.md must STILL be detected as a
// collision (the rename destination must surface as an add, not be dropped).
func TestStashPullConflicts_RenameDestinationStashed(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{
		base:        map[string]string{"a": "orig\n"},
		renames:     map[string]string{"a": "docs/new.md"},
		diffRenames: true,
	})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	sl := stashList(t, local)
	if !strings.Contains(sl, "st-nonstate-residue:") || !strings.Contains(sl, "docs/new.md") {
		t.Fatalf("rename destination must be stashed even with diff.renames on; got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); !os.IsNotExist(err) {
		t.Errorf("colliding rename-destination must be removed from the worktree; stat err=%v", err)
	}
	if out, err := exec.Command("git", "-C", local, "merge", "--ff-only", "origin/main").CombinedOutput(); err != nil {
		t.Fatalf("ff-merge should succeed after stash; got: %v\n%s", err, out)
	}
}

// #4/#5 (diverged-HEAD guard): the local clone has a commit origin lacks, so a
// real ff is impossible. Even though origin adds a path that exists locally as an
// untracked file, NOTHING must be stashed — the pull failure is divergence, not
// an untracked-file conflict.
func TestStashPullConflicts_DivergedHeadNoStash(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{
		added:       map[string]string{"docs/new.md": "from origin\n"},
		localCommit: map[string]string{"local-only.txt": "diverge\n"},
	})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	if sl := strings.TrimSpace(stashList(t, local)); sl != "" {
		t.Errorf("diverged HEAD must produce no stash; got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); err != nil {
		t.Errorf("diverged HEAD must leave the untracked file in place; stat err=%v", err)
	}
}

// #3 (master default): the branch guard accepts master, and the command must use
// origin/master (not a hardcoded origin/main) — so a master-default clone
// actually stashes the collision instead of silently no-opping.
func TestStashPullConflicts_MasterDefaultBranch(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{
		branch: "master",
		added:  map[string]string{"docs/new.md": "from origin\n"},
	})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	sl := stashList(t, local)
	if !strings.Contains(sl, "st-nonstate-residue:") || !strings.Contains(sl, "docs/new.md") {
		t.Fatalf("master-default clone must stash the collision (origin/master path); got:\n%s", sl)
	}
	if out, err := exec.Command("git", "-C", local, "merge", "--ff-only", "origin/master").CombinedOutput(); err != nil {
		t.Fatalf("ff-merge should succeed after stash; got: %v\n%s", err, out)
	}
}

// #7 (mid-operation guard): a leftover MERGE_HEAD means a merge is paused — the
// command must NOT stash mid-operation, even with a genuine collision present.
func TestStashPullConflicts_MidOperationNoop(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})
	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	// Simulate a paused merge: PreFlightGitState fails on a present .git/MERGE_HEAD.
	if err := os.WriteFile(filepath.Join(local, ".git", "MERGE_HEAD"),
		[]byte("0000000000000000000000000000000000000000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	if sl := strings.TrimSpace(stashList(t, local)); sl != "" {
		t.Errorf("mid-operation must be a strict no-op (no stash); got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); err != nil {
		t.Errorf("mid-operation must leave the untracked file in place; stat err=%v", err)
	}
}

// #9 (cheap-check-first): no untracked files at all ⇒ no possible collision ⇒
// return 0 with no stash (and, by construction, without needing the network diff).
func TestStashPullConflicts_NoUntrackedNoop(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/new.md": "from origin\n"}})
	// Deliberately create NO untracked file.

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	if sl := strings.TrimSpace(stashList(t, local)); sl != "" {
		t.Errorf("no untracked ⇒ no stash; got:\n%s", sl)
	}
}

// #1 (case-insensitive intersection): origin adds docs/New.md; the local untracked
// file is docs/new.md (case-only difference). On a case-sensitive FS these are
// distinct paths, so ONLY the ToLower-keyed match detects the collision — this
// test fails if the case-fold is dropped. On a case-insensitive FS (the operator's
// APFS) the two names denote the same path and the match holds trivially, so the
// assertion passes on both.
func TestStashPullConflicts_CaseInsensitiveMatch(t *testing.T) {
	local := setupPullRepos(t, pullRepoSpec{added: map[string]string{"docs/New.md": "from origin\n"}})

	addUntrackedFile(t, local, "docs/new.md", "local untracked\n")

	if rc := StashPullConflicts(local, "agent-a"); rc != 0 {
		t.Fatalf("expected rc 0; got %d", rc)
	}
	sl := stashList(t, local)
	if !strings.Contains(sl, "st-nonstate-residue:") || !strings.Contains(sl, "docs/new.md") {
		t.Fatalf("case-only-differing incoming add must collide with the local untracked path; got:\n%s", sl)
	}
	if _, err := os.Stat(filepath.Join(local, "docs/new.md")); !os.IsNotExist(err) {
		t.Errorf("colliding untracked (case-fold) must be removed; stat err=%v", err)
	}
}
