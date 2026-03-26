package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// --- Epic/Note error paths (targeting uncovered Load/Save/operation error blocks) ---

func corruptRegistryEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	// Make registry files unreadable (triggers os.Open permission error)
	os.WriteFile(filepath.Join(root, ".as", "epics.yaml"), []byte(""), 0000)
	os.WriteFile(filepath.Join(root, ".as", "notes.yaml"), []byte(""), 0000)
	t.Cleanup(func() {
		os.Chmod(filepath.Join(root, ".as", "epics.yaml"), 0644)
		os.Chmod(filepath.Join(root, ".as", "notes.yaml"), 0644)
	})

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg
}

func TestEpicCreateLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := EpicCreate(cfg, "Test Epic")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestEpicCreateSaveErr(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, ".as", "epics.yaml"), []byte(""), 0644)
	cfg, _ := config.Load(root)
	// Make file unwritable after config loads
	os.Chmod(filepath.Join(root, ".as", "epics.yaml"), 0444)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, ".as", "epics.yaml"), 0644) })
	code := EpicCreate(cfg, "Test Epic")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestEpicListLoadErr(t *testing.T) {
	s, cfg := corruptRegistryEnv(t)
	code := EpicList(s, cfg)
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestSprintCreateLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := SprintCreate(cfg, "epic-id", "Sprint 1")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestSprintCreateAddErr(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, ".as", "epics.yaml"), []byte(""), 0644)
	cfg, _ := config.Load(root)
	code := SprintCreate(cfg, "nonexistent-epic", "Sprint 1")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestSprintListLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := SprintList(cfg, "")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteAddSaveErr(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, ".as", "notes.yaml"), []byte(""), 0644)
	cfg, _ := config.Load(root)
	// Make file unwritable after config loads
	os.Chmod(filepath.Join(root, ".as", "notes.yaml"), 0444)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, ".as", "notes.yaml"), 0644) })
	code := NoteAdd(cfg, "test note")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteListLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := NoteList(cfg, 10)
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteEditLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := NoteEdit(cfg, "1", "updated")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteEditBadIDErr(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, ".as", "notes.yaml"), []byte(""), 0644)
	cfg, _ := config.Load(root)
	code := NoteEdit(cfg, "999", "updated")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteRmLoadErr(t *testing.T) {
	_, cfg := corruptRegistryEnv(t)
	code := NoteRm(cfg, "1")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

func TestNoteRmBadIDErr(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, ".as", "notes.yaml"), []byte(""), 0644)
	cfg, _ := config.Load(root)
	code := NoteRm(cfg, "999")
	if code != 1 {
		t.Errorf("expected 1, got %d", code)
	}
}

// --- dep_mutate.go write+remove paths ---

func TestDepAddAndRemove(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepAdd(s, cfg, "T-001", "T-002")
	if code != 0 {
		t.Errorf("DepAdd expected 0, got %d", code)
	}
	code = DepRm(s, cfg, "T-001", "T-002")
	if code != 0 {
		t.Errorf("DepRm expected 0, got %d", code)
	}
}

// --- check.go delivery gate pass path ---

func TestCheckDeliveryGatePass(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .
delivery:
  stages: [coding, committed, pushed, merged, uat_approved, closed]
  archive_gate: uat_approved
`), 0644)
	writeFile(t, filepath.Join(root, "archive", "T-001-done.md"), `id: T-001
type: task
status: completed
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Done task
delivery:
  stage: uat_approved
`)
	writeFile(t, filepath.Join(root, "index.md"), "# Index\n")
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	gitCallCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		gitCallCount++
		if gitCallCount%2 == 1 {
			return []byte(""), nil
		}
		return []byte("0\n"), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	code := Check(s, cfg, false, false)
	if code != 0 {
		t.Errorf("expected 0, got %d", code)
	}
}
