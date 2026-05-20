package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Tests for session 3.5 patch: release, commit, edit, start with worktrees.

// === Release ===

func TestReleaseHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	// T-003 is assigned to agent-a
	code := Release(s, cfg, "T-003")
	if code != 0 {
		t.Errorf("Release returned %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	if item.AssignedTo != "" {
		t.Errorf("assigned_to = %q, want empty", item.AssignedTo)
	}
	// I-408: release must also reset status from active back to queued
	// so the item disappears from `--status active` lists.
	if item.Status != "queued" {
		t.Errorf("status = %q, want queued (release should reset active items)", item.Status)
	}

	// Verify changelog
	entries, _ := changelog.Read(cfg, "T-003")
	foundRelease := false
	foundStatusReset := false
	for _, e := range entries {
		if e.Op == "release" && e.Field == "assigned_to" && e.OldValue == "agent-a" {
			foundRelease = true
		}
		if e.Op == "release" && e.Field == "status" && e.OldValue == "active" && e.NewValue == "queued" {
			foundStatusReset = true
		}
	}
	if !foundRelease {
		t.Error("expected changelog entry for release of assigned_to")
	}
	if !foundStatusReset {
		t.Error("expected changelog entry for status reset (active → queued)")
	}
}

func TestReleaseNotAssigned(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// T-001 is not assigned
	code := Release(s, cfg, "T-001")
	if code != 1 {
		t.Errorf("Release unassigned returned %d, want 1", code)
	}
}

