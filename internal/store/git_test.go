package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	// Mirror production .gitignore so test commits don't pick up
	// transient runtime files. `.st-git.lock` exists on disk during the
	// I-575 `git add` calls because acquireGitLock holds it; without
	// this ignore line, the auto-stage step would commit the lock file
	// in tests and the assertions would silently disagree with what
	// production sees. The production workspace .gitignore covers it
	// at workspace level; tests need a local one in their temp repo.
	gitignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignore); os.IsNotExist(err) {
		os.WriteFile(gitignore, []byte("**/.st-git.lock\n"), 0644)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Scrub GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE from inherited
		// env so a developer running `go test` from a shell that has
		// those vars exported doesn't accidentally route git at the
		// wrong repo. Mirrors the production-side gateGitOutput helper
		// and the gitRun test helper.
		env := os.Environ()
		filtered := make([]string, 0, len(env)+2)
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_DIR=") ||
				strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
				strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
				continue
			}
			filtered = append(filtered, kv)
		}
		filtered = append(filtered,
			"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
			"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
		)
		cmd.Env = filtered
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	git("add", "-A")
	git("commit", "-m", "initial")
}

func TestAcquireGitLockWritesPID(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireGitLockTimeout(dir, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer unlock()

	content, err := os.ReadFile(filepath.Join(dir, ".st-git.lock"))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	want := fmt.Sprintf("pid=%d", os.Getpid())
	if !strings.Contains(string(content), want) {
		t.Errorf("lock file %q does not contain %q", string(content), want)
	}
}

func TestAcquireGitLockTimesOutWithHolderInfo(t *testing.T) {
	dir := t.TempDir()
	// Goroutine A acquires and holds the lock.
	unlock, err := acquireGitLockTimeout(dir, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer unlock()

	// Goroutine B times out and should report the holder's PID.
	_, err = acquireGitLockTimeout(dir, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	want := fmt.Sprintf("pid=%d", os.Getpid())
	if !strings.Contains(err.Error(), want) {
		t.Errorf("timeout error %q does not name holder PID %q", err.Error(), want)
	}
}

func TestAcquireGitLock_TimeoutReturnsSentinel(t *testing.T) {
	dir := t.TempDir()
	// Hold the lock so a second acquire times out.
	unlock, err := acquireGitLockTimeout(dir, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer unlock()

	_, err = acquireGitLockTimeout(dir, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, ErrGitLockTimeout) {
		t.Errorf("expected errors.Is(err, ErrGitLockTimeout) = true, got err = %v", err)
	}
}

func TestGitSyncDisabled(t *testing.T) {
	root, _ := setupTestDir(t)
	cfg := config.Defaults()
	cfg.Git = nil // disable git
	cfg.Paths.Root = root

	// Override the root by creating a .as config
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, _ = config.Load(root)
	cfg.Git = nil // disable

	s, _ := New(cfg)
	err := s.GitSync("test")
	if err != nil {
		t.Errorf("GitSync disabled should not error: %v", err)
	}
}

func TestGitSyncNoAutoCommit(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: false}

	s, _ := New(cfg)
	err := s.GitSync("test")
	if err != nil {
		t.Errorf("GitSync no autocommit should not error: %v", err)
	}
}

func TestGitSyncHappy(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// Make a change
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("test update")
	if err != nil {
		t.Errorf("GitSync: %v", err)
	}

	// Verify commit was made
	cmd := exec.Command("git", "log", "--oneline", "-1")
	cmd.Dir = root
	out, _ := cmd.Output()
	if len(out) == 0 {
		t.Error("expected a git commit")
	}
}

// I-1451: on workspaces where .st-git.lock was committed before the .gitignore
// rule, it stays tracked, so `git add -u` re-stages its per-op churn every sync
// (blocking session-stop + polluting history). GitSync must drop it from the
// index. This reproduces that production state (force-tracked lock) and asserts
// GitSync untracks it.
func TestGitSyncUntracksLegacyTrackedLock(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	gitT := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// Production clones are on `main`; commitStagedOntoMain (I-1313) advances
	// refs/heads/main, so the working branch must BE main for the index/HEAD to
	// reflect the lock removal (initGitRepo's default branch may be master).
	gitT("branch", "-M", "main")

	// Force-track a committed lock (the pre-.gitignore production state).
	lockPath := filepath.Join(root, ".st-git.lock")
	os.WriteFile(lockPath, []byte("pid=1 cmd=st sync"), 0644)
	gitT("add", "-f", ".st-git.lock")
	gitT("commit", "-m", "legacy: committed lock")

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, _ := New(cfg)

	// The lock mutates every op; also make a real change so GitSync commits.
	os.WriteFile(lockPath, []byte("pid=2 cmd=st update"), 0644)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("update"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	// The lock must no longer be tracked.
	if tracked := gitT("ls-files", "--", ".st-git.lock"); strings.TrimSpace(tracked) != "" {
		t.Errorf(".st-git.lock still tracked after GitSync: %q", tracked)
	}
	// A second sync (lock churns again) must not re-stage or commit it.
	os.WriteFile(lockPath, []byte("pid=3 cmd=st again"), 0644)
	item2, _ := s.Get("T-002")
	item2.Doc.SetField("status", "active")
	s.write(item2)
	if err := s.GitSync("update 2"); err != nil {
		t.Fatalf("GitSync 2: %v", err)
	}
	if files := gitT("show", "--name-only", "--format=", "HEAD"); strings.Contains(files, ".st-git.lock") {
		t.Errorf("second commit must not touch .st-git.lock; files:\n%s", files)
	}
}

func TestGitSyncNothingToCommit(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// No changes — should be a no-op
	err := s.GitSync("empty commit")
	if err != nil {
		t.Errorf("GitSync empty: %v", err)
	}
}

// I-442 + I-575: GitSync's auto-stage scope is deliberately narrow.
//
// Tracked-and-modified anywhere under the item-dir is always staged
// (via `git add -u`). Untracked files inside the autoStageSubdirs list
// (currently just `.plans/`) are also auto-staged — that's the I-575
// addition. Everything else stays untracked, including:
//   - peer-WIP at the WORKSPACE root: .as/sessions/, .claude/,
//     build artifacts (this test's primary subject)
//   - peer-WIP item files in agent-state/issues/, /tasks/, /archive/
//     (covered separately by TestGitSyncDoesNotSweepUntrackedPeerItemFiles)
//
// Test layout mirrors production: workspace root with a nested
// agent-state/ items dir + sibling .as/ where peer-session WIP lives.
func TestGitSyncDoesNotSweepUntrackedPeerFilesOutsideItemDir(t *testing.T) {
	workspace := t.TempDir()
	itemDir := filepath.Join(workspace, "agent-state")
	for _, dir := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(itemDir, dir), 0755)
	}
	writeItem(t, filepath.Join(itemDir, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task

depends_on:
- []
`)
	asDir := filepath.Join(workspace, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)
	initGitRepo(t, workspace)

	cfg, _ := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// This agent modifies a tracked item inside agent-state/.
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	// Peer-agent WIP outside the items dir: a session yaml in .as/, a
	// build artifact at the workspace root. NEITHER should be swept.
	peerSession := filepath.Join(asDir, "sessions", "peer.yaml")
	os.MkdirAll(filepath.Dir(peerSession), 0755)
	os.WriteFile(peerSession, []byte("session: peer\n"), 0644)
	rootJunk := filepath.Join(workspace, "build-artifact.tmp")
	os.WriteFile(rootJunk, []byte("scratch\n"), 0644)

	if err := s.GitSync("agent-b: update T-001"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	// The commit MUST contain the tracked T-001 modification.
	cmd := exec.Command("git", "show", "--stat", "--name-only", "HEAD")
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	body := string(out)
	if !contains(body, "T-001") {
		t.Errorf("HEAD commit should include the modified T-001 file. got:\n%s", body)
	}

	// The commit MUST NOT contain peer-WIP outside the item-dir.
	if contains(body, "peer.yaml") || contains(body, "build-artifact") {
		t.Errorf("HEAD commit swept up an out-of-item-dir peer file. got:\n%s", body)
	}

	// And those files should still exist on disk, untracked.
	for _, p := range []string{peerSession, rootJunk} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("peer file %q disappeared: %v", p, err)
		}
	}
}

// I-442 + I-575: peer-agent untracked item files (e.g. agent-b ran
// `st create` but its own GitSync was rejected before retry) live in
// agent-state/issues/, /tasks/, or /archive/ — autoStageSubdirs
// deliberately excludes these subdirs so this agent's GitSync does NOT
// sweep them. Without this protection the peer's not-yet-committed
// item file lands under the wrong agent's commit attribution. Filed
// during PR #89 code review as a regression of the I-442 invariant
// the original test (now repurposed for outside-item-dir paths) was
// guarding.
func TestGitSyncDoesNotSweepUntrackedPeerItemFiles(t *testing.T) {
	workspace := t.TempDir()
	itemDir := filepath.Join(workspace, "agent-state")
	for _, dir := range []string{"tasks", "issues", "archive", ".plans"} {
		os.MkdirAll(filepath.Join(itemDir, dir), 0755)
	}
	writeItem(t, filepath.Join(itemDir, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task

depends_on:
- []
`)
	asDir := filepath.Join(workspace, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)
	initGitRepo(t, workspace)

	cfg, _ := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// This agent modifies a tracked item.
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	// Peer agent's mid-flight item file: created on disk but never
	// passed to GitSync (their GitSync crashed / was rejected). Lives
	// in issues/, NOT in .plans/.
	peerItem := filepath.Join(itemDir, "issues", "I-999-peer-mid-create.md")
	os.WriteFile(peerItem, []byte("id: I-999\ntype: issue\nstatus: queued\ntitle: peer mid-create\n"), 0644)

	if err := s.GitSync("agent-c: update T-001"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	cmd := exec.Command("git", "show", "--stat", "--name-only", "HEAD")
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	body := string(out)

	if !contains(body, "T-001") {
		t.Errorf("HEAD commit should include the modified T-001 file. got:\n%s", body)
	}
	// The peer's item file MUST NOT be swept — that's the I-442 invariant.
	if contains(body, "I-999") || contains(body, "peer-mid-create") {
		t.Errorf("HEAD commit swept up a peer agent's untracked item file (I-442 regression). got:\n%s", body)
	}
	// And the peer's file must still exist on disk, untracked, ready
	// for the peer agent's own next GitSync to commit.
	if _, err := os.Stat(peerItem); err != nil {
		t.Errorf("peer item file disappeared: %v", err)
	}
}

// I-575: untracked files inside autoStageSubdirs (currently `.plans/`)
// DO get swept into the commit. This is the very behavior I-575 called
// for: agent-state/.plans/<id>.md created by `st prep` or `st start`
// no longer requires a manual `git add` after `st sync`.
func TestGitSyncStagesUntrackedFilesInsideItemDir(t *testing.T) {
	workspace := t.TempDir()
	itemDir := filepath.Join(workspace, "agent-state")
	for _, dir := range []string{"tasks", "issues", "archive", ".plans"} {
		os.MkdirAll(filepath.Join(itemDir, dir), 0755)
	}
	writeItem(t, filepath.Join(itemDir, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task

depends_on:
- []
`)
	asDir := filepath.Join(workspace, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)
	initGitRepo(t, workspace)

	cfg, _ := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// Modify a tracked item.
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	// Drop a brand-new untracked plan file inside agent-state/.plans/.
	// This simulates `st prep` writing a plan file that today fails to
	// land in the commit because `git add -u` skips untracked.
	planPath := filepath.Join(itemDir, ".plans", "T-001.md")
	os.WriteFile(planPath, []byte("# T-001 plan\nseed contents\n"), 0644)

	if err := s.GitSync("agent-c: update T-001 and add plan"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	cmd := exec.Command("git", "show", "--stat", "--name-only", "HEAD")
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	body := string(out)

	// Both files should land in the same commit.
	if !contains(body, "T-001-first-task.md") {
		t.Errorf("HEAD commit missing the modified T-001 item. got:\n%s", body)
	}
	if !contains(body, ".plans/T-001.md") {
		t.Errorf("HEAD commit missing the new plan file (this is the I-575 regression). got:\n%s", body)
	}

	// And the working tree under agent-state should now be clean — no
	// more untracked nag from the next session-stop hook fire.
	cmd = exec.Command("git", "status", "--porcelain", "agent-state/")
	cmd.Dir = workspace
	statusOut, _ := cmd.Output()
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("expected agent-state/ to be clean post-sync; got:\n%s", statusOut)
	}
}

// I-442: callers that create new files (st create, mail.Send, the
// rename half of st close's archive move) pass the path explicitly to
// GitSync so `git add -u` doesn't miss them.
func TestGitSyncStagesExplicitNewPath(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// The agent creates a new item file (this is the st-create scenario).
	newPath := filepath.Join(root, "issues", "I-100-mine.md")
	os.MkdirAll(filepath.Dir(newPath), 0755)
	os.WriteFile(newPath, []byte("id: I-100\ntype: issue\nstatus: queued\ntitle: mine\n"), 0644)

	if err := s.GitSync("st create: I-100", newPath); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	cmd := exec.Command("git", "show", "--stat", "--name-only", "HEAD")
	cmd.Dir = root
	out, _ := cmd.Output()
	if !contains(string(out), "I-100") {
		t.Errorf("expected explicit new path to land in HEAD commit. got:\n%s", string(out))
	}
}

// I-442 follow-up (PR #51 code review): defense in depth — reject
// new-paths that resolve outside the item root so a bugged caller
// can't accidentally stage a sibling agent's file via a `../..`
// pathspec. The whole PR is about preventing cross-tree bleed; this
// closes the explicit-add side door.
func TestGitSyncRejectsPathsOutsideRoot(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, _ := New(cfg)

	// Modify a tracked file so there's something to commit (otherwise
	// the empty-commit early-return fires before the path validation).
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	// Pass an absolute path that resolves OUTSIDE root.
	outside := filepath.Join(filepath.Dir(root), "sibling-agent-wip.md")
	err := s.GitSync("should reject", outside)
	if err == nil {
		t.Errorf("expected error for path outside root, got nil")
	}
}

func TestGitSyncWithPushNoRemote(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}

	s, _ := New(cfg)

	// Make a change
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	// Will fail on push (no remote) — should return error
	err := s.GitSync("test push")
	if err == nil {
		t.Error("expected push error with no remote")
	}
}

// TestGitSyncPushesPreexistingUnpushedCommits (I-1593): a sync with NOTHING new to
// stage must still push agent-state commits that were committed locally but never
// pushed (e.g. a prior sync that committed then failed to push). Before the fix,
// GitSync early-returned nil on "nothing to commit", so `st sync` printed "Synced."
// while local main stayed AHEAD of origin — the false-success this regression guards.
func TestGitSyncPushesPreexistingUnpushedCommits(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	// Establish the remote-tracking ref, then strand an agent-state commit on local
	// main WITHOUT pushing it (simulating a prior committed-but-not-pushed sync).
	run("fetch", "origin")
	run("commit", "--allow-empty", "-m", "stranded agent-state commit")
	if ahead := out("rev-list", "--count", "origin/main..main"); ahead == "0" {
		t.Fatalf("setup: expected local main ahead of origin, got %s", ahead)
	}

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, _ := New(cfg)

	// Sync with NO new working-tree change — must push the stranded commit, not
	// early-return a false success.
	if err := s.GitSync("noop sync"); err != nil {
		t.Fatalf("GitSync should succeed and push the stranded commit, got: %v", err)
	}
	if unpushed := out("rev-list", "--count", "origin/main..main"); unpushed != "0" {
		t.Errorf("I-1593: sync left %s unpushed commit(s) — false 'Synced' (local main ahead of origin)", unpushed)
	}
}

func TestWriteNoDirectory(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item, _ := s.Get("T-001")
	item.Status = "archived" // status with no directory mapping
	item.Doc.SetField("status", "archived")

	// Should handle the directory lookup gracefully
	_ = s.Move("T-001")
}

func TestScanSkipsSubdirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Create a subdirectory in tasks (should be skipped)
	os.MkdirAll(filepath.Join(root, "tasks", "subdir"), 0755)
	writeItem(t, filepath.Join(root, "tasks", "subdir", "T-001-nested.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Nested
`)

	// Also create a non-.md file (should be skipped)
	os.WriteFile(filepath.Join(root, "tasks", "notes.txt"), []byte("not an item"), 0644)

	cfg, _ := config.Load(root)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Should have 0 items (nested items are not scanned, non-.md is skipped)
	if len(s.All()) != 0 {
		t.Errorf("expected 0 items, got %d", len(s.All()))
	}
}

func TestWriteNilDoc(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item, _ := s.Get("T-001")
	item.Doc = nil

	err := s.write(item)
	if err == nil {
		t.Error("Write nil doc should error")
	}
}

// --- RefreshWorkspace (I-380) ---

// setupRefreshTestRepo builds a bare repo + clone wired through cfg for
// RefreshWorkspace to operate on. Returns (cloneDir, originDir, cfg).
// The clone has tasks/ and a single committed item file so cfg.ItemDir()
// resolves correctly.
func setupRefreshTestRepo(t *testing.T) (string, string, *config.Config) {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	clone := filepath.Join(tmp, "clone")

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	if err := os.MkdirAll(origin, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(origin, "init", "--bare", "--initial-branch=main")

	// Seed: clone, add an item, commit, push.
	runGit(tmp, "clone", origin, "clone")
	if err := os.MkdirAll(filepath.Join(clone, "tasks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "tasks", "T-001-seed.md"), []byte("id: T-001\ntype: task\nstatus: queued\n\ntitle: seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(clone, "add", "-A")
	runGit(clone, "commit", "-m", "seed")
	runGit(clone, "push", "origin", "main")

	// .as/config.yaml so config.Load discovers the clone.
	if err := os.MkdirAll(filepath.Join(clone, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(clone)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	return clone, origin, cfg
}

func TestRefreshWorkspaceDisabled(t *testing.T) {
	clone, _, cfg := setupRefreshTestRepo(t)
	_ = clone
	cfg.Git = nil
	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshDisabled {
		t.Fatalf("outcome = %v, want RefreshDisabled", res.Outcome)
	}
}

func TestRefreshWorkspaceUpToDate(t *testing.T) {
	_, _, cfg := setupRefreshTestRepo(t)
	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshUpToDate {
		t.Fatalf("outcome = %v, want RefreshUpToDate (err=%v)", res.Outcome, res.Err)
	}
	if res.PulledCount != 0 {
		t.Errorf("PulledCount = %d, want 0", res.PulledCount)
	}
}

func TestRefreshWorkspacePulled(t *testing.T) {
	clone, origin, cfg := setupRefreshTestRepo(t)
	tmp := filepath.Dir(clone)
	other := filepath.Join(tmp, "other")
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Push 2 new commits via a sibling clone, then refresh the original.
	runGit(tmp, "clone", origin, "other")
	for i := 0; i < 2; i++ {
		path := filepath.Join(other, "tasks", "extra.md")
		if err := os.WriteFile(path, []byte("id: T-002\nbody"+strconv.Itoa(i)+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit(other, "add", "-A")
		runGit(other, "commit", "-m", "more"+strconv.Itoa(i))
	}
	runGit(other, "push", "origin", "main")

	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshPulled {
		t.Fatalf("outcome = %v, want RefreshPulled (err=%v)", res.Outcome, res.Err)
	}
	if res.PulledCount != 2 {
		t.Errorf("PulledCount = %d, want 2", res.PulledCount)
	}
}

// I-430: pure-ahead workspace (local commits not pushed yet, remote has
// nothing new) returns RefreshAhead with the unpushed count — separate
// from RefreshDiverged. Previously this test fixture asserted
// RefreshDiverged, but per I-430 that scenario is "ahead, not diverged."
func TestRefreshWorkspaceAhead(t *testing.T) {
	clone, _, cfg := setupRefreshTestRepo(t)
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Two unpushed local commits; remote stays put.
	for i := 0; i < 2; i++ {
		path := filepath.Join(clone, "tasks", "local"+strconv.Itoa(i)+".md")
		if err := os.WriteFile(path, []byte("id: T-LOCAL"+strconv.Itoa(i)+"\nbody\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit(clone, "add", "-A")
		runGit(clone, "commit", "-m", "local-only-"+strconv.Itoa(i))
	}

	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshAhead {
		t.Fatalf("outcome = %v, want RefreshAhead (err=%v)", res.Outcome, res.Err)
	}
	if res.AheadCount != 2 {
		t.Errorf("AheadCount = %d, want 2", res.AheadCount)
	}
}

// True divergence: both sides have non-shared commits. Distinct from
// the pure-ahead case above; should still return RefreshDiverged.
func TestRefreshWorkspaceDiverged(t *testing.T) {
	clone, origin, cfg := setupRefreshTestRepo(t)
	tmp := filepath.Dir(clone)
	other := filepath.Join(tmp, "other-div")
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Local: one unpushed commit.
	if err := os.WriteFile(filepath.Join(clone, "tasks", "local.md"), []byte("id: T-LOCAL\nbody\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(clone, "add", "-A")
	runGit(clone, "commit", "-m", "local-only")
	// Remote: a sibling clone advances and pushes a different commit.
	runGit(tmp, "clone", origin, "other-div")
	if err := os.WriteFile(filepath.Join(other, "tasks", "remote.md"), []byte("id: T-REMOTE\nbody\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(other, "add", "-A")
	runGit(other, "commit", "-m", "remote-only")
	runGit(other, "push", "origin", "main")

	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshDiverged {
		t.Fatalf("outcome = %v, want RefreshDiverged (err=%v)", res.Outcome, res.Err)
	}
}

func TestRefreshWorkspaceBlocked(t *testing.T) {
	clone, origin, cfg := setupRefreshTestRepo(t)
	tmp := filepath.Dir(clone)
	other := filepath.Join(tmp, "other")
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Origin advances by modifying the seed file.
	runGit(tmp, "clone", origin, "other")
	if err := os.WriteFile(filepath.Join(other, "tasks", "T-001-seed.md"), []byte("id: T-001\nremote-version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(other, "add", "-A")
	runGit(other, "commit", "-m", "remote-edit")
	runGit(other, "push", "origin", "main")

	// Local has uncommitted changes to the same file → ff-only refuses.
	if err := os.WriteFile(filepath.Join(clone, "tasks", "T-001-seed.md"), []byte("id: T-001\nlocal-uncommitted\n"), 0644); err != nil {
		t.Fatal(err)
	}

	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshBlocked {
		t.Fatalf("outcome = %v, want RefreshBlocked (err=%v)", res.Outcome, res.Err)
	}
}

func TestRefreshWorkspaceOffline(t *testing.T) {
	clone, _, cfg := setupRefreshTestRepo(t)
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Point origin at a non-existent path so fetch fails.
	runGit(clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "nowhere.git"))

	res := RefreshWorkspace(cfg)
	if res.Outcome != RefreshOffline {
		t.Fatalf("outcome = %v, want RefreshOffline (err=%v)", res.Outcome, res.Err)
	}
}

func TestRefreshWorkspaceFetchTimeout(t *testing.T) {
	// Smoke: just verify the constant is sane (non-zero, finite). A real
	// timeout test would need a slow remote — covered indirectly by
	// TestRefreshWorkspaceOffline (immediate-fail case uses the same path).
	if refreshFetchTimeout <= 0 {
		t.Fatalf("refreshFetchTimeout must be positive, got %v", refreshFetchTimeout)
	}
}

func TestFilenameForLongTitle(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item := &model.Item{
		ID:    "T-099",
		Title: "This is a very long title that exceeds the sixty character limit for slugs in file names because it is too verbose",
	}
	name := s.filenameForItem(item)
	// Slug should be truncated to 60 chars
	if len(name) > 70 { // "T-099-" + 60 + ".md" = 69
		t.Errorf("filename too long: %q (len=%d)", name, len(name))
	}
}

// --- I-684: st sync must fail loudly on a rejected push ---

// gitBareRemote inits a bare remote, renames the local branch to main,
// seeds the remote's main from the current local HEAD, and wires
// origin. Returns the bare-remote path so a test can install a rejecting
// pre-receive hook AFTER the seed (so the seed itself isn't blocked).
func gitBareRemote(t *testing.T, root string) string {
	t.Helper()
	bare := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(bare, "init", "--bare")
	run(root, "branch", "-M", "main")
	run(root, "remote", "add", "origin", bare)
	run(root, "push", "origin", "refs/heads/main:refs/heads/main") // seed
	return bare
}

// installRejectingPreReceive makes every future push to the bare remote
// fail with a GH001-like message on stderr — the exact pre-receive-decline
// shape that stranded a whole session's agent-state in the I-684 incident.
func installRejectingPreReceive(t *testing.T, bare string) {
	t.Helper()
	hook := filepath.Join(bare, "hooks", "pre-receive")
	body := "#!/bin/sh\n" +
		"echo 'error: GH001: Large files detected. notes.yaml is 142.00 MB; exceeds 100.00 MB' 1>&2\n" +
		"echo 'error: pre-receive hook declined' 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(hook, []byte(body), 0755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
}

func gitSyncStore(t *testing.T, root string) *Store {
	t.Helper()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, _ := New(cfg)
	return s
}

func dirtyItem(t *testing.T, s *Store) {
	t.Helper()
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)
}

// TestGitSync_FromFeatureBranch_LandsOnMainNotBranch is the I-1313
// regression guard: running an agent-state sync while a CODE feature branch
// is checked out must land the commit on main (local + origin) and leave the
// feature branch byte-identical — zero leak onto the PR branch.
func TestGitSync_FromFeatureBranch_LandsOnMainNotBranch(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)

	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Check out a CODE feature branch off main — the exact situation that
	// stranded agent-state commits on PR branches before this fix.
	run("checkout", "-b", "feat/some-code-work")
	featureRefBefore := run("rev-parse", "feat/some-code-work")
	mainBefore := run("rev-parse", "refs/heads/main")

	// st mutates agent-state while standing on the feature branch.
	s := gitSyncStore(t, root)
	dirtyItem(t, s)

	if err := s.GitSync("agent-d: update T-001"); err != nil {
		t.Fatalf("GitSync on a feature branch must succeed, got: %v", err)
	}

	// (1) ZERO LEAK: the feature branch ref must be byte-identical.
	if got := run("rev-parse", "feat/some-code-work"); got != featureRefBefore {
		t.Errorf("agent-state leaked onto the feature branch: ref moved %s -> %s", featureRefBefore, got)
	}

	// (2) local main advanced by exactly one commit.
	mainAfter := run("rev-parse", "refs/heads/main")
	if mainAfter == mainBefore {
		t.Fatal("local main did not advance — the agent-state commit did not land on main")
	}
	if parent := run("rev-parse", "refs/heads/main^"); parent != mainBefore {
		t.Errorf("main advanced by more than one commit (parent=%s want %s)", parent, mainBefore)
	}

	// (3) the new main commit actually contains the T-001 change.
	files := run("diff-tree", "--no-commit-id", "--name-only", "-r", "refs/heads/main")
	if !strings.Contains(files, "T-001") {
		t.Errorf("main commit does not contain the T-001 change; files=%q", files)
	}

	// (4) push landed: origin/main contains the new local main HEAD.
	if err := gitCmdQuiet(root, "merge-base", "--is-ancestor", mainAfter, "refs/remotes/origin/main"); err != nil {
		t.Errorf("origin/main does not contain the new main commit %s — push did not land", mainAfter)
	}

	// (5) HEAD is still the feature branch and nothing is left STAGED
	// against it (no stranded staged diff that a later sync would re-commit).
	if cur := run("symbolic-ref", "--short", "HEAD"); cur != "feat/some-code-work" {
		t.Errorf("HEAD moved off the feature branch to %q", cur)
	}
	if staged := run("diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("agent-state left staged against the feature branch: %q", staged)
	}

	// (6) The working-tree item file is byte-identical to refs/heads/main —
	// this is the precondition the session-stop / st check "ONMAIN" guards
	// rely on to recognize the file as already-committed (not dirty WIP) and
	// skip it rather than re-committing it onto the feature branch.
	if err := gitCmdQuiet(root, "diff", "--quiet", "refs/heads/main", "--",
		"tasks/T-001-first-task.md"); err != nil {
		t.Errorf("working tree does not match refs/heads/main — the ONMAIN guard would not recognize it as already-committed")
	}
}

// TestGitSync_RejectedPushIsLoud is the core I-684 guard: when the remote
// rejects the push (pre-receive / GH001), GitSync MUST return a non-nil
// error (so `st sync` exits 1 and never prints "Synced.") and the error
// MUST carry the remote's actionable text, not a bare "exit status 1".
func TestGitSync_RejectedPushIsLoud(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	bare := gitBareRemote(t, root)
	installRejectingPreReceive(t, bare)

	s := gitSyncStore(t, root)
	dirtyItem(t, s)

	err := s.GitSync("agent-c: update T-001")
	if err == nil {
		t.Fatal("GitSync returned nil on a REJECTED push — st sync would print 'Synced.' while agent-state is stranded local-only (the I-684 demo-killer)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GH001") && !strings.Contains(msg, "pre-receive") &&
		!strings.Contains(msg, "stranded") && !strings.Contains(msg, "did not advance") &&
		!strings.Contains(strings.ToLower(msg), "rejected") {
		t.Errorf("error must surface WHY the push failed (remote text / refs-not-advanced), got: %q", msg)
	}
}

// TestGitSync_HappyPushVerifies: a healthy push to a normal bare remote
// lands and the post-push ground-truth verification passes — GitSync
// returns nil and origin/main truly contains the local commit.
func TestGitSync_HappyPushVerifies(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)

	s := gitSyncStore(t, root)
	dirtyItem(t, s)

	if err := s.GitSync("agent-c: update T-001"); err != nil {
		t.Fatalf("healthy push must succeed and verify, got: %v", err)
	}
	// Ground truth: origin/main must now contain the local main HEAD.
	localHead, _ := gitOutput(root, "rev-parse", "refs/heads/main")
	if err := gitCmdQuiet(root, "merge-base", "--is-ancestor",
		strings.TrimSpace(localHead), "refs/remotes/origin/main"); err != nil {
		t.Errorf("post-push: origin/main does not contain local HEAD — verification was not effective")
	}
}

// TestGitSync_NoAutoPushSkipsVerify: AutoPush=false is commit-only; the
// post-push verification must NOT run (no remote) and GitSync returns nil.
func TestGitSync_NoAutoPushSkipsVerify(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, _ := New(cfg)
	dirtyItem(t, s)

	if err := s.GitSync("commit-only"); err != nil {
		t.Errorf("AutoPush=false must commit-only without verification error, got: %v", err)
	}
}

// TestVerifyPushLanded_DistinctDiagnostics (I-684 review): a missing
// refs/remotes/origin/main must NOT be reported with the misleading
// "oversized file / pre-receive" hint — that is a different failure mode
// (no upstream ref) and the durability primitive must not cry wolf with
// the wrong remediation.
func TestVerifyPushLanded_DistinctDiagnostics(t *testing.T) {
	root := t.TempDir()
	bare := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(bare, "init", "--bare")
	run(root, "init")
	run(root, "config", "user.email", "t@t.co")
	run(root, "config", "user.name", "T")
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0644)
	run(root, "add", "-A")
	run(root, "commit", "-m", "c1")
	run(root, "branch", "-M", "main")
	// Add origin with NO fetch refspec → `git fetch origin main` updates
	// FETCH_HEAD but never creates refs/remotes/origin/main.
	run(root, "remote", "add", "origin", bare)
	run(root, "config", "--unset-all", "remote.origin.fetch")
	run(root, "push", "origin", "refs/heads/main:refs/heads/main")

	err := verifyPushLanded(root)
	if err == nil {
		t.Fatal("verifyPushLanded must not certify when refs/remotes/origin/main is absent")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no refs/remotes/origin/main") && !strings.Contains(msg, "upstream") {
		t.Errorf("missing-remote-ref case must get the no-upstream diagnostic, got: %q", msg)
	}
	if strings.Contains(msg, "oversized") || strings.Contains(msg, "pre-receive gate") {
		t.Errorf("missing-remote-ref must NOT use the stranded/oversized hint (wrong remediation): %q", msg)
	}

	// With the default fetch refspec restored, the same landed push verifies.
	run(root, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	if err := verifyPushLanded(root); err != nil {
		t.Errorf("a genuinely-landed push must verify clean once the remote-tracking ref exists, got: %v", err)
	}
}

// ---- I-807: checkMainBranchGate tests ----

// setupI807Workspace builds the workspace-with-nested-agent-state layout
// used by all I-807 gate tests: workspace root with agent-state/ subdirs,
// a sibling .as/config.yaml, and (optionally) a pre-committed tracked
// non-state file at claude-config/hooks/foo.sh so subsequent edits show
// as tracked-modified rather than `??`-untracked.
func setupI807Workspace(t *testing.T, preCommitNonState bool) (workspace, asDir string) {
	t.Helper()
	workspace = t.TempDir()
	itemDir := filepath.Join(workspace, "agent-state")
	for _, dir := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(itemDir, dir), 0755)
	}
	writeItem(t, filepath.Join(itemDir, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task

depends_on:
- []
`)
	asDir = filepath.Join(workspace, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)

	// Pre-commit a tracked non-state file so later edits show up as
	// tracked-modified (XY = " M") rather than untracked (XY = "??"),
	// which the gate skips by design.
	if preCommitNonState {
		nonStateDir := filepath.Join(workspace, "claude-config", "hooks")
		os.MkdirAll(nonStateDir, 0755)
		os.WriteFile(filepath.Join(nonStateDir, "foo.sh"), []byte("#!/bin/sh\necho original\n"), 0755)
	}

	initGitRepo(t, workspace)

	// Ensure the test branch is `main` regardless of the user's
	// init.defaultBranch config — the gate only fires on main.
	cmd := exec.Command("git", "branch", "-M", "main")
	cmd.Dir = workspace
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch -M main: %v\n%s", err, out)
	}

	return workspace, asDir
}

func loadI807Store(t *testing.T, asDir string) *Store {
	t.Helper()
	cfg, err := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	return s
}

func TestGitSync_RefusesPushOnMain_WhenNonStateFileDirty(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Modify the tracked non-state file and explicitly stage it.
	// I-1472: gate now only fires on staged (index-dirty) non-state entries;
	// working-tree-only modifications no longer block (peer-file false-alarm fix).
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	// Mutate an item to give GitSync something legitimate to commit.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001")
	if err == nil {
		t.Fatalf("expected GitSync to fail closed; it succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude-config/hooks/foo.sh") {
		t.Errorf("error must name the offending file; got: %q", msg)
	}
	if !strings.Contains(msg, "main") {
		t.Errorf("error must name the branch (main); got: %q", msg)
	}
	if !strings.Contains(msg, "ST_SYNC_ALLOW_MAIN") {
		t.Errorf("error must mention the override env var; got: %q", msg)
	}
}

// TestGitSync_AllowsPushOnMain_WhenNonStateFileUnstagedOnly — peer agents'
// uncommitted (working-tree-only) edits to shared hook/doc files must NOT
// block st sync. I-1472: gate skips entries where code[0]==' ' (index clean).
func TestGitSync_AllowsPushOnMain_WhenNonStateFileUnstagedOnly(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Modify the tracked non-state file but do NOT stage it — simulates a
	// peer agent's uncommitted edit visible in the shared workspace checkout.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho peer-wip\n"), 0755)

	// Mutate an item to give GitSync something to commit.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-h: update T-001 (peer file unstaged)"); err != nil {
		t.Fatalf("GitSync must succeed when non-state file is working-tree-only dirty: %v", err)
	}

	// Verify the peer's hook modification was NOT committed.
	out, err := exec.Command("git", "-C", workspace, "show", "--name-only", "--format=", "HEAD").Output()
	if err != nil {
		t.Fatalf("git show HEAD: %v", err)
	}
	if strings.Contains(string(out), "claude-config") {
		t.Errorf("peer's unstaged hook file must not appear in the commit; got:\n%s", out)
	}
}

func TestGitSync_AllowsPushOnMain_WhenOnlyStateDirty(t *testing.T) {
	_, asDir := setupI807Workspace(t, false)

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (clean non-state)"); err != nil {
		t.Fatalf("GitSync should succeed when only agent-state is dirty: %v", err)
	}
}

func TestGitSync_AllowsPushOnMain_WithEnvOverride(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified-with-override\n"), 0755)

	t.Setenv("ST_SYNC_ALLOW_MAIN", "1")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (override set)"); err != nil {
		t.Fatalf("GitSync should succeed with ST_SYNC_ALLOW_MAIN=1: %v", err)
	}
}

func TestGitSync_RefusesPushOnFeatureBranch_WhenNonStateFileDirty(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	gitRun(t, workspace, "checkout", "-b", "fix/I-765-test")

	// Modify and explicitly stage the non-state file.
	// I-1472: gate fires only on staged entries; unstaged working-tree changes
	// from peer agents no longer block (see TestGitSync_AllowsPushOnMain_WhenNonStateFileUnstagedOnly).
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified-on-feature\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (feature branch, non-state dirty)")
	if err == nil {
		t.Fatal("GitSync must refuse when non-state is dirty on a feature branch (I-765)")
	}
	if !errors.Is(err, ErrI807MainBranchGate) {
		t.Errorf("expected ErrI807MainBranchGate sentinel; got: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude-config/hooks/foo.sh") {
		t.Errorf("error must name the offending file; got: %q", msg)
	}
	if !strings.Contains(msg, "fix/I-765-test") {
		t.Errorf("error must name the branch; got: %q", msg)
	}
	if !strings.Contains(msg, "ST_SYNC_ALLOW_NON_STATE") {
		t.Errorf("error must mention ST_SYNC_ALLOW_NON_STATE; got: %q", msg)
	}
}

func TestGitSync_AllowsPushOnFeatureBranch_WithNonStateOverride(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	gitRun(t, workspace, "checkout", "-b", "fix/I-765-override-test")

	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho override-on-feature\n"), 0755)

	t.Setenv("ST_SYNC_ALLOW_NON_STATE", "1")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (feature branch, override set)"); err != nil {
		t.Fatalf("GitSync must succeed with ST_SYNC_ALLOW_NON_STATE=1 on feature branch: %v", err)
	}
}

func TestGitSync_AllowsPushOnMain_WithNonStateOverride(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho override-on-main\n"), 0755)

	t.Setenv("ST_SYNC_ALLOW_NON_STATE", "1")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (main, ST_SYNC_ALLOW_NON_STATE=1)"); err != nil {
		t.Fatalf("GitSync must succeed with ST_SYNC_ALLOW_NON_STATE=1 on main: %v", err)
	}
}

func TestGitSync_AllowsPushOnFeatureBranch_WhenOnlyStateDirty(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	gitRun(t, workspace, "checkout", "-b", "fix/I-765-state-only-test")

	// No non-state dirty files; only the agent-state item mutation below.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (feature branch, state-only dirty)"); err != nil {
		t.Fatalf("GitSync must succeed on feature branch when only state is dirty: %v", err)
	}
}

func TestGitSync_AllowsPushOnFeatureBranch_WhenOnlyUntrackedNonState(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	gitRun(t, workspace, "checkout", "-b", "fix/I-765-untracked-test")

	// Drop an UNTRACKED non-state file — gate must skip ?? entries (I-442).
	nonStateDir := filepath.Join(workspace, "claude-config", "hooks")
	os.MkdirAll(nonStateDir, 0755)
	os.WriteFile(filepath.Join(nonStateDir, "new.sh"), []byte("#!/bin/sh\necho untracked\n"), 0755)

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (feature branch, untracked non-state)"); err != nil {
		t.Fatalf("GitSync must skip ?? entries on feature branch (I-442): %v", err)
	}
}

// TestGitSync_AllowsPushOnMain_WhenOnlyUntrackedNonState locks in the
// I-442 peer-WIP protection: pure-untracked (`??`) non-state files MUST
// NOT trigger the I-807 gate. `git add -u` wouldn't stage them anyway,
// so they're not a push-to-main risk; flagging them would regress I-442.
func TestGitSync_AllowsPushOnMain_WhenOnlyUntrackedNonState(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	// Drop an UNTRACKED non-state file. Never git add-ed; porcelain
	// will report it as `??` — the gate must skip it.
	nonStateDir := filepath.Join(workspace, "claude-config", "hooks")
	os.MkdirAll(nonStateDir, 0755)
	os.WriteFile(filepath.Join(nonStateDir, "new.sh"),
		[]byte("#!/bin/sh\necho brand-new untracked\n"), 0755)

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (untracked non-state present)"); err != nil {
		t.Fatalf("GitSync must skip ?? entries (I-442): %v", err)
	}

	// And the untracked file should still be untracked (not swept in).
	statusCmd := exec.Command("git", "status", "--porcelain", "claude-config/hooks/new.sh")
	statusCmd.Dir = workspace
	out, _ := statusCmd.Output()
	if !strings.HasPrefix(string(out), "??") {
		t.Errorf("untracked file should remain untracked after GitSync; status output: %q", string(out))
	}
}

// ---- I-807 review-driven additions (PR #163 review round 1) ----

// I-807 review #1: quoted-path mis-classification. Porcelain v1
// (without -z) emits filenames containing spaces or non-ASCII as
// `"path with space.md"` — surrounding double-quotes that defeat the
// HasPrefix allowlist check. (`core.quotePath=false` does NOT help;
// it only controls non-ASCII quoting, not space quoting.) Pinned
// with `git status --porcelain -z` (NUL-terminated, raw bytes).
func TestGitSync_AllowsPushOnMain_StateFileWithSpaceInName(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	// Drop a tracked state file with a space in its name BEFORE
	// initGitRepo's commit so it's committed in the initial commit.
	// (initGitRepo runs `git add -A` then `git commit` — but
	// initGitRepo already ran in setupI807Workspace. So commit it
	// in a follow-up commit.)
	spacedPath := filepath.Join(workspace, "agent-state", "tasks", "T-002 with space.md")
	os.WriteFile(spacedPath, []byte(`id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: With space

depends_on:
- []
`), 0644)
	gitRun(t, workspace, "add", "agent-state/tasks/T-002 with space.md")
	gitRun(t, workspace, "commit", "-m", "add spaced item")

	// Now modify it — should appear in porcelain as a tracked-modified
	// entry. Without quotePath=false the path arrives wrapped in `"`.
	os.WriteFile(spacedPath, []byte(`id: T-002
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: With space

depends_on:
- []
`), 0644)

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (spaced peer file present)"); err != nil {
		t.Fatalf("GitSync must not flag a state file with a space in its name: %v", err)
	}
}

// I-807 review #1: rename FROM non-state INTO state must flag the
// non-state source. The both-names-gated semantics ensure the deletion
// side of a rename is surfaced.
func TestGitSync_RefusesPushOnMain_RenameFromNonStateIntoState(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Ensure the rename target dir exists (setupI807Workspace creates
	// tasks/issues/archive but not .plans/).
	os.MkdirAll(filepath.Join(workspace, "agent-state", ".plans"), 0755)

	// Stage a rename from claude-config/hooks/foo.sh (committed in
	// setupI807Workspace via preCommitNonState=true) into agent-state/.
	gitRun(t, workspace, "mv",
		"claude-config/hooks/foo.sh",
		"agent-state/.plans/T-999-rehome.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (rename across boundary)")
	if err == nil {
		t.Fatalf("expected refusal; rename from non-state should flag the source")
	}
	if !strings.Contains(err.Error(), "claude-config/hooks/foo.sh") {
		t.Errorf("error must name the non-state rename source; got: %q", err.Error())
	}
}

// I-807 review #1: a path containing literal ` -> ` should NOT trigger
// rename parsing — the XY status code is the source of truth for renames.
func TestGitSync_AllowsPushOnMain_FilenameWithArrowSubstring(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	// Tracked state file whose name contains the literal ` -> `.
	arrowPath := filepath.Join(workspace, "agent-state", "tasks", "T-003 -> followup.md")
	os.WriteFile(arrowPath, []byte(`id: T-003
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Arrow name

depends_on:
- []
`), 0644)
	gitRun(t, workspace, "add", "agent-state/tasks/T-003 -> followup.md")
	gitRun(t, workspace, "commit", "-m", "add arrow item")

	// Modify (NOT rename) — porcelain emits ` M "agent-state/tasks/T-003 -> followup.md"`.
	// XY code is ` M`, not `R`, so the rename branch must NOT fire.
	os.WriteFile(arrowPath, []byte(`id: T-003
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Arrow name

depends_on:
- []
`), 0644)

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (arrow filename present)"); err != nil {
		t.Fatalf("modified file with arrow substring in name must not trigger rename split: %v", err)
	}
}

// I-807 review #1: staged-add of a non-state file is the most direct
// 9edc8732b reproduction — explicit coverage.
func TestGitSync_RefusesPushOnMain_StagedAddNonState(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	// Create + stage a new non-state file (XY = `A ` in porcelain).
	newPath := filepath.Join(workspace, "claude-config", "hooks", "newhook.sh")
	os.MkdirAll(filepath.Dir(newPath), 0755)
	os.WriteFile(newPath, []byte("#!/bin/sh\necho new\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/newhook.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (staged add present)")
	if err == nil {
		t.Fatalf("expected refusal on staged-add of non-state file")
	}
	if !strings.Contains(err.Error(), "claude-config/hooks/newhook.sh") {
		t.Errorf("error must name the staged-added file; got: %q", err.Error())
	}
}

// I-807 review #1: a stranded local commit (committed locally, not yet
// pushed) that touches a non-state file must also be flagged — working-
// tree porcelain alone misses it because the commit is already made.
func TestGitSync_RefusesPushOnMain_StrandedLocalNonStateCommit(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Wire up a synthetic origin so refs/remotes/origin/main has a
	// commit BEHIND HEAD — the gate's `origin/main..HEAD` log scan
	// will then surface the stranded commit's non-state files.
	upstream := t.TempDir()
	gitRun(t, upstream, "init", "--bare")
	gitRun(t, workspace, "remote", "add", "origin", upstream)
	gitRun(t, workspace, "push", "-u", "origin", "main")

	// Commit a non-state change to local main (not pushed yet).
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho stranded\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")
	gitRun(t, workspace, "commit", "-m", "stranded non-state")

	// Working tree is now CLEAN (no porcelain entries) but the unpushed
	// commit carries a non-state file change.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (stranded commit ahead)")
	if err == nil {
		t.Fatalf("expected refusal: stranded local non-state commit should be flagged")
	}
	if !strings.Contains(err.Error(), "claude-config/hooks/foo.sh") {
		t.Errorf("error must name the stranded-committed file; got: %q", err.Error())
	}
}

// I-807 review #1: the override emits an audit-stderr line ONLY when
// the gate would have fired, AND the line names the offender list.
func TestGitSync_OverrideAuditNamesOffenders(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)
	// Stage the non-state file so the gate (and override audit) fires.
	// I-1472: gate only fires on staged (index-dirty) entries.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho dirty\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	t.Setenv("ST_SYNC_ALLOW_MAIN", "1")

	// Capture stderr via a pipe. defer restores even if an intermediate
	// t.Fatalf fires between the swap and the explicit close, so the
	// test runner's own stderr stays unaffected.
	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	defer func() {
		os.Stderr = origStderr
		w.Close() // idempotent if the explicit mid-test w.Close() already ran
		r.Close()
	}()
	os.Stderr = w

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)
	err := s.GitSync("agent-b: update T-001 (override with offender)")

	w.Close()
	os.Stderr = origStderr
	captured, _ := io.ReadAll(r)
	msg := string(captured)

	if err != nil {
		t.Fatalf("override should bypass the gate: %v", err)
	}
	if !strings.Contains(msg, "ST_SYNC_ALLOW_MAIN=1") {
		t.Errorf("audit stderr must announce the bypass; got: %q", msg)
	}
	if !strings.Contains(msg, "claude-config/hooks/foo.sh") {
		t.Errorf("audit stderr must name the offender that was greenlit; got: %q", msg)
	}
}

// I-807 review #1: the override emits NO audit line when the gate
// would not have fired (no offenders OR not on main). Prevents noise
// on every GitSync when the operator leaves the override exported.
func TestGitSync_OverrideSilentWhenGateWouldNotFire(t *testing.T) {
	_, asDir := setupI807Workspace(t, false)
	t.Setenv("ST_SYNC_ALLOW_MAIN", "1")

	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	defer func() {
		os.Stderr = origStderr
		w.Close() // idempotent if the explicit mid-test w.Close() already ran
		r.Close()
	}()
	os.Stderr = w

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)
	err := s.GitSync("agent-b: update T-001 (override on clean tree)")

	w.Close()
	os.Stderr = origStderr
	captured, _ := io.ReadAll(r)
	msg := string(captured)

	if err != nil {
		t.Fatalf("clean-tree sync should succeed: %v", err)
	}
	if strings.Contains(msg, "ST_SYNC_ALLOW_MAIN=1") {
		t.Errorf("audit stderr must NOT fire when gate would not have refused; got: %q", msg)
	}
}

// I-765: ST_SYNC_ALLOW_MAIN=1 is main-only in scope. On a feature branch
// with dirty non-state, the gate still fires (refuses) even when
// ST_SYNC_ALLOW_MAIN=1 is set. Only ST_SYNC_ALLOW_NON_STATE=1 bypasses
// on non-main branches.
func TestGitSync_RefusesPushOnFeatureBranch_WhenAllowMainOverrideSet(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)
	gitRun(t, workspace, "checkout", "-b", "fix/I-765-allow-main-scope-test")
	// Stage the non-state file — I-1472: gate fires only on staged entries.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho feature-with-allow-main\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	t.Setenv("ST_SYNC_ALLOW_MAIN", "1")

	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	defer func() {
		os.Stderr = origStderr
		w.Close()
		r.Close()
	}()
	os.Stderr = w

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)
	err := s.GitSync("agent-b: update T-001 (ST_SYNC_ALLOW_MAIN + dirty + feature)")

	w.Close()
	os.Stderr = origStderr
	captured, _ := io.ReadAll(r)
	auditMsg := string(captured)

	// Gate must refuse: ST_SYNC_ALLOW_MAIN=1 has no effect on feature branches.
	if err == nil {
		t.Fatal("gate must refuse on feature branch even with ST_SYNC_ALLOW_MAIN=1")
	}
	if !errors.Is(err, ErrI807MainBranchGate) {
		t.Errorf("expected ErrI807MainBranchGate sentinel; got: %v", err)
	}
	// Audit stderr must NOT fire (main-only var was not active).
	if strings.Contains(auditMsg, "bypassing gate") {
		t.Errorf("audit stderr must NOT fire when ST_SYNC_ALLOW_MAIN=1 is inactive on feature branch; got: %q", auditMsg)
	}
}

// I-807 review #2: a rename WITHIN agent-state (canonical st close
// pattern: issues/<id>.md -> archive/<id>.md) must NOT be flagged.
// Both-paths-must-be-allowlisted is correct only if within-state
// renames produce zero offenders.
func TestGitSync_AllowsPushOnMain_WithinStateRename(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	// Pre-commit an item file in issues/ so we can rename it.
	srcPath := filepath.Join(workspace, "agent-state", "issues", "I-200-existing.md")
	os.WriteFile(srcPath, []byte(`id: I-200
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Existing
`), 0644)
	gitRun(t, workspace, "add", "agent-state/issues/I-200-existing.md")
	gitRun(t, workspace, "commit", "-m", "add I-200")

	// Now rename within agent-state (issues/ -> archive/). Both paths
	// start with itemsPrefix; offenders must be empty.
	gitRun(t, workspace, "mv",
		"agent-state/issues/I-200-existing.md",
		"agent-state/archive/I-200-existing.md")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("agent-b: update T-001 (within-state rename)"); err != nil {
		t.Fatalf("within-state rename must pass — both paths in allowlist: %v", err)
	}
}

// I-807 review #1: the refusal returns an error wrapping
// ErrI807MainBranchGate so callers can use errors.Is instead of
// string matching the message.
func TestGitSync_RefusalErrorIsSentinel(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)
	// Stage the non-state file — I-1472: gate fires only on staged entries.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho sentinel\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (sentinel check)")
	if err == nil {
		t.Fatalf("expected refusal")
	}
	if !errors.Is(err, ErrI807MainBranchGate) {
		t.Errorf("error must wrap ErrI807MainBranchGate; got: %v", err)
	}
}

// I-807 review #1: detached HEAD pointing at origin/main's SHA is
// functionally equivalent to being on main for push purposes — gate
// should still fire when the working tree has non-state dirt.
func TestGitSync_RefusesPushOnDetachedHEADAtOriginMain(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Wire up a fake origin/main pointing at the same SHA HEAD is on.
	upstream := t.TempDir()
	gitRun(t, upstream, "init", "--bare")
	gitRun(t, workspace, "remote", "add", "origin", upstream)
	gitRun(t, workspace, "push", "-u", "origin", "main")

	// Detach HEAD onto the same commit.
	gitRun(t, workspace, "checkout", "--detach", "HEAD")

	// Stage the non-state file — I-1472: gate fires only on staged entries.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho detached\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (detached HEAD at origin/main)")
	if err == nil {
		t.Fatalf("expected refusal: detached HEAD at origin/main is push-equivalent to main")
	}
	if !errors.Is(err, ErrI807MainBranchGate) {
		t.Errorf("error must wrap ErrI807MainBranchGate; got: %v", err)
	}
}

// I-765: detached HEAD AWAY from origin/main is refused by the non-state gate.
// Under I-807 (main-only) the gate failed-open here; under I-765 (all branches)
// the gate runs on detached HEAD too since pushWithRetry still targets main.
func TestGitSync_RefusesPushOnDetachedHEADAwayFromOriginMain(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, true)

	// Wire up upstream so origin/main exists.
	upstream := t.TempDir()
	gitRun(t, upstream, "init", "--bare")
	gitRun(t, workspace, "remote", "add", "origin", upstream)
	gitRun(t, workspace, "push", "-u", "origin", "main")

	// Make a divergent commit, then detach HEAD on it (different SHA
	// than origin/main).
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho divergent\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")
	gitRun(t, workspace, "commit", "-m", "divergent")
	gitRun(t, workspace, "checkout", "--detach", "HEAD")

	// New non-state edit on the detached HEAD — staged so the gate fires.
	// I-1472: gate skips working-tree-only (unstaged) entries; stage explicitly.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho detached-edit\n"), 0755)
	gitRun(t, workspace, "add", "claude-config/hooks/foo.sh")

	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	err := s.GitSync("agent-b: update T-001 (detached HEAD away from main)")
	if err == nil {
		t.Fatal("gate must refuse on detached HEAD away from origin/main when non-state is dirty (I-765)")
	}
	if !errors.Is(err, ErrI807MainBranchGate) {
		t.Errorf("expected ErrI807MainBranchGate sentinel; got: %v", err)
	}
}

// TestIsManagedStatePath_CaseInsensitivePrefix_I834 verifies that
// isManagedStatePath performs case-insensitive prefix matching so the
// I-807 gate does not mis-classify agent-state files as offenders when
// EvalSymlinks resolves the workspace root to a different case than
// what git status reports (macOS APFS: case-insensitive, case-preserving).
func TestIsManagedStatePath_CaseInsensitivePrefix_I834(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		itemsPrefix string
		want        bool
	}{
		// Identical casing — baseline
		{"identical casing", "agent-state/issues/I-834.md", "agent-state/", true},
		// Mixed-case path, lowercase prefix
		{"mixed-case path", "Agent-State/issues/I-834.md", "agent-state/", true},
		// Lowercase path, mixed-case prefix (EvalSymlinks returns different case)
		{"mixed-case prefix", "agent-state/issues/I-834.md", "Agent-State/", true},
		// Nested workspace shape (theraprac-workspace prefix with casing divergence)
		{"nested workspace mixed", "Theraprac-Workspace/agent-state/issues/I-1.md", "theraprac-workspace/agent-state/", true},
		// .AS/ uppercase variant
		{".AS/ uppercase", ".AS/config.yaml", "", true},
		// Non-state file must still be rejected
		{"non-state file", "claude-config/hooks/foo.sh", "agent-state/", false},
		// Empty itemsPrefix + non-.as path — no items prefix means no match
		{"empty prefix non-as", "claude-config/settings.json", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isManagedStatePath(tc.path, tc.itemsPrefix)
			if got != tc.want {
				t.Errorf("isManagedStatePath(%q, %q) = %v, want %v", tc.path, tc.itemsPrefix, got, tc.want)
			}
		})
	}
}

// TestCheckMainBranchGate_CaseDivergentToplevel_I835 verifies that the
// I-807 gate correctly fail-opens (returns nil) when only agent-state
// files are dirty, even when the items-root path and the git toplevel
// path have different casings for the same components (the I-835 bug).
//
// On macOS APFS (case-insensitive, case-preserving), filepath.EvalSymlinks
// preserves whatever casing was given — so if the store's root path came
// from a lowercase PWD but git rev-parse returns the stored uppercase path,
// the pre-fix filepath.Rel call produces a ../../... traversal path that
// mis-classifies every agent-state file as a non-state offender.
func TestCheckMainBranchGate_CaseDivergentToplevel_I835(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("I-835: APFS case-insensitive path divergence only manifests on macOS")
	}
	workspace, _ := setupI807Workspace(t, false)

	// Dirty an agent-state file so the gate has changes to inspect.
	taskFile := filepath.Join(workspace, "agent-state", "tasks", "T-001-first-task.md")
	if err := os.WriteFile(taskFile, []byte("id: T-001\ntype: task\nstatus: active\n"), 0644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	// Derive a case-divergent items path by lowercasing every component of
	// the workspace. On macOS APFS the filesystem accepts mixed casing, so
	// os.Lstat succeeds. This simulates the production scenario where ST_ROOT
	// comes from a lowercase PWD while git rev-parse returns the stored
	// uppercase path, making filepath.Rel emit a traversal path.
	divergentItems := filepath.Join(strings.ToLower(workspace), "agent-state")
	if _, err := os.Lstat(divergentItems); err != nil {
		t.Skipf("filesystem does not support case-insensitive access (%v) — APFS required", err)
	}

	// With the I-835 fix (strings.ToLower on both inputs to filepath.Rel),
	// the gate must fail-open: nil error means only state files are dirty.
	if err := checkNonStateGate(divergentItems); err != nil {
		t.Errorf("I-835 regression: gate returned non-nil for state-only dirty set with case-divergent path: %v", err)
	}
}

// TestGitSync_DropsLockFileFromIndex_I1451 verifies that GitSync removes
// .st-git.lock from the git index when it is accidentally tracked. Without
// the fix, git add -u would re-stage the mutated lock file on every st op,
// churning history and eventually blocking session-stop.
func TestGitSync_DropsLockFileFromIndex_I1451(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)

	itemDir := filepath.Join(workspace, "agent-state")

	// Force .st-git.lock into the index (bypass .gitignore with -f) and
	// commit it — this simulates the pre-fix scenario where an older st
	// version committed the lock file.
	lockPath := filepath.Join(itemDir, ".st-git.lock")
	if err := os.WriteFile(lockPath, []byte("pid=12345\n"), 0644); err != nil {
		t.Fatalf("write lock fixture: %v", err)
	}
	gitRun(t, workspace, "add", "-f", "agent-state/.st-git.lock")
	gitRun(t, workspace, "commit", "-m", "accidentally track .st-git.lock")

	// Modify the lock file (simulates acquireGitLock rewriting it) so
	// git add -u would stage it again without the fix.
	if err := os.WriteFile(lockPath, []byte("pid=99999\n"), 0644); err != nil {
		t.Fatalf("update lock fixture: %v", err)
	}

	// Mutate a state item to give GitSync something legitimate to commit.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("test: update T-001 (I-1451 lock-file drain)"); err != nil {
		t.Fatalf("GitSync failed: %v", err)
	}

	// After GitSync, refs/heads/main must not contain .st-git.lock in its
	// committed tree. This is the real invariant: the lock file must not
	// appear in the next commit GitSync writes.
	cmd := exec.Command("git", "show", "refs/heads/main:agent-state/.st-git.lock")
	cmd.Dir = workspace
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("I-1451 regression: .st-git.lock is still in refs/heads/main tree after GitSync; content: %s", out)
	}

	// Also verify that a second GitSync does NOT re-commit the lock file.
	item2, _ := s.Get("T-001")
	item2.Doc.SetField("status", "done")
	s.write(item2)
	if err := s.GitSync("test: second sync (I-1451 no-rechurn)"); err != nil {
		t.Fatalf("second GitSync failed: %v", err)
	}
	cmd2 := exec.Command("git", "show", "refs/heads/main:agent-state/.st-git.lock")
	cmd2.Dir = workspace
	if cmd2.Run() == nil {
		t.Error("I-1451 regression: .st-git.lock re-appeared in main tree on second GitSync")
	}
}

// TestGitSync_FeatureBranch_LockNotRestagedAfterReset_I1451 verifies that
// .st-git.lock is not re-staged into the real index after git reset -q on a
// feature branch. Before the fix, commitStagedOntoMain advanced refs/heads/main
// but NOT HEAD; git reset -q then reset the index to HEAD (feature branch tip)
// which still had .st-git.lock tracked, causing every sync to dirty the index
// and block session-stop.
func TestGitSync_FeatureBranch_LockNotRestagedAfterReset_I1451(t *testing.T) {
	workspace, asDir := setupI807Workspace(t, false)
	itemDir := filepath.Join(workspace, "agent-state")

	// Force .st-git.lock into the index on main and commit it.
	lockPath := filepath.Join(itemDir, ".st-git.lock")
	if err := os.WriteFile(lockPath, []byte("pid=12345\n"), 0644); err != nil {
		t.Fatalf("write lock fixture: %v", err)
	}
	gitRun(t, workspace, "add", "-f", "agent-state/.st-git.lock")
	gitRun(t, workspace, "commit", "-m", "accidentally track .st-git.lock on main")

	// Switch to a feature branch. The feature branch tip still has
	// .st-git.lock tracked because it branched from the above commit.
	gitRun(t, workspace, "checkout", "-b", "fix/I-1451-feature-branch-test")

	// Mutate the lock file (acquireGitLock rewrites it every sync).
	if err := os.WriteFile(lockPath, []byte("pid=99999\n"), 0644); err != nil {
		t.Fatalf("update lock fixture: %v", err)
	}

	// Mutate a state item so GitSync has something to commit to main.
	s := loadI807Store(t, asDir)
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("test: update T-001 (I-1451 feature-branch test)"); err != nil {
		t.Fatalf("GitSync failed: %v", err)
	}

	// After GitSync, .st-git.lock must NOT be in the real index — even
	// though we are on a feature branch whose HEAD still has it tracked.
	// Without the fix, git reset -q would have restored it.
	lsCmd := exec.Command("git", "ls-files", "--error-unmatch", "agent-state/.st-git.lock")
	lsCmd.Dir = workspace
	if err := lsCmd.Run(); err == nil {
		t.Error("I-1451 regression (feature branch): .st-git.lock is tracked in real index after GitSync on feature branch")
	}

	// Verify the committed tree on main also excludes it.
	showCmd := exec.Command("git", "show", "refs/heads/main:agent-state/.st-git.lock")
	showCmd.Dir = workspace
	if showCmd.Run() == nil {
		t.Error("I-1451 regression: .st-git.lock is in refs/heads/main tree after GitSync")
	}
}

// gitRun is a test helper that runs `git <args>` in dir and t.Fatals
// on failure with the combined output for diagnostics.
//
// Scrubs GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE from the inherited env
// so a developer running `go test` from a shell that has those vars
// exported (e.g., from an interrupted plumbing-replay debug session)
// doesn't accidentally route git at the wrong repo. Mirrors the
// production-side gateGitOutput helper.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
		"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
	)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGitSync_StagesDotAsInNestedLayout_I1622 is the I-1622 regression guard:
// in a NESTED layout (paths.root: agent-state), st-owned canonical `.as/` state
// (epics+sprints, queue, notes, the agent's stack) is a SIBLING of ItemDir at
// the git toplevel. Before the fix, GitSync's ItemDir-cwd-scoped `git add -u --
// .` never reached it, so those mutations were silently dropped from sync while
// "Synced." still printed (the Inv-1 honest-sync violation found live on
// 2026-06-27). The test asserts the fix AND its PR-#300 review properties:
//   - a tracked-MODIFIED canonical file (epics.yaml) lands on origin/main;
//   - an UNTRACKED-NEW canonical file (queue.yaml) ALSO lands (not just `-u`);
//   - a peer's tracked-modified `.as/` file (mailbox/) is NOT swept;
//   - no exit-128 hard-fail (sync succeeds).
func TestGitSync_StagesDotAsInNestedLayout_I1622(t *testing.T) {
	top := t.TempDir()
	itemsRoot := filepath.Join(top, "agent-state")
	for _, d := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(itemsRoot, d), 0755)
	}
	asDir := filepath.Join(top, ".as")
	os.MkdirAll(asDir, 0755)
	// Nested config: items live under agent-state/, .as/ stays at the toplevel.
	os.WriteFile(filepath.Join(asDir, "config.yaml"),
		[]byte("paths:\n  root: agent-state\n"), 0644)
	// A tracked canonical .as/ state file (the kind that was going unpersisted).
	os.WriteFile(filepath.Join(asDir, "epics.yaml"),
		[]byte("epics: []\nsprints: []\n"), 0644)
	// A peer-owned, tracked .as/ file that must NOT be swept by this agent's sync.
	peerMail := filepath.Join(asDir, "mailbox", "agent-zz")
	os.MkdirAll(peerMail, 0755)
	os.WriteFile(filepath.Join(peerMail, "m.yaml"), []byte("body: v1\n"), 0644)
	// An item under ItemDir so the ItemDir staging path also has content.
	writeItem(t, filepath.Join(itemsRoot, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task
`)

	initGitRepo(t, top)
	gitBareRemote(t, top)

	cfg, err := config.Load(top)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Sanity: nested layout (ItemDir is a subdir of the toplevel).
	if filepath.Clean(cfg.ItemDir()) == filepath.Clean(top) {
		t.Fatalf("expected nested layout, but ItemDir == toplevel (%s)", top)
	}

	// (a) Mutate the canonical .as/epics.yaml — exactly what `st sprint add` /
	// `st epic create` do. Tracked-modified, lives OUTSIDE ItemDir.
	const epicMarker = "test-epic-i1622"
	os.WriteFile(filepath.Join(asDir, "epics.yaml"),
		[]byte("epics:\n  - id: "+epicMarker+"\n    title: T\n    status: active\nsprints: []\n"), 0644)
	// (b) Create an UNTRACKED-NEW canonical file (queue.yaml) — `-u` would skip
	// this; the fix must still persist it.
	const queueMarker = "queued-i1622"
	os.WriteFile(filepath.Join(asDir, "queue.yaml"),
		[]byte("queue:\n  - "+queueMarker+"\n"), 0644)
	// (c) Modify a PEER's tracked .as/ file — must NOT be swept into this sync.
	os.WriteFile(filepath.Join(peerMail, "m.yaml"), []byte("body: v2-peer-wip\n"), 0644)

	if err := s.GitSync("st sprint add: i1622 test"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = top
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	// (a) tracked-modified canonical file landed.
	if blob := run("show", "refs/remotes/origin/main:.as/epics.yaml"); !strings.Contains(blob, epicMarker) {
		t.Errorf("origin/main:.as/epics.yaml missing %q — GitSync did not persist tracked-modified canonical .as/ state (I-1622). Got:\n%s", epicMarker, blob)
	}
	// (b) untracked-NEW canonical file landed (PR #300 finding 3).
	if blob := run("show", "refs/remotes/origin/main:.as/queue.yaml"); !strings.Contains(blob, queueMarker) {
		t.Errorf("origin/main:.as/queue.yaml missing %q — untracked-new canonical .as/ file was dropped (PR #300 finding 3). Got:\n%s", queueMarker, blob)
	}
	// (c) peer's tracked-modified .as/ file was NOT swept (PR #300 finding 2).
	if blob := run("show", "refs/remotes/origin/main:.as/mailbox/agent-zz/m.yaml"); strings.Contains(blob, "v2-peer-wip") {
		t.Errorf("origin/main swept a PEER's tracked-modified .as/ file into this agent's sync (PR #300 finding 2). Got:\n%s", blob)
	}
}

// TestGitSync_NestedLayout_NoCanonicalDotAsFiles_I1622 guards PR #300 finding 1:
// a nested-layout workspace whose `.as/` holds NO st-owned canonical files (only
// config / untracked junk) must sync cleanly. The first cut (`git add -u -- .as`)
// returned exit 128 ("pathspec '.as' did not match any file(s)") here, aborting
// every sync. The explicit per-file `git add` with an os.Stat guard cannot.
func TestGitSync_NestedLayout_NoCanonicalDotAsFiles_I1622(t *testing.T) {
	top := t.TempDir()
	itemsRoot := filepath.Join(top, "agent-state")
	for _, d := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(itemsRoot, d), 0755)
	}
	asDir := filepath.Join(top, ".as")
	os.MkdirAll(asDir, 0755)
	// Only config.yaml under .as/ — no epics/queue/notes/stack canonical files.
	os.WriteFile(filepath.Join(asDir, "config.yaml"),
		[]byte("paths:\n  root: agent-state\n"), 0644)
	writeItem(t, filepath.Join(itemsRoot, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task
`)
	initGitRepo(t, top)
	gitBareRemote(t, top)

	cfg, err := config.Load(top)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Mutate an ITEM (so there is something to sync) — .as/ has no canonical files.
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.write(item)

	if err := s.GitSync("st update: T-001"); err != nil {
		t.Fatalf("GitSync must not hard-fail when .as/ has no canonical files (PR #300 finding 1), got: %v", err)
	}
}

// TestGitSync_StagesDotAsDeletion_I1622 guards PR #300 finding 6: deleting a
// canonical .as/ file (e.g. retiring queue.yaml) must sync the DELETION to
// origin/main. The os.Stat-then-skip first cut dropped deletions; `git add -A`
// over the existing-or-tracked filter records them.
func TestGitSync_StagesDotAsDeletion_I1622(t *testing.T) {
	top := t.TempDir()
	itemsRoot := filepath.Join(top, "agent-state")
	for _, d := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(itemsRoot, d), 0755)
	}
	asDir := filepath.Join(top, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)
	os.WriteFile(filepath.Join(asDir, "queue.yaml"), []byte("queue: [X]\n"), 0644)
	writeItem(t, filepath.Join(itemsRoot, "tasks", "T-001-first-task.md"), "id: T-001\ntype: task\nstatus: queued\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\n\ntitle: First task\n")
	initGitRepo(t, top)
	gitBareRemote(t, top)

	cfg, err := config.Load(top)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, _ := New(cfg)

	// Retire the canonical queue.yaml.
	if err := os.Remove(filepath.Join(asDir, "queue.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := s.GitSync("st: retire queue.yaml"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = top
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, _ := run("ls-tree", "refs/remotes/origin/main", ".as/queue.yaml"); strings.TrimSpace(out) != "" {
		t.Errorf("origin/main still has .as/queue.yaml — deletion was not synced (PR #300 finding 6): %q", out)
	}
}

// TestGitSync_FlatLayout_StagesUntrackedNewCanonical_I1622 guards PR #300
// finding 1: in a FLAT layout, the ItemDir `git add -u -- .` reaches .as/ but
// `-u` ignores untracked-NEW files, so a first-ever canonical file was dropped.
// The canonical-staging block now runs in flat layout too.
func TestGitSync_FlatLayout_StagesUntrackedNewCanonical_I1622(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)

	s := gitSyncStore(t, root)
	dirtyItem(t, s) // something for the ItemDir path to stage

	// First-ever (untracked) canonical queue.yaml.
	const marker = "flat-new-i1622"
	os.WriteFile(filepath.Join(asDir, "queue.yaml"), []byte("queue: ["+marker+"]\n"), 0644)

	if err := s.GitSync("st queue add: flat"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}
	cmd := exec.Command("git", "show", "refs/remotes/origin/main:.as/queue.yaml")
	cmd.Dir = root
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), marker) {
		t.Errorf("origin/main:.as/queue.yaml missing %q — untracked-new canonical file dropped in flat layout (PR #300 finding 1). Got:\n%s", marker, out)
	}
}

