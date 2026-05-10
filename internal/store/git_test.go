package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
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
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
			"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
		)
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
