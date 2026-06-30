package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo initialises a bare git repo in dir (with an initial commit so
// stash has a valid HEAD to stash against).
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Need at least one commit so stash has a valid base.
	placeholder := filepath.Join(dir, ".gitkeep")
	if err := os.WriteFile(placeholder, nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", ".gitkeep"},
		{"commit", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// addTrackedDirtyFile creates dir/rel, writes content, stages it, then
// writes new content so git status --porcelain shows it as modified.
func addTrackedDirtyFile(t *testing.T, repoDir, rel, content string) {
	t.Helper()
	full := filepath.Join(repoDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	// Write initial content and commit it.
	if err := os.WriteFile(full, []byte("original\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", rel},
		{"commit", "-m", "add " + rel},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Modify it so git status shows it dirty.
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// writeOrphanItemFile creates a minimal YAML item file at dir/rel with the given assigned_to value.
func writeOrphanItemFile(t *testing.T, dir, rel, assignedTo string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	content := "id: T-001\ntype: task\nstatus: coding\n"
	if assignedTo != "" {
		content += "assigned_to: " + assignedTo + "\n"
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestOrphanStash_StashesUnownedFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	const itemDir = "agent-state"
	const myAgent = "agent-i"
	const peerAgent = "agent-b"

	// File owned by peer — should be stashed.
	peerRel := filepath.Join(itemDir, "tasks", "T-peer.md")
	writeOrphanItemFile(t, dir, peerRel, peerAgent)
	addTrackedDirtyFile(t, dir, peerRel, "id: T-peer\nassigned_to: "+peerAgent+"\n")

	// File owned by current agent — must NOT be stashed.
	myRel := filepath.Join(itemDir, "tasks", "T-mine.md")
	writeOrphanItemFile(t, dir, myRel, myAgent)
	addTrackedDirtyFile(t, dir, myRel, "id: T-mine\nassigned_to: "+myAgent+"\n")

	stashed := OrphanStash(dir, itemDir, myAgent)
	if len(stashed) != 1 {
		t.Fatalf("expected 1 stash, got %d: %v", len(stashed), stashed)
	}
	if !strings.Contains(stashed[0], peerRel) {
		t.Errorf("stash should reference peer file %q; got %q", peerRel, stashed[0])
	}
	// The stash entry must contain a real stash ref (stash@{N}), not the git push message.
	if !strings.Contains(stashed[0], "stash@{") {
		t.Errorf("stash entry should contain a real stash ref (stash@{N}); got %q", stashed[0])
	}

	// The peer file should now be clean in the working tree.
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--", peerRel).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("peer file should be stashed (clean); git status: %q", string(out))
	}

	// The current agent's file should still be dirty.
	out, err = exec.Command("git", "-C", dir, "status", "--porcelain", "--", myRel).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("own file should remain dirty (not stashed)")
	}
}

func TestOrphanStash_NoopWhenAllOwned(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	const itemDir = "agent-state"
	const myAgent = "agent-i"

	rel := filepath.Join(itemDir, "tasks", "T-mine.md")
	writeOrphanItemFile(t, dir, rel, myAgent)
	addTrackedDirtyFile(t, dir, rel, "id: T-mine\nassigned_to: "+myAgent+"\n")

	stashed := OrphanStash(dir, itemDir, myAgent)
	if len(stashed) != 0 {
		t.Errorf("expected no stashes when all files owned by current agent; got %v", stashed)
	}
}

func TestOrphanStash_NoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	stashed := OrphanStash(dir, "agent-state", "agent-i")
	if stashed != nil {
		t.Errorf("expected nil on clean tree; got %v", stashed)
	}
}

func TestOrphanStash_OwnerUnknown(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	const itemDir = "agent-state"
	const myAgent = "agent-i"

	// File with NO assigned_to — should be left alone (not stashed).
	rel := filepath.Join(itemDir, "tasks", "T-unowned.md")
	writeOrphanItemFile(t, dir, rel, "") // no assigned_to
	addTrackedDirtyFile(t, dir, rel, "id: T-unowned\nstatus: queued\n")

	stashed := OrphanStash(dir, itemDir, myAgent)
	if len(stashed) != 0 {
		t.Errorf("files with no assigned_to should be left alone; got stashes: %v", stashed)
	}
}

func TestOrphanList_FiltersOrphanStashes(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Inject a fake stash list output via the execGitOrphan override.
	fakeOutput := `stash@{0}: On main: st-orphan: agent-state/tasks/T-001.md owned-by:agent-b dropped-by:agent-i date:2026-06-14
stash@{1}: On main: WIP on main: some unrelated stash
stash@{2}: On main: st-orphan: agent-state/issues/I-002.md owned-by:agent-c dropped-by:agent-i date:2026-06-14`
	orig := execGitOrphan
	defer func() { execGitOrphan = orig }()
	execGitOrphan = func(d string, args ...string) ([]byte, error) {
		if args[0] == "stash" && args[1] == "list" {
			return []byte(fakeOutput), nil
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		return cmd.Output()
	}

	// Capture stdout.
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	OrphanList(dir)

	w.Close()
	os.Stdout = origStdout
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	if !strings.Contains(output, "st-orphan: agent-state/tasks/T-001.md") {
		t.Errorf("expected T-001.md in list output; got:\n%s", output)
	}
	if !strings.Contains(output, "st-orphan: agent-state/issues/I-002.md") {
		t.Errorf("expected I-002.md in list output; got:\n%s", output)
	}
	if strings.Contains(output, "WIP on main: some unrelated stash") {
		t.Errorf("non-orphan stash should be filtered out; got:\n%s", output)
	}
}

// TestOrphanStashIncludesAgentMemory verifies that tracked modifications to
// agent-memory/ files are stashed by OrphanStash even though they have no
// assigned_to field (I-1683: those files are deprecated peer-state that blocks
// git pull --rebase on the shared workspace clone).
func TestOrphanStashIncludesAgentMemory(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	// Commit a tracked agent-memory/MEMORY.md and then dirty it (simulating a
	// peer session writing to the deprecated shared memory index).
	addTrackedDirtyFile(t, dir, "agent-memory/MEMORY.md", "peer wrote this\n")

	// Also add an untracked file in agent-memory/ — it must NOT be stashed
	// (untracked files don't block pull --rebase; stashing them silently would lose work).
	untrackedPath := filepath.Join(dir, "agent-memory", "feedback_new.md")
	if err := os.WriteFile(untrackedPath, []byte("untracked\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stashed := OrphanStash(dir, "agent-state", "agent-a")

	if len(stashed) == 0 {
		t.Fatal("expected OrphanStash to stash agent-memory/MEMORY.md; got nothing stashed")
	}
	found := false
	for _, s := range stashed {
		if strings.Contains(s, "agent-memory/MEMORY.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("stash list %v does not mention agent-memory/MEMORY.md", stashed)
	}

	// The untracked file must still be present in the working tree.
	if _, err := os.Stat(untrackedPath); os.IsNotExist(err) {
		t.Error("untracked agent-memory/feedback_new.md should NOT have been stashed")
	}
}