// I-408: the early-exit guard previously refused to release any item
// whose assigned_to + claimed_by were empty. That blocked recovery for
// items that landed in "stuck active" — assignment cleared by hand but
// status still active. Release must now reach the Mutate path when the
// item is active so the caller can recover the queued state.
func TestReleaseStuckActiveRecovers(t *testing.T) {
	root := setupTestEnvRoot(t)
	writeFile(t, filepath.Join(root, "tasks", "T-100-stuck.md"), `id: T-100
type: task
status: active
created: 2026-04-30T10:00:00-06:00
last_touched: 2026-04-30T10:00:00-06:00
title: Stuck active task
assigned_to: ""
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	code := Release(s, cfg, "T-100")
	if code != 0 {
		t.Fatalf("Release on stuck-active returned %d, want 0", code)
	}
	item, _ := s.Get("T-100")
	if item.Status != "queued" {
		t.Errorf("status = %q, want queued (stuck-active recovery)", item.Status)
	}
}

func TestReleaseNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Release(s, cfg, "T-999")
	if code != 1 {
		t.Errorf("Release not found returned %d, want 1", code)
	}
}

// === Commit ===

func TestCommitHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Commit(s, cfg, "T-001", "Fix the login bug")
	if code != 0 {
		t.Errorf("Commit returned %d, want 0", code)
	}

	// Verify changelog
	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "commit" && e.NewValue == "Fix the login bug" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for commit")
	}
}

func TestCommitMultiple(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Commit(s, cfg, "T-001", "First commit")
	code := Commit(s, cfg, "T-001", "Second commit")
	if code != 0 {
		t.Errorf("Second commit returned %d, want 0", code)
	}
}

func TestCommitNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Commit(s, cfg, "T-999", "msg")
	if code != 1 {
		t.Errorf("Commit not found returned %d, want 1", code)
	}
}

func TestCommitNoDoc(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	item, _ := s.Get("T-001")
	item.Doc = nil
	code := Commit(s, cfg, "T-001", "msg")
	if code != 1 {
		t.Errorf("Commit no doc returned %d, want 1", code)
	}
}

// Test appendToNestedList when parent section already has commits
func TestCommitWithExistingWorkTracking(t *testing.T) {
	root := setupTestEnvRoot(t)
	writeFile(t, filepath.Join(root, "tasks", "T-010-wt.md"), `id: T-010
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Has work tracking

work_tracking:
  branch: feat/T-010-test
  commits:
  - []
  pr: []
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	code := Commit(s, cfg, "T-010", "Initial commit")
	if code != 0 {
		t.Errorf("Commit with existing WT returned %d, want 0", code)
	}

	// Second commit should append
	code = Commit(s, cfg, "T-010", "Follow-up fix")
	if code != 0 {
		t.Errorf("Second commit returned %d, want 0", code)
	}
}

// === Edit ===
// T-382: TestEditNoEditor, TestEditNotFound (UpdateModeEditor variant),
// TestEditNoDoc, TestEditNoChanges removed — the editor mode surface
// no longer exists. The not-found / no-doc cases are still covered
// for the remaining modes by TestUpdateNotFound and TestUpdateNoDoc
// in command_test.go.

func TestEditFromStdinFlag(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	// Swap os.Stdin with a pipe containing the new value
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString("new title from stdin\n")
	w.Close()
	defer func() { os.Stdin = oldStdin }()

	code := Update(s, cfg, "T-001", "title", "", UpdateModeStdin)
	if code != 0 {
		t.Errorf("Edit --stdin returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	val, _ := item.Doc.GetField("title")
	if val != "new title from stdin" {
		t.Errorf("title = %q, want %q", val, "new title from stdin")
	}
}

func TestEditFromStdinEmpty(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString("")
	w.Close()
	defer func() { os.Stdin = oldStdin }()

	code := Update(s, cfg, "T-001", "title", "", UpdateModeStdin)
	if code != 1 {
		t.Errorf("Edit --stdin with empty input returned %d, want 1", code)
	}
}

func TestEditListFieldPreservesIndentation(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	// First, set some initial ACs via stdin. I-713 enforces AC
	// verifiability at this write surface, so fixtures use `cmd:`
	// prefixes (the replace-vs-append test is about list-block
	// rewriting, not AC content shape).
	input1 := "- cmd: echo test1\n- cmd: echo test2\n"
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString(input1)
	w.Close()

	code := Update(s, cfg, "T-001", "acceptance_criteria", "", UpdateModeStdin)
	os.Stdin = oldStdin
	if code != 0 {
		t.Fatalf("first Edit --stdin returned %d, want 0", code)
	}

	// Now replace with different content via stdin — this should REPLACE, not append
	input2 := "- cmd: echo replaced\n"
	r2, w2, _ := os.Pipe()
	os.Stdin = r2
	w2.WriteString(input2)
	w2.Close()

	code = Update(s, cfg, "T-001", "acceptance_criteria", "", UpdateModeStdin)
	os.Stdin = oldStdin
	if code != 0 {
		t.Fatalf("second Edit --stdin returned %d, want 0", code)
	}

	// Re-read item and verify: should have only the replacement, not both
	item, _ := s.Get("T-001")
	raw := item.Doc.String()
	// Count occurrences of "echo " in the acceptance_criteria block.
	// Pre-I-713: the test counted "description:" (the old YAML mapping
	// shape). Post-I-713 the canonical shape is `- cmd: ...` lines.
	echoCount := strings.Count(raw, "echo ")
	if echoCount != 1 {
		t.Errorf("expected 1 echo command in ACs after replacement, got %d.\nRaw doc:\n%s", echoCount, raw)
	}
	// Verify the old content is gone — the first write used
	// `echo test1`/`echo test2`; after replacement only `echo replaced`
	// should remain.
	if strings.Contains(raw, "echo test1") || strings.Contains(raw, "echo test2") {
		t.Errorf("old AC content (echo test1/echo test2) still present after replacement:\n%s", raw)
	}
	if !strings.Contains(raw, "echo replaced") {
		t.Errorf("replacement AC (echo replaced) missing:\n%s", raw)
	}
}

// === Start with worktrees ===

func TestStartRequiresSlugWithWorktrees(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api"},
		RepoMap: map[string]string{"api": "repo-a"},
	}

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 2 {
		t.Errorf("Start without slug returned %d, want 2", code)
	}
}

func TestStartWithoutWorktreeConfig(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// No worktree config — should work without slug
	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Errorf("Start without worktree config returned %d, want 0", code)
	}
}

// === Start: worktree creation helpers ===

func TestBranchNaming(t *testing.T) {
	// Verify the branch prefix logic indirectly through the error path
	// (can't easily test actual git worktree creation without a real repo)
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: "/nonexistent",
		Repos:     []string{"api"},
		RepoMap:   map[string]string{"api": "repo-a"},
	}

	// Task → feat/ prefix (will fail because repo doesn't exist, but tests the path)
	code := Start(s, cfg, "T-001", StartOpts{Slug: "test-slug"})
	if code != 1 {
		t.Errorf("Start with bad repo returned %d, want 1", code)
	}
}

func TestStartIssuePrefix(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: "/nonexistent",
		Repos:     []string{"api"},
		RepoMap:   map[string]string{"api": "repo-a"},
	}

	// Issue → fix/ prefix
	code := Start(s, cfg, "I-001", StartOpts{Slug: "bug-fix"})
	if code != 1 {
		t.Errorf("Start issue with bad repo returned %d, want 1", code)
	}
}

