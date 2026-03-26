package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

func TestPathAndAll(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	path, ok := s.Path("T-001")
	if !ok {
		t.Fatal("T-001 path not found")
	}
	if path == "" {
		t.Error("path should not be empty")
	}

	all := s.All()
	if len(all) != 4 {
		t.Errorf("All() = %d, want 4", len(all))
	}
}

func TestTagFilter(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeItem(t, filepath.Join(root, "tasks", "T-001-tagged.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Tagged task

tags:
- security
- alpha
`)

	writeItem(t, filepath.Join(root, "tasks", "T-002-untagged.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Untagged task
`)

	cfg, _ := config.Load(root)
	s, _ := New(cfg)

	tagged := s.List(TagFilter("security"))
	if len(tagged) != 1 {
		t.Errorf("TagFilter(security) = %d, want 1", len(tagged))
	}

	none := s.List(TagFilter("nonexistent"))
	if len(none) != 0 {
		t.Errorf("TagFilter(nonexistent) = %d, want 0", len(none))
	}
}

func TestNonTerminalFilter(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)
	cfg, _ := config.Load(root)

	items := s.List(NonTerminalFilter(cfg))
	// Should exclude T-004 (completed) and include T-001 (queued), T-002 (active), T-003 (active), I-001 (open)
	for _, item := range items {
		if item.Status == "completed" || item.Status == "archived" {
			t.Errorf("NonTerminalFilter included %s with status %s", item.ID, item.Status)
		}
	}
}

func TestWriteNewItem(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	// Create a new item via store.Write (no existing path)
	item := &model.Item{
		ID:     "T-099",
		Type:   "task",
		Status: "queued",
		Title:  "Brand new",
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "id: T-099", Key: "id", Value: "T-099"},
				{Raw: "type: task", Key: "type", Value: "task"},
				{Raw: "status: queued", Key: "status", Value: "queued"},
				{Raw: "title: Brand new", Key: "title", Value: "Brand new"},
			},
		},
		WorkTracking:    make(map[string]interface{}),
		Delivery:        make(map[string]interface{}),
		TestingEvidence: make(map[string]interface{}),
		TimeTracking:    make(map[string]interface{}),
		Manifest:        make(map[string]interface{}),
	}

	if err := s.Write(item); err != nil {
		t.Fatalf("Write new item: %v", err)
	}

	// Verify file exists
	path, ok := s.Path("T-099")
	if !ok {
		t.Fatal("T-099 path not found after write")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not found at %s: %v", path, err)
	}
}

func TestMoveNoOp(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	// T-001 is queued in tasks/ — move should be a no-op
	oldPath, _ := s.Path("T-001")
	if err := s.Move("T-001"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	newPath, _ := s.Path("T-001")
	if oldPath != newPath {
		t.Errorf("path changed: %s -> %s", oldPath, newPath)
	}
}

func TestMoveNotFound(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	if err := s.Move("T-999"); err == nil {
		t.Error("Move nonexistent should fail")
	}
}

func TestScanEmptyDir(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, _ := config.Load(root)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(s.All()) != 0 {
		t.Errorf("expected 0 items, got %d", len(s.All()))
	}
}

func TestNextIDEmptyStore(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, _ := config.Load(root)
	s, _ := New(cfg)

	id, err := s.NextID("task")
	if err != nil {
		t.Fatalf("NextID: %v", err)
	}
	if id != "T-001" {
		t.Errorf("NextID = %q, want T-001", id)
	}
}
