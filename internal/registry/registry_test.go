package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEmpty(t *testing.T) {
	r, err := Load("/nonexistent/path.yaml")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if len(r.Epics) != 0 || len(r.Sprints) != 0 || len(r.Notes) != 0 {
		t.Error("expected empty registry")
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	e := r.AddEpic("Billing v1")
	s, err := r.AddSprint(e.ID, "Week 13")
	if err != nil {
		t.Fatalf("AddSprint: %v", err)
	}
	n := r.AddNote("agent-a", "session-123", "Test note")

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify
	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(r2.Epics) != 1 {
		t.Fatalf("expected 1 epic, got %d", len(r2.Epics))
	}
	if r2.Epics[0].ID != e.ID {
		t.Errorf("epic ID: got %q, want %q", r2.Epics[0].ID, e.ID)
	}
	if r2.Epics[0].Title != "Billing v1" {
		t.Errorf("epic title: got %q, want %q", r2.Epics[0].Title, "Billing v1")
	}
	if r2.Epics[0].Status != "active" {
		t.Errorf("epic status: got %q, want %q", r2.Epics[0].Status, "active")
	}

	if len(r2.Sprints) != 1 {
		t.Fatalf("expected 1 sprint, got %d", len(r2.Sprints))
	}
	if r2.Sprints[0].ID != s.ID {
		t.Errorf("sprint ID: got %q, want %q", r2.Sprints[0].ID, s.ID)
	}
	if r2.Sprints[0].Epic != e.ID {
		t.Errorf("sprint epic: got %q, want %q", r2.Sprints[0].Epic, e.ID)
	}

	if len(r2.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(r2.Notes))
	}
	if r2.Notes[0].ID != n.ID {
		t.Errorf("note ID: got %q, want %q", r2.Notes[0].ID, n.ID)
	}
	if r2.Notes[0].Message != "Test note" {
		t.Errorf("note message: got %q, want %q", r2.Notes[0].Message, "Test note")
	}
	if r2.Notes[0].Author != "agent-a" {
		t.Errorf("note author: got %q, want %q", r2.Notes[0].Author, "agent-a")
	}
}

func TestAddEpicGeneratesID(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Test Epic")
	parts := strings.Split(e.ID, "-")
	if len(parts) != 3 {
		t.Errorf("expected 3-part ID, got %d: %q", len(parts), e.ID)
	}
	if e.Status != "active" {
		t.Errorf("expected active status, got %q", e.Status)
	}
}

func TestAddSprintValidatesEpic(t *testing.T) {
	r := &Registry{}
	_, err := r.AddSprint("nonexistent", "Bad Sprint")
	if err == nil {
		t.Error("expected error for nonexistent epic")
	}
}

func TestAddSprintSuccess(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent Epic")
	s, err := r.AddSprint(e.ID, "Good Sprint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Epic != e.ID {
		t.Errorf("sprint epic: got %q, want %q", s.Epic, e.ID)
	}
	parts := strings.Split(s.ID, "-")
	if len(parts) != 3 {
		t.Errorf("expected 3-part sprint ID, got %d: %q", len(parts), s.ID)
	}
}

func TestNoteOperations(t *testing.T) {
	r := &Registry{}
	n := r.AddNote("agent-a", "sess-1", "Original message")

	// Edit
	if err := r.EditNote(n.ID, "Updated message"); err != nil {
		t.Fatalf("EditNote: %v", err)
	}
	if r.Notes[0].Message != "Updated message" {
		t.Errorf("expected updated message, got %q", r.Notes[0].Message)
	}

	// Edit nonexistent
	if err := r.EditNote("nonexistent", "x"); err == nil {
		t.Error("expected error for nonexistent note")
	}

	// Remove
	if err := r.RemoveNote(n.ID); err != nil {
		t.Fatalf("RemoveNote: %v", err)
	}
	if len(r.Notes) != 0 {
		t.Error("expected empty notes after remove")
	}

	// Remove nonexistent
	if err := r.RemoveNote("nonexistent"); err == nil {
		t.Error("expected error for nonexistent note")
	}
}