// TestContainsConflictMarkers verifies the helper that detects raw git conflict
// marker pairs (<<<<<<< + >>>>>>>). Bare ======= lines must NOT trigger it.
func TestContainsConflictMarkers(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "both markers present",
			content: "<<<<<<< HEAD\nstatus: queued\n=======\nstatus: active\n>>>>>>> origin/main\n",
			want:    true,
		},
		{
			name:    "7-char markers with surrounding text",
			content: "id: I-001\ntitle: foo\n<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> other\n",
			want:    true,
		},
		{
			name:    "open marker only",
			content: "<<<<<<< HEAD\nstatus: queued\n",
			want:    false,
		},
		{
			name:    "close marker only",
			content: ">>>>>>> origin/main\nstatus: active\n",
			want:    false,
		},
		{
			name:    "bare equals only (legitimate SBAR separator)",
			content: "background: |\n  =======\n  some docs\n",
			want:    false,
		},
		{
			name:    "6-char prefix — not a conflict marker",
			content: "<<<<<<\nstatus: queued\n>>>>>>\n",
			want:    false,
		},
		{
			name:    "clean YAML",
			content: "id: T-001\nstatus: queued\ntitle: Normal task\n",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsConflictMarkers(tc.content); got != tc.want {
				t.Errorf("containsConflictMarkers(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestGitSyncRejectsConflictMarkers is the I-1320 Bug B regression guard:
// GitSync must refuse to commit a staged file that contains raw conflict markers.
func TestGitSyncRejectsConflictMarkers(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)
	s := gitSyncStore(t, root)

	// Capture main ref before the attempted sync.
	mainBefore, err := gitOutput(root, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	mainBefore = strings.TrimSpace(mainBefore)

	// Overwrite a tracked file with raw conflict markers.
	conflictContent := "id: T-001\ntype: task\nstatus: queued\ncreated: 2026-03-25T10:00:00-06:00\n" +
		"last_touched: 2026-03-25T10:00:00-06:00\n\ntitle: First task\n\n" +
		"<<<<<<< HEAD\ndepends_on:\n- []\n=======\ndepends_on:\n- [T-002]\n>>>>>>> origin/main\n"
	itemPath := filepath.Join(root, "tasks", "T-001-first-task.md")
	if err := os.WriteFile(itemPath, []byte(conflictContent), 0644); err != nil {
		t.Fatalf("write conflict file: %v", err)
	}

	err = s.GitSync("conflict-marker test")
	if err == nil {
		t.Fatal("GitSync must return an error when staged content contains conflict markers")
	}
	if !strings.Contains(err.Error(), "conflict markers") {
		t.Errorf("error must mention 'conflict markers', got: %v", err)
	}

	// No new commit must have landed on main.
	mainAfter, _ := gitOutput(root, "rev-parse", "refs/heads/main")
	if strings.TrimSpace(mainAfter) != mainBefore {
		t.Errorf("main advanced despite conflict-marker rejection: %s -> %s", mainBefore, strings.TrimSpace(mainAfter))
	}
}

// TestGitSyncAllowsCleanState confirms the happy path: GitSync succeeds and
// commits when no conflict markers are present.
func TestGitSyncAllowsCleanState(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)
	gitBareRemote(t, root)
	s := gitSyncStore(t, root)

	mainBefore, _ := gitOutput(root, "rev-parse", "refs/heads/main")
	mainBefore = strings.TrimSpace(mainBefore)

	dirtyItem(t, s)
	if err := s.GitSync("clean state test"); err != nil {
		t.Fatalf("GitSync with clean content must succeed, got: %v", err)
	}

	mainAfter, _ := gitOutput(root, "rev-parse", "refs/heads/main")
	if strings.TrimSpace(mainAfter) == mainBefore {
		t.Error("main did not advance — commit was not made for clean content")
	}
}

// TestReplay_NestedLayout_PreservesChangedFiles is the I-1315 regression guard.
//
// In a nested layout (paths.root: agent-state), the old replayCommitOnFetchedMain
// used filepath.Join(root, rel) to locate blobs on disk. Because diff-tree emits
// toplevel-relative paths (e.g. "agent-state/tasks/T-002-our.md"), joining them
// onto root = "…/agent-state" double-prefixes to "…/agent-state/agent-state/…" —
// a path that doesn't exist — causing os.Stat to return IsNotExist, which the code
// interpreted as "file was deleted", and it removed the entry from the temp index
// instead of adding it. The fix uses diff-tree --raw -z to obtain blob SHAs and
// modes directly from the commit (no filesystem access) and delegates overlay
// construction to buildOverlayTree, which is anchored to the git toplevel.
func TestReplay_NestedLayout_PreservesChangedFiles(t *testing.T) {
	// Set up nested layout: workspace/ = git toplevel, agent-state/ = root.
	workspace, asDir := setupI807Workspace(t, false)
	root := filepath.Join(workspace, "agent-state")

	// Commit a base file so there's real tree content to overlay onto.
	// Use T-900 to avoid colliding with T-001 written by setupI807Workspace.
	baseFile := filepath.Join(root, "tasks", "T-900-base.md")
	if err := os.WriteFile(baseFile, []byte("id: T-900\nstatus: queued\n"), 0644); err != nil {
		t.Fatalf("write T-900: %v", err)
	}
	gitRun(t, workspace, "add", "-A")
	gitRun(t, workspace, "commit", "-m", "base state")

	// Seed bare remote from this base commit.
	bare := gitBareRemote(t, workspace)

	// Make our unshared local commit: add T-002.
	ourFile := filepath.Join(root, "tasks", "T-002-our.md")
	if err := os.WriteFile(ourFile, []byte("id: T-002\nstatus: active\n"), 0644); err != nil {
		t.Fatalf("write T-002: %v", err)
	}
	gitRun(t, workspace, "add", "-A")
	gitRun(t, workspace, "commit", "-m", "our: add T-002")
	// Do NOT push — this is the unshared local commit that will be replayed.

	// Simulate a peer pushing a non-overlapping commit to origin/main.
	peer := t.TempDir()
	gitRun(t, peer, "clone", bare, ".")
	gitRun(t, peer, "config", "user.email", "peer@test.com")
	gitRun(t, peer, "config", "user.name", "Peer")
	peerFile := filepath.Join(peer, "agent-state", "tasks", "T-003-peer.md")
	if err := os.MkdirAll(filepath.Dir(peerFile), 0755); err != nil {
		t.Fatalf("mkdir peer dir: %v", err)
	}
	if err := os.WriteFile(peerFile, []byte("id: T-003\nstatus: queued\n"), 0644); err != nil {
		t.Fatalf("write T-003: %v", err)
	}
	gitRun(t, peer, "add", "-A")
	gitRun(t, peer, "commit", "-m", "peer: add T-003")
	gitRun(t, peer, "push", "origin", "main")

	// Load the store with nested layout config.
	cfg, err := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, storeErr := New(cfg)
	if storeErr != nil {
		t.Fatalf("New store: %v", storeErr)
	}

	// replayCommitOnFetchedMain must succeed: fetch origin, detect no overlap
	// between T-002 (ours) and T-003 (peer's), and replay our commit on top of
	// the peer's commit.
	if err := s.replayCommitOnFetchedMain(root); err != nil {
		t.Fatalf("replayCommitOnFetchedMain: %v", err)
	}

	// Our file must survive in the replayed commit (was silently dropped before the fix).
	ourContent, err := gitOutput(workspace, "show", "refs/heads/main:agent-state/tasks/T-002-our.md")
	if err != nil {
		t.Fatalf("T-002-our.md not found in refs/heads/main after replay (was silently dropped): %v", err)
	}
	if !strings.Contains(ourContent, "T-002") {
		t.Errorf("T-002 content mangled in replay: %q", ourContent)
	}

	// Peer's file must also be present (we replayed on top of origin/main).
	peerContent, err := gitOutput(workspace, "show", "refs/heads/main:agent-state/tasks/T-003-peer.md")
	if err != nil {
		t.Fatalf("T-003-peer.md not found in refs/heads/main after replay: %v", err)
	}
	if !strings.Contains(peerContent, "T-003") {
		t.Errorf("T-003 content mangled in replay: %q", peerContent)
	}
}
