package command

import (
	"os"
	"path/filepath"
	"testing"
)

// Additional tests for coverage targets.

func TestShowField(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, []string{"--field", "status", "T-001"})
	if code != 0 {
		t.Errorf("Show --field returned %d, want 0", code)
	}
}

func TestShowFieldMissing(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, []string{"--field", "nonexistent", "T-001"})
	if code != 1 {
		t.Errorf("Show --field nonexistent returned %d, want 1", code)
	}
}

func TestListByStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, []string{"--status", "active"})
	if code != 0 {
		t.Errorf("List --status active returned %d, want 0", code)
	}
}

func TestListByAssigned(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, []string{"--assigned", "agent-a"})
	if code != 0 {
		t.Errorf("List --assigned returned %d, want 0", code)
	}
}

func TestCheckFull(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Run check without --quiet to exercise output paths
	code := Check(s, cfg, []string{})
	_ = code // just verify no crash
}

func TestReadyWithFilters(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, []string{"--type", "task"})
	if code != 0 {
		t.Errorf("Ready --type task returned %d, want 0", code)
	}
}

func TestReadyWithTag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, []string{"--tag", "nonexistent"})
	if code != 0 {
		t.Errorf("Ready --tag returned %d, want 0", code)
	}
}

func TestReadyWithLimit(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, []string{"--limit", "1"})
	if code != 0 {
		t.Errorf("Ready --limit returned %d, want 0", code)
	}
}

func TestCreateWithOptions(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, []string{"task", "Tagged task", "--tag", "security", "--priority", "1", "--depends", "T-001"})
	if code != 0 {
		t.Errorf("Create with options returned %d, want 0", code)
	}
}

func TestCreateIssue(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, []string{"issue", "New bug"})
	if code != 0 {
		t.Errorf("Create issue returned %d, want 0", code)
	}
}

func TestStartNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, []string{"T-999"})
	if code != 1 {
		t.Errorf("Start nonexistent returned %d, want 1", code)
	}
}

func TestStartNoArgs(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, []string{})
	if code != 2 {
		t.Errorf("Start no args returned %d, want 2", code)
	}
}

func TestCloseNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, []string{"T-999", "completed"})
	if code != 1 {
		t.Errorf("Close nonexistent returned %d, want 1", code)
	}
}

func TestCloseNoArgs(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, []string{"T-003"})
	if code != 2 {
		t.Errorf("Close no resolution returned %d, want 2", code)
	}
}

func TestCloseFromQueued(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Closing queued item as abandoned — flags must come before positional args
	code := Close(s, cfg, []string{"--reason", "not needed", "T-001", "abandoned"})
	if code != 0 {
		t.Errorf("Close queued as abandoned returned %d, want 0", code)
	}
}

func TestUpdateStdin(t *testing.T) {
	s, _ := setupTestEnv(t)
	// Create a pipe to simulate stdin
	r, w, _ := os.Pipe()
	w.WriteString("multiline\ncontent")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	code := Update(s, []string{"T-001", "summary", "--stdin"})
	if code != 0 {
		t.Errorf("Update --stdin returned %d, want 0", code)
	}
}

func TestIndex(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Create index.md path
	indexPath := cfg.IndexPath()
	os.MkdirAll(filepath.Dir(indexPath), 0755)

	code := Index(s, cfg, []string{})
	if code != 0 {
		t.Errorf("Index returned %d, want 0", code)
	}

	// Verify file was created
	if _, err := os.Stat(indexPath); err != nil {
		t.Errorf("index.md not created: %v", err)
	}
}
