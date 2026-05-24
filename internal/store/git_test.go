package store

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	// Modify the tracked non-state file → tracked-modified entry.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified\n"), 0755)

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

	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified-on-feature\n"), 0755)

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
	_, asDir := setupI807Workspace(t, false)

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
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho dirty\n"), 0755)

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
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho feature-with-allow-main\n"), 0755)

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
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho sentinel\n"), 0755)

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

	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho detached\n"), 0755)

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

	// New non-state edit on the detached HEAD.
	os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho detached-edit\n"), 0755)

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
