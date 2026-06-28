package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/testutil"
)

// TestClose_RemovesStaleDuplicate verifies the I-472 fix: when an item
// is closed and a stale duplicate file with the SAME basename exists in
// another type-dir (typical cause: peer-merged feature branch carrying
// the pre-archive copy), Close sweeps the duplicate so only the
// canonical archive copy remains.
func TestClose_RemovesStaleDuplicate(t *testing.T) {
	env := testutil.NewEnv(t)

	// Simulate the peer-merge resurrection: an archive/ duplicate with
	// the SAME basename as the canonical file already exists when
	// close runs. Close mutates tasks/T-003-active.md, renames it to
	// archive/T-003-active.md, and must remove the pre-existing
	// archive/T-003-active.md that the merge re-introduced.
	body := `id: T-003
type: task
status: active
created: 2026-03-25T12:00:00-06:00
last_touched: 2026-03-25T12:00:00-06:00

title: Active task
`
	stale := filepath.Join(env.Root, "archive", "T-003-active.md")
	if err := os.WriteFile(stale, []byte(body), 0644); err != nil {
		t.Fatalf("seed stale dup: %v", err)
	}
	env.Reload(t)

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{AllowMissingCapture: "test: capture gate not under test", NoAC: true, Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	// Walk the tree — exactly one T-003-*.md should remain, in archive/.
	var found []string
	for _, dir := range []string{"tasks", "issues", "archive"} {
		entries, err := os.ReadDir(filepath.Join(env.Root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if name := e.Name(); name == "T-003.md" || (len(name) > 6 && name[:6] == "T-003-") {
				found = append(found, filepath.Join(dir, name))
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 T-003 file post-close, got %d: %v", len(found), found)
	}
	if filepath.Dir(found[0]) != "archive" {
		t.Errorf("surviving file should be in archive/, got %s", found[0])
	}
}

// TestClose_RemovesStaleDuplicate_GitStagesDeletion verifies the
// reviewer's coverage concern: when the stale duplicate file is
// already tracked by git (the realistic peer-merge case where the file
// arrived via `git pull --ff-only`), Close + GitSync must commit the
// deletion via `git add -u`. Otherwise the deletion would sit in the
// working tree forever.
func TestClose_RemovesStaleDuplicate_GitStagesDeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	env := testutil.NewGitEnv(t)

	// Configure GitSync: enable AutoCommit, disable AutoPush (no
	// remote in the temp repo).
	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	body := `id: T-003
type: task
status: active
created: 2026-03-25T12:00:00-06:00
last_touched: 2026-03-25T12:00:00-06:00

title: Active task
`
	// The stale file lives in a directory OTHER than where Move's
	// rename will land. Move sends tasks/ → archive/, so we put the
	// stale at issues/. That way Move's os.Rename target doesn't
	// naturally overwrite it; only RemoveStaleDuplicates can clean
	// it up.
	stale := filepath.Join(env.Root, "issues", "T-003-active.md")
	if err := os.WriteFile(stale, []byte(body), 0644); err != nil {
		t.Fatalf("seed stale dup: %v", err)
	}
	// Commit the stale file so it's tracked — this models the
	// peer-merge scenario where the duplicate arrived via pull and
	// is in the local index. `git add -u` only stages tracked-file
	// modifications, so this is the path the reviewer flagged.
	runGitTest(t, env.Root, "add", "-A")
	runGitTest(t, env.Root, "commit", "-m", "seed stale duplicate")

	env.Reload(t)

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{AllowMissingCapture: "test: capture gate not under test", NoAC: true, Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	// The stale file at issues/ must be gone from disk.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file %s should be removed, stat err=%v", stale, err)
	}

	// More importantly: the deletion must be committed. `git status
	// --porcelain` should be clean for the issues/ path.
	out := gitOutput(t, env.Root, "status", "--porcelain")
	if strings.Contains(out, "T-003") {
		t.Errorf("expected T-003 deletion to be committed, got dirty status:\n%s", out)
	}

	// Working tree should have exactly one T-003 file (the new
	// archive canonical).
	t003 := 0
	for _, dir := range []string{"tasks", "issues", "archive"} {
		files, _ := os.ReadDir(filepath.Join(env.Root, dir))
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "T-003") {
				t003++
			}
		}
	}
	if t003 != 1 {
		t.Errorf("expected 1 T-003 file across all dirs, got %d", t003)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
		"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
