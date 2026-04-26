package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func setupTestDir(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()

	// Create directory structure
	for _, dir := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	// Create test items
	writeItem(t, filepath.Join(root, "tasks", "T-001-first-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: First task

depends_on:
- []
`)

	writeItem(t, filepath.Join(root, "tasks", "T-002-second-task.md"), `id: T-002
type: task
status: active
created: 2026-03-25T11:00:00-06:00
last_touched: 2026-03-25T11:00:00-06:00

title: Second task
assigned_to: agent-a

depends_on:
- T-001
`)

	writeItem(t, filepath.Join(root, "issues", "I-001-a-bug.md"), `id: I-001
type: issue
status: open
created: 2026-03-25T12:00:00-06:00
last_touched: 2026-03-25T12:00:00-06:00

title: A bug
severity: high
`)

	writeItem(t, filepath.Join(root, "archive", "T-003-done-task.md"), `id: T-003
type: task
status: completed
created: 2026-03-20T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Done task
`)

	cfg := config.Defaults()
	cfg.Paths.Root = root // override to use our temp dir

	// Hack: set the root directly
	return root, cfg
}

func writeItem(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	cfg := config.Defaults()

	// Create a config that points to our test root
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := "paths:\n  root: .\n"
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

func TestScanFindsAllItems(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	if len(s.items) != 4 {
		t.Errorf("found %d items, want 4", len(s.items))
		for id := range s.items {
			t.Logf("  found: %s", id)
		}
	}
}

func TestGet(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found")
	}
	if item.Title != "First task" {
		t.Errorf("title = %q, want %q", item.Title, "First task")
	}

	_, ok = s.Get("T-999")
	if ok {
		t.Error("T-999 should not exist")
	}
}

func TestListWithFilters(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	// All items
	all := s.List()
	if len(all) != 4 {
		t.Errorf("List() = %d items, want 4", len(all))
	}

	// Filter by type
	tasks := s.List(TypeFilter("task"))
	if len(tasks) != 3 {
		t.Errorf("TypeFilter(task) = %d items, want 3", len(tasks))
	}

	issues := s.List(TypeFilter("issue"))
	if len(issues) != 1 {
		t.Errorf("TypeFilter(issue) = %d items, want 1", len(issues))
	}

	// Filter by status
	queued := s.List(StatusFilter("queued"))
	if len(queued) != 1 {
		t.Errorf("StatusFilter(queued) = %d items, want 1", len(queued))
	}

	// Filter by assignment
	agentA := s.List(AssignedFilter("agent-a"))
	if len(agentA) != 1 {
		t.Errorf("AssignedFilter(agent-a) = %d items, want 1", len(agentA))
	}

	// Combined filters
	activeTasks := s.List(TypeFilter("task"), StatusFilter("active"))
	if len(activeTasks) != 1 {
		t.Errorf("task+active = %d items, want 1", len(activeTasks))
	}
}

func TestNextID(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	id, err := s.NextID("task")
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	if id != "T-004" {
		t.Errorf("NextID = %q, want T-004", id)
	}

	id, err = s.NextID("issue")
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	if id != "I-002" {
		t.Errorf("NextID = %q, want I-002", id)
	}

	_, err = s.NextID("banana")
	if err == nil {
		t.Error("NextID(banana) should fail")
	}
}

func TestWrite(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "active")
	item.Status = "active"

	if err := s.write(item); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Re-read and verify
	content, _ := os.ReadFile(s.paths["T-001"])
	if !contains(string(content), "status: active") {
		t.Error("written file should contain 'status: active'")
	}
	if !contains(string(content), "last_touched:") {
		t.Error("written file should have updated last_touched")
	}
}

func TestMove(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	// Change T-001 to completed, then move
	item, _ := s.Get("T-001")
	item.Status = "completed"
	item.Doc.SetField("status", "completed")

	if err := s.write(item); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := s.Move("T-001"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	newPath := s.paths["T-001"]
	if !contains(newPath, "archive") {
		t.Errorf("moved path %q should be in archive/", newPath)
	}

	// Verify file exists at new location
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("file should exist at %s: %v", newPath, err)
	}
}

func TestFilenameGeneration(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	item := &model.Item{
		ID:    "T-099",
		Title: "Fix the CSRF bug — important!",
	}

	got := s.filenameForItem(item)
	if got != "T-099-fix-the-csrf-bug-important.md" {
		t.Errorf("filename = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsStr(s, sub)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