func TestListSprintsFilterByEpic(t *testing.T) {
	r := &Registry{}
	e1 := r.AddEpic("Epic 1")
	e2 := r.AddEpic("Epic 2")

	r.AddSprint(e1.ID, "Sprint A")
	r.AddSprint(e1.ID, "Sprint B")
	r.AddSprint(e2.ID, "Sprint C")

	all := r.ListSprints("")
	if len(all) != 3 {
		t.Errorf("expected 3 sprints, got %d", len(all))
	}

	e1Sprints := r.ListSprints(e1.ID)
	if len(e1Sprints) != 2 {
		t.Errorf("expected 2 sprints for epic 1, got %d", len(e1Sprints))
	}

	e2Sprints := r.ListSprints(e2.ID)
	if len(e2Sprints) != 1 {
		t.Errorf("expected 1 sprint for epic 2, got %d", len(e2Sprints))
	}
}

func TestListNotesLimit(t *testing.T) {
	r := &Registry{}
	r.AddNote("a", "s", "Note 1")
	r.AddNote("a", "s", "Note 2")
	r.AddNote("a", "s", "Note 3")

	all := r.ListNotes(0)
	if len(all) != 3 {
		t.Errorf("expected 3 notes, got %d", len(all))
	}

	last2 := r.ListNotes(2)
	if len(last2) != 2 {
		t.Errorf("expected 2 notes, got %d", len(last2))
	}
	if last2[0].Message != "Note 2" {
		t.Errorf("expected Note 2 first in limited list, got %q", last2[0].Message)
	}
}

func TestGetEpic(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Find Me")

	found, ok := r.GetEpic(e.ID)
	if !ok {
		t.Fatal("expected to find epic")
	}
	if found.Title != "Find Me" {
		t.Errorf("expected title 'Find Me', got %q", found.Title)
	}

	_, ok = r.GetEpic("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent")
	}
}

func TestGetSprint(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s, _ := r.AddSprint(e.ID, "Child")

	found, ok := r.GetSprint(s.ID)
	if !ok {
		t.Fatal("expected to find sprint")
	}
	if found.Title != "Child" {
		t.Errorf("expected title 'Child', got %q", found.Title)
	}

	_, ok = r.GetSprint("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent")
	}
}

func TestSaveQuotesSpecialChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	r := &Registry{}
	r.AddEpic("Title: with colon")
	r.AddNote("a", "s", "Message with #hash and 'quotes'")

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), `"Title: with colon"`) {
		t.Error("expected quoted title with colon")
	}

	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r2.Epics[0].Title != "Title: with colon" {
		t.Errorf("title roundtrip failed: got %q", r2.Epics[0].Title)
	}
}

func TestUniqueIDsAcrossTypes(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Epic")
	s, _ := r.AddSprint(e.ID, "Sprint")
	n := r.AddNote("a", "s", "Note")

	ids := map[string]bool{e.ID: true, s.ID: true, n.ID: true}
	if len(ids) != 3 {
		t.Errorf("expected 3 unique IDs, got %d (epic=%s sprint=%s note=%s)", len(ids), e.ID, s.ID, n.ID)
	}
}

func TestYamlQuote(t *testing.T) {
	tests := []struct {
		input string
		want  bool // true if should be quoted
	}{
		{"simple", false},
		{"has: colon", true},
		{"has #hash", true},
		{" leading space", true},
		{"trailing space ", true},
		{"normal-hyphenated", false},
	}
	for _, tt := range tests {
		result := yamlQuote(tt.input)
		isQuoted := strings.HasPrefix(result, `"`) && strings.HasSuffix(result, `"`)
		if isQuoted != tt.want {
			t.Errorf("yamlQuote(%q) = %q, quoted=%v, want quoted=%v", tt.input, result, isQuoted, tt.want)
		}
	}
}
