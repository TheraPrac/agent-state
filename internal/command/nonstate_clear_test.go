package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// addUntrackedFile writes dir/rel without staging or committing it, so git
// status --porcelain shows it as untracked (??).
func addUntrackedFile(t *testing.T, repoDir, rel, content string) {
	t.Helper()
	full := filepath.Join(repoDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// gitPorcelain returns `git status --porcelain --untracked-files=all` for repoDir.
func gitPorcelain(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		t.Fatal(err)
	}
	// Trim only the trailing newline — the leading XY space (e.g. ` D`) is
	// significant (unstaged vs staged), so TrimSpace would corrupt it.
	return strings.Trim(string(out), "\n")
}

// checkoutBranch creates and switches to a feature branch in repoDir.
func checkoutBranch(t *testing.T, repoDir, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", branch).CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", branch, err, out)
	}
}

// commitFile creates, stages and commits dir/rel so it is tracked at HEAD.
func commitFile(t *testing.T, repoDir, rel, content string) {
	t.Helper()
	addUntrackedFile(t, repoDir, rel, content)
	mustGit(t, repoDir, "add", rel)
	mustGit(t, repoDir, "commit", "-m", "add "+rel)
}

const nsItemDir = "agent-state"
const nsAgent = "agent-a"

func TestClearStagedNonState_UnstagesStagedNonState(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commitFile(t, dir, "scripts/foo.py", "base\n")

	// Stage a non-state edit — this is the gate-blocking case.
	if err := os.WriteFile(filepath.Join(dir, "scripts/foo.py"), []byte("staged change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "scripts/foo.py")

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if len(cleared) != 1 || !strings.Contains(cleared[0], "scripts/foo.py") {
		t.Fatalf("expected scripts/foo.py un-staged; got %v", cleared)
	}
	// After: the change is now UNSTAGED (` M`) — the gate skips it — and content
	// is preserved (non-destructive).
	st := gitPorcelain(t, dir)
	if st != "M scripts/foo.py" && st != " M scripts/foo.py" {
		t.Errorf("expected unstaged ` M scripts/foo.py`; got %q", st)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "scripts/foo.py"))
	if string(body) != "staged change\n" {
		t.Errorf("content must be preserved in the working tree; got %q", body)
	}
}

func TestClearStagedNonState_PartiallyStagedKeepsUnstagedHunk(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commitFile(t, dir, "scripts/mm.py", "base\n")

	// Stage one version, then edit again so the file is partially staged (MM).
	if err := os.WriteFile(filepath.Join(dir, "scripts/mm.py"), []byte("staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "scripts/mm.py")
	if err := os.WriteFile(filepath.Join(dir, "scripts/mm.py"), []byte("staged+unstaged\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if len(cleared) != 1 {
		t.Fatalf("expected mm.py un-staged; got %v", cleared)
	}
	// The peer's unstaged hunk must survive intact (the bug the stash approach hit).
	body, _ := os.ReadFile(filepath.Join(dir, "scripts/mm.py"))
	if string(body) != "staged+unstaged\n" {
		t.Errorf("peer's unstaged work must be preserved; got %q", body)
	}
}

func TestClearStagedNonState_StagedDeletionUnstaged(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commitFile(t, dir, "scripts/del.py", "base\n")
	mustGit(t, dir, "rm", "scripts/del.py") // staged deletion `D `

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if len(cleared) != 1 {
		t.Fatalf("expected del.py un-staged; got %v", cleared)
	}
	// After: deletion is unstaged (` D`), which the gate skips — sync unblocked.
	if got := gitPorcelain(t, dir); !strings.Contains(got, " D scripts/del.py") {
		t.Errorf("staged deletion must become unstaged ` D`; got %q", got)
	}
}

func TestClearStagedNonState_StagedRenameBothSidesUnstaged(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commitFile(t, dir, "scripts/ren.py", "x\n")
	mustGit(t, dir, "mv", "scripts/ren.py", "scripts/ren2.py") // staged rename

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if len(cleared) != 1 {
		t.Fatalf("expected the rename un-staged (one residue entry); got %v", cleared)
	}
	// After: rename decomposes into a worktree deletion + untracked file — both
	// gate-skipped, so no lingering staged deletion blocks the gate.
	got := gitPorcelain(t, dir)
	if !strings.Contains(got, " D scripts/ren.py") || !strings.Contains(got, "?? scripts/ren2.py") {
		t.Errorf("rename must un-stage to ` D old` + `?? new`; got %q", got)
	}
}

func TestClearStagedNonState_RenameOutOfAgentStateLeftAlone(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Staged rename of an agent-state item OUT to a non-state path. Un-staging
	// the non-state side would leave the item's staged DELETION, which st sync
	// would commit → silent data loss. The whole rename must be left intact.
	rel := filepath.Join(nsItemDir, "issues", "I-9.md")
	commitFile(t, dir, rel, "id: I-9\n")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "mv", rel, "scripts/leaked.md")

	before := gitPorcelain(t, dir)
	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Fatalf("cross-boundary rename (agent-state→non-state) must be left alone; got %v", cleared)
	}
	if gitPorcelain(t, dir) != before {
		t.Errorf("rename must be left fully intact; before=%q after=%q", before, gitPorcelain(t, dir))
	}
	if !strings.Contains(gitPorcelain(t, dir), "I-9.md") {
		t.Errorf("agent-state rename source must stay visible to the gate; got %q", gitPorcelain(t, dir))
	}
}

func TestClearStagedNonState_RenameIntoAgentStateLeftAlone(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Staged rename of a non-state file INTO agent-state. The new side is a
	// legitimate staged agent-state add; the non-state old side must not be
	// half-cleared. Leave the whole rename.
	commitFile(t, dir, "scripts/foo.py", "x\n")
	if err := os.MkdirAll(filepath.Join(dir, nsItemDir, "issues"), 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "mv", "scripts/foo.py", filepath.Join(nsItemDir, "issues", "foo.py"))

	before := gitPorcelain(t, dir)
	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Fatalf("cross-boundary rename (non-state→agent-state) must be left alone; got %v", cleared)
	}
	if gitPorcelain(t, dir) != before {
		t.Errorf("rename must be left fully intact; before=%q after=%q", before, gitPorcelain(t, dir))
	}
}

func TestClearStagedNonState_LeavesUntrackedAlone(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Untracked non-state (agent-memory/, WIP docs/) legitimately lives in the
	// shared main checkout — the gate skips `??`, so this must too.
	addUntrackedFile(t, dir, "docs/junk.md", "wip\n")
	addUntrackedFile(t, dir, "agent-memory/note.md", "a note\n")

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Fatalf("untracked non-state must be left alone; got %v", cleared)
	}
	if gitPorcelain(t, dir) == "" {
		t.Errorf("untracked files must remain")
	}
}

