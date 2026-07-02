package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
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
	// I-433: terminal statuses are now done/abandoned/archived for both
	// types under the unified vocabulary.
	for _, item := range items {
		if item.Status == "done" || item.Status == "archived" || item.Status == "abandoned" {
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

	if err := s.write(item); err != nil {
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
	newPath, err := s.Move("T-001")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if oldPath != newPath {
		t.Errorf("path changed: %s -> %s", oldPath, newPath)
	}
	if statPath, _ := s.Path("T-001"); statPath != newPath {
		t.Errorf("Move's returned path %q diverges from s.Path %q", newPath, statPath)
	}
}

func TestMoveNotFound(t *testing.T) {
	root, _ := setupTestDir(t)
	s := newTestStore(t, root)

	if _, err := s.Move("T-999"); err == nil {
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

func TestNonTerminalFilterUnknownType(t *testing.T) {
	cfg := config.Defaults()
	f := NonTerminalFilter(cfg)
	item := &model.Item{Type: "nonexistent", Status: "queued"}
	// Unknown type → should return true (keep)
	if !f(item) {
		t.Error("unknown type should pass filter")
	}
}

func TestNonTerminalFilterUnknownStatus(t *testing.T) {
	cfg := config.Defaults()
	f := NonTerminalFilter(cfg)
	item := &model.Item{Type: "task", Status: "fictional_status"}
	// Status not in statuses list → should return true
	if !f(item) {
		t.Error("unknown status should pass filter")
	}
}

func TestMoveNoPath(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, _ := config.Load(root)
	s, _ := New(cfg)

	// Inject an item with no path
	s.items["T-999"] = &model.Item{ID: "T-999", Type: "task", Status: "queued"}
	// Don't set a path — s.paths["T-999"] is empty

	_, err := s.Move("T-999")
	if err == nil {
		t.Error("Move with no path should fail")
	}
}

func TestWriteNoDirectoryMapping(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, _ := config.Load(root)
	s, _ := New(cfg)

	// Item with unmapped type/status and no existing path
	item := &model.Item{
		ID:     "X-001",
		Type:   "nonexistent",
		Status: "unknown",
		Doc:    model.NewParsedDocument(),
	}
	item.Doc.SetField("id", "X-001")
	item.Doc.SetField("type", "nonexistent")

	err := s.write(item)
	if err == nil {
		t.Error("Write for unmapped type should fail")
	}
}

func TestScanWithBadFile(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "tasks"), 0755)
	os.MkdirAll(filepath.Join(root, ".as"), 0755)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Create a valid item
	os.WriteFile(filepath.Join(root, "tasks", "T-001-test.md"), []byte("id: T-001\ntype: task\nstatus: queued\ntitle: test\n"), 0644)

	cfg, _ := config.Load(root)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(s.All()) != 1 {
		t.Errorf("expected 1 item, got %d", len(s.All()))
	}
}