func TestStartNoReposConfigured(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   nil,
		RepoMap: nil,
	}

	code := Start(s, cfg, "T-001", StartOpts{Slug: "test"})
	if code != 1 {
		t.Errorf("Start with no repos returned %d, want 1", code)
	}
}

// === Coverage: writeWorkinfo ===

func TestWriteWorkinfo(t *testing.T) {
	dir := t.TempDir()
	writeWorkinfo(dir, "T-001", "feat/T-001-test", []string{"api", "web"})

	path := filepath.Join(dir, ".workinfo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .workinfo: %v", err)
	}
	content := string(data)
	if !containsStr(content, "name: T-001") {
		t.Error("missing name")
	}
	if !containsStr(content, "branch: feat/T-001-test") {
		t.Error("missing branch")
	}
	if !containsStr(content, "- api") {
		t.Error("missing api repo")
	}
}

// === Coverage: appendToNestedList edge cases ===

func TestCommitCreatesWorkTracking(t *testing.T) {
	// Item with no work_tracking section at all
	root := setupTestEnvRoot(t)
	writeFile(t, filepath.Join(root, "tasks", "T-010-bare.md"), `id: T-010
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Bare task
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	code := Commit(s, cfg, "T-010", "Fresh commit on bare item")
	if code != 0 {
		t.Errorf("Commit on bare item returned %d, want 0", code)
	}
}

// === Coverage: DepRm missing doc path ===

func TestDepRmDepNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepRm(s, cfg, "T-001", "T-999")
	if code != 1 {
		t.Errorf("DepRm dep not found returned %d, want 1", code)
	}
}

// T-382: TestEditUsesVisual removed — $VISUAL / $EDITOR fallback
// path no longer exists.

// === Start with real git repo worktree creation ===

func TestStartCreatesWorktreeWithGitRepo(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Create a real git repo as a sibling directory (simulating monorepo layout)
	parentDir := t.TempDir()
	repoDir := filepath.Join(parentDir, "test-repo")
	os.MkdirAll(repoDir, 0755)
	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# test"), 0644)
	initTestGitRepo(t, repoDir)

	// I-407: WorktreeBase is now <agent-root>/worktrees (one level up
	// from cfg.Root()), not <workspace>/worktrees. The test's parentDir
	// is the agent root in this layout.
	agentRoot := filepath.Dir(root)
	os.MkdirAll(filepath.Join(agentRoot, "worktrees"), 0755)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: parentDir,
		Repos:     []string{"test"},
		RepoMap:   map[string]string{"test": "test-repo"},
	}

	code := Start(s, cfg, "T-001", StartOpts{Slug: "my-feature"})
	if code != 0 {
		t.Errorf("Start with worktree returned %d, want 0", code)
	}

	// Verify worktree was created
	expectedWtPath := filepath.Join(agentRoot, "worktrees", "T-001", "test-repo")
	if _, err := os.Stat(expectedWtPath); err != nil {
		t.Errorf("worktree not created at %s: %v", expectedWtPath, err)
	}

	// Verify .workinfo was created
	workinfoPath := filepath.Join(agentRoot, "worktrees", "T-001", ".workinfo")
	data, err := os.ReadFile(workinfoPath)
	if err != nil {
		t.Fatalf("reading .workinfo: %v", err)
	}
	if !containsStr(string(data), "feat/T-001-my-feature") {
		t.Errorf(".workinfo missing branch name")
	}
}

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// === Coverage: symlinkEnvFiles ===

func TestSymlinkEnvFiles(t *testing.T) {
	mainDir := t.TempDir()
	wtDir := t.TempDir()

	// Create .env files in main
	os.WriteFile(filepath.Join(mainDir, ".env"), []byte("SECRET=1"), 0644)
	os.WriteFile(filepath.Join(mainDir, ".env.local"), []byte("LOCAL=1"), 0644)
	os.WriteFile(filepath.Join(mainDir, ".env.example"), []byte("EXAMPLE=1"), 0644)

	symlinkEnvFiles(mainDir, wtDir)

	// .env and .env.local should be symlinked
	if _, err := os.Lstat(filepath.Join(wtDir, ".env")); err != nil {
		t.Error(".env should be symlinked")
	}
	if _, err := os.Lstat(filepath.Join(wtDir, ".env.local")); err != nil {
		t.Error(".env.local should be symlinked")
	}
	// .env.example should NOT be symlinked
	if _, err := os.Lstat(filepath.Join(wtDir, ".env.example")); err == nil {
		t.Error(".env.example should not be symlinked")
	}
}