func TestClearStagedNonState_LeavesUnstagedAlone(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Tracked non-state file modified but NOT staged (` M`) — gate skips it.
	addTrackedDirtyFile(t, dir, "scripts/foo.py", "print('unstaged')\n")

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Fatalf("unstaged non-state must be left alone; got %v", cleared)
	}
}

func TestClearStagedNonState_LeavesStagedAgentStateAlone(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// A STAGED agent-state file — handled by OrphanStash + normal st sync, never
	// un-staged here (exercises the managed-path skip on a staged entry).
	rel := filepath.Join(nsItemDir, "tasks", "T-1.md")
	commitFile(t, dir, rel, "id: T-1\n")
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("id: T-1\nstatus: coding\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", rel)

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Errorf("staged agent-state must be left alone; got %v", cleared)
	}
	if !strings.Contains(gitPorcelain(t, dir), "M  "+rel) {
		t.Errorf("agent-state file must remain STAGED; got %q", gitPorcelain(t, dir))
	}
}

func TestClearStagedNonState_NoopOffMain(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	checkoutBranch(t, dir, "fix/I-999-feature")

	// Staged non-state on a feature branch is the agent's OWN legitimate WIP.
	commitFile(t, dir, "scripts/foo.py", "base\n")
	if err := os.WriteFile(filepath.Join(dir, "scripts/foo.py"), []byte("wip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "scripts/foo.py")

	cleared := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if cleared != nil {
		t.Errorf("off main must be a strict no-op; got %v", cleared)
	}
	if !strings.Contains(gitPorcelain(t, dir), "M  scripts/foo.py") {
		t.Errorf("feature-branch staged WIP must remain staged")
	}
}

func TestClearStagedNonState_NoopFlatLayout(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Flat layout (Paths.Root "."): item files at the repo root must NOT be
	// treated as residue — the gate fail-opens, so this no-ops.
	commitFile(t, dir, "issues/I-5.md", "id: I-5\n")
	if err := os.WriteFile(filepath.Join(dir, "issues/I-5.md"), []byte("id: I-5 edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "issues/I-5.md")

	if cleared := ClearStagedNonState(dir, ".", nsAgent); cleared != nil {
		t.Errorf("flat layout must be a strict no-op; got %v", cleared)
	}
}

func TestClearStagedNonState_NoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if cleared := ClearStagedNonState(dir, nsItemDir, nsAgent); cleared != nil {
		t.Errorf("expected nil on clean tree; got %v", cleared)
	}
}

func TestClearStagedNonState_Idempotent(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commitFile(t, dir, "scripts/foo.py", "base\n")
	if err := os.WriteFile(filepath.Join(dir, "scripts/foo.py"), []byte("staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "scripts/foo.py")

	first := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if len(first) != 1 {
		t.Fatalf("first run should un-stage 1 file; got %v", first)
	}
	second := ClearStagedNonState(dir, nsItemDir, nsAgent)
	if second != nil {
		t.Errorf("second run should be a no-op (nothing staged); got %v", second)
	}
}

func TestOrphanList_ShowsNonStateResidue(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// OrphanList still surfaces any legacy st-nonstate-residue stashes (left by
	// the earlier stash-based cut) alongside st-orphan stashes, for cleanup.
	fakeOutput := `stash@{0}: On main: st-orphan: agent-state/tasks/T-001.md owned-by:agent-b dropped-by:agent-i date:2026-06-14
stash@{1}: On main: st-nonstate-residue: scripts/foo.py dropped-by:agent-a date:2026-06-26
stash@{2}: On main: WIP on main: unrelated`
	orig := execGitOrphan
	defer func() { execGitOrphan = orig }()
	execGitOrphan = func(d string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "stash" && args[1] == "list" {
			return []byte(fakeOutput), nil
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		return cmd.Output()
	}

	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	OrphanList(dir)
	w.Close()
	os.Stdout = origStdout
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	if !strings.Contains(output, "st-nonstate-residue: scripts/foo.py") {
		t.Errorf("expected non-state residue stash listed; got:\n%s", output)
	}
	if !strings.Contains(output, "st-orphan: agent-state/tasks/T-001.md") {
		t.Errorf("expected orphan stash listed; got:\n%s", output)
	}
	if strings.Contains(output, "WIP on main: unrelated") {
		t.Errorf("unrelated stash should be filtered; got:\n%s", output)
	}
}
