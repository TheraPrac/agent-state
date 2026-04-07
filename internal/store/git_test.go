package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
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
	s.Write(item)

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
	s.Write(item)

	// Will fail on push (no remote) — should return error
	err := s.GitSync("test push")
	if err == nil {
		t.Error("expected push error with no remote")
	}
}

func TestGitSyncScopedStaging(t *testing.T) {
	root, _ := setupTestDir(t)
	// Add a second item
	writeItem(t, filepath.Join(root, "tasks", "T-002-second-task.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Second task
`)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// Modify T-001 through the Store (dirty-tracked)
	item1, _ := s.Get("T-001")
	item1.Doc.SetField("status", "active")
	s.Write(item1)

	// Modify T-002 directly on disk (NOT through Store — simulates parallel process)
	t002Path := filepath.Join(root, "tasks", "T-002-second-task.md")
	os.WriteFile(t002Path, []byte("id: T-002\ntype: task\nstatus: active\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\n\ntitle: CLOBBERED\n"), 0644)

	// GitSync should only stage T-001, not T-002
	err := s.GitSync("scoped update T-001")
	if err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	// Verify T-002 is NOT in the commit (use diff-tree to inspect commit contents only)
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = root
	out, _ := cmd.Output()
	if strings.Contains(string(out), "T-002") {
		t.Errorf("GitSync staged unrelated file T-002; committed files: %s", string(out))
	}
	if !strings.Contains(string(out), "T-001") {
		t.Errorf("GitSync did not stage T-001; committed files: %s", string(out))
	}

	// T-002 should still be dirty on disk (uncommitted)
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, _ = cmd.Output()
	if !strings.Contains(string(out), "T-002") {
		t.Errorf("T-002 should still be uncommitted; status: %s", string(out))
	}
}

func TestGitSyncAllStagesEverything(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}

	s, _ := New(cfg)

	// Modify T-001 through Store
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	s.Write(item)

	// Also write an untracked file directly (simulates changelog, index, etc.)
	os.WriteFile(filepath.Join(root, "tasks", "untracked.md"), []byte("extra"), 0644)

	// GitSyncAll should stage everything
	err := s.GitSyncAll("sync all")
	if err != nil {
		t.Fatalf("GitSyncAll: %v", err)
	}

	// Verify both files are in the commit (use diff-tree for commit-only view)
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = root
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "T-001") {
		t.Errorf("GitSyncAll should include T-001; got: %s", string(out))
	}
	if !strings.Contains(string(out), "untracked.md") {
		t.Errorf("GitSyncAll should include untracked.md; got: %s", string(out))
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

	err := s.Write(item)
	if err == nil {
		t.Error("Write nil doc should error")
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
