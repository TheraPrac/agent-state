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

// --- Sprint promotion (T-164) ---

func TestAddSprintAutoSequence(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s1, err := r.AddSprint(e.ID, "Sprint 1")
	if err != nil {
		t.Fatalf("AddSprint: %v", err)
	}
	if s1.Sequence != 1 {
		t.Errorf("expected sequence 1, got %d", s1.Sequence)
	}

	s2, err := r.AddSprint(e.ID, "Sprint 2")
	if err != nil {
		t.Fatalf("AddSprint: %v", err)
	}
	if s2.Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", s2.Sequence)
	}
}

func TestAddSprintAppendsToEpicSprintOrder(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s1, _ := r.AddSprint(e.ID, "Sprint 1")
	s2, _ := r.AddSprint(e.ID, "Sprint 2")

	epic, _ := r.GetEpic(e.ID)
	if len(epic.SprintOrder) != 2 {
		t.Fatalf("expected 2 in sprint_order, got %d", len(epic.SprintOrder))
	}
	if epic.SprintOrder[0] != s1.ID || epic.SprintOrder[1] != s2.ID {
		t.Errorf("sprint_order = %v, want [%s, %s]", epic.SprintOrder, s1.ID, s2.ID)
	}
}

func TestSprintByID(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s, _ := r.AddSprint(e.ID, "Test Sprint")

	found, err := r.SprintByID(s.ID)
	if err != nil {
		t.Fatalf("SprintByID: %v", err)
	}
	if found.Title != "Test Sprint" {
		t.Errorf("title: got %q, want %q", found.Title, "Test Sprint")
	}

	_, err = r.SprintByID("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent sprint")
	}
}

func TestSprintAddItems(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s, _ := r.AddSprint(e.ID, "Sprint")

	// Add items
	err := r.SprintAddItems(s.ID, []string{"T-001", "T-002"})
	if err != nil {
		t.Fatalf("SprintAddItems: %v", err)
	}

	sp, _ := r.SprintByID(s.ID)
	if len(sp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sp.Items))
	}

	// Deduplicate
	err = r.SprintAddItems(s.ID, []string{"T-002", "T-003"})
	if err != nil {
		t.Fatalf("SprintAddItems dedup: %v", err)
	}
	sp, _ = r.SprintByID(s.ID)
	if len(sp.Items) != 3 {
		t.Errorf("expected 3 items after dedup, got %d", len(sp.Items))
	}

	// Bad sprint
	err = r.SprintAddItems("nonexistent", []string{"T-001"})
	if err == nil {
		t.Error("expected error for nonexistent sprint")
	}
}

func TestSprintRemoveItem(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("Parent")
	s, _ := r.AddSprint(e.ID, "Sprint")
	r.SprintAddItems(s.ID, []string{"T-001", "T-002", "T-003"})

	err := r.SprintRemoveItem(s.ID, "T-002")
	if err != nil {
		t.Fatalf("SprintRemoveItem: %v", err)
	}
	sp, _ := r.SprintByID(s.ID)
	if len(sp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sp.Items))
	}

	// Item not in sprint
	err = r.SprintRemoveItem(s.ID, "T-999")
	if err == nil {
		t.Error("expected error for item not in sprint")
	}

	// Bad sprint
	err = r.SprintRemoveItem("nonexistent", "T-001")
	if err == nil {
		t.Error("expected error for nonexistent sprint")
	}
}

func TestSprintsForEpic(t *testing.T) {
	r := &Registry{}
	e1 := r.AddEpic("Epic 1")
	e2 := r.AddEpic("Epic 2")

	r.AddSprint(e1.ID, "Sprint A")
	r.AddSprint(e1.ID, "Sprint B")
	r.AddSprint(e2.ID, "Sprint C")

	sprints := r.SprintsForEpic(e1.ID)
	if len(sprints) != 2 {
		t.Fatalf("expected 2 sprints for epic 1, got %d", len(sprints))
	}
	// Should be ordered by sequence
	if sprints[0].Sequence > sprints[1].Sequence {
		t.Error("sprints not ordered by sequence")
	}

	sprints = r.SprintsForEpic("nonexistent")
	if len(sprints) != 0 {
		t.Errorf("expected 0 sprints for nonexistent epic, got %d", len(sprints))
	}
}

func TestSprintFieldsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	e := r.AddEpic("Epic")
	s, _ := r.AddSprint(e.ID, "Sprint")
	r.SprintAddItems(s.ID, []string{"T-001", "T-002"})

	// Set plan approval
	sp, _ := r.SprintByID(s.ID)
	sp.PlanApproved = true
	sp.PlanApprovedAt = "2026-03-28T10:00:00Z"
	sp.PlanApprovedBy = "user"

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify
	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(r2.Sprints) != 1 {
		t.Fatalf("expected 1 sprint, got %d", len(r2.Sprints))
	}
	sp2 := r2.Sprints[0]
	if len(sp2.Items) != 2 {
		t.Errorf("expected 2 items, got %d", len(sp2.Items))
	}
	if sp2.Items[0] != "T-001" || sp2.Items[1] != "T-002" {
		t.Errorf("items = %v, want [T-001, T-002]", sp2.Items)
	}
	if !sp2.PlanApproved {
		t.Error("expected plan_approved = true")
	}
	if sp2.PlanApprovedAt != "2026-03-28T10:00:00Z" {
		t.Errorf("plan_approved_at = %q", sp2.PlanApprovedAt)
	}
	if sp2.PlanApprovedBy != "user" {
		t.Errorf("plan_approved_by = %q", sp2.PlanApprovedBy)
	}
	if sp2.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", sp2.Sequence)
	}
}

// I-405: sprint description round-trips through Save + Load.
func TestSprintDescriptionRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	e := r.AddEpic("Epic")
	s, _ := r.AddSprint(e.ID, "Sprint")
	for i := range r.Sprints {
		if r.Sprints[i].ID == s.ID {
			r.Sprints[i].Description = "ship the alpha gate by Friday"
		}
	}

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r2.Sprints[0].Description != "ship the alpha gate by Friday" {
		t.Errorf("description = %q, want round-trip preserved", r2.Sprints[0].Description)
	}
}

// I-405: sprints with no description round-trip cleanly — the saver
// must omit the description: line entirely so existing pre-I-405
// fixtures stay byte-identical.
func TestSprintNoDescriptionOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	e := r.AddEpic("Epic")
	r.AddSprint(e.ID, "Sprint")

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "description:") {
		t.Errorf("empty description should not be serialized, got:\n%s", string(body))
	}
}

func TestEpicSprintOrderRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	e := r.AddEpic("Epic")
	s1, _ := r.AddSprint(e.ID, "Sprint 1")
	s2, _ := r.AddSprint(e.ID, "Sprint 2")

	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(r2.Epics[0].SprintOrder) != 2 {
		t.Fatalf("expected 2 in sprint_order, got %d", len(r2.Epics[0].SprintOrder))
	}
	if r2.Epics[0].SprintOrder[0] != s1.ID || r2.Epics[0].SprintOrder[1] != s2.ID {
		t.Errorf("sprint_order = %v", r2.Epics[0].SprintOrder)
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

// I-489: Epic.Priority round-trips through Save/Load and ListEpics
// orders prioritized epics ahead of unprioritized ones.
func TestEpicPriorityRoundTripAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epics.yaml")

	r := &Registry{}
	a := r.AddEpic("alpha-go-live")
	b := r.AddEpic("billing-v2")
	c := r.AddEpic("unprioritized")
	prio := 1
	r.Epics[indexOfEpic(r, b.ID)].Priority = &prio
	prio2 := 2
	r.Epics[indexOfEpic(r, a.ID)].Priority = &prio2
	_ = c // stays nil

	if err := r.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	r2, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := r2.ListEpics()
	if len(got) != 3 {
		t.Fatalf("got %d epics, want 3", len(got))
	}
	if got[0].ID != b.ID || got[1].ID != a.ID {
		t.Errorf("priority order broken: got %s, %s; want %s (p1), %s (p2)",
			got[0].ID, got[1].ID, b.ID, a.ID)
	}
	if got[2].ID != c.ID || got[2].Priority != nil {
		t.Errorf("unprioritized epic should sort last with nil priority; got %+v", got[2])
	}
}

func TestMoveEpicRenumbers(t *testing.T) {
	r := &Registry{}
	a := r.AddEpic("a")
	b := r.AddEpic("b")
	c := r.AddEpic("c")

	// Initial state: all unprioritized.
	if err := r.MoveEpic(a.ID, 1); err != nil {
		t.Fatalf("move a: %v", err)
	}
	if r.Epics[indexOfEpic(r, a.ID)].Priority == nil || *r.Epics[indexOfEpic(r, a.ID)].Priority != 1 {
		t.Errorf("a.Priority not set to 1")
	}
	// Other epics should remain unprioritized.
	for _, id := range []string{b.ID, c.ID} {
		if r.Epics[indexOfEpic(r, id)].Priority != nil {
			t.Errorf("%s should remain unprioritized after first move", id)
		}
	}

	// Move c to position 1; a should shift to 2.
	if err := r.MoveEpic(c.ID, 1); err != nil {
		t.Fatalf("move c: %v", err)
	}
	if got := *r.Epics[indexOfEpic(r, c.ID)].Priority; got != 1 {
		t.Errorf("c.Priority = %d, want 1", got)
	}
	if got := *r.Epics[indexOfEpic(r, a.ID)].Priority; got != 2 {
		t.Errorf("a.Priority = %d, want 2 (shifted by c's insert)", got)
	}
	if r.Epics[indexOfEpic(r, b.ID)].Priority != nil {
		t.Errorf("b should still be unprioritized — it was never moved")
	}

	// Out-of-range pos clamps to end.
	if err := r.MoveEpic(b.ID, 100); err != nil {
		t.Fatalf("move b: %v", err)
	}
	if got := *r.Epics[indexOfEpic(r, b.ID)].Priority; got != 3 {
		t.Errorf("b.Priority = %d, want 3 (clamped to end)", got)
	}
}

func TestMoveEpicNotFound(t *testing.T) {
	r := &Registry{}
	if err := r.MoveEpic("ghost", 1); err == nil {
		t.Error("expected error for missing epic")
	}
}

func TestMoveEpicRejectsZero(t *testing.T) {
	r := &Registry{}
	a := r.AddEpic("a")
	if err := r.MoveEpic(a.ID, 0); err == nil {
		t.Error("expected error for pos=0")
	}
}

// I-489: MoveSprint renumbers within the parent epic only.
func TestMoveSprintRenumbersWithinEpic(t *testing.T) {
	r := &Registry{}
	e1 := r.AddEpic("e1")
	e2 := r.AddEpic("e2")
	s1, _ := r.AddSprint(e1.ID, "s1") // Sequence 1
	s2, _ := r.AddSprint(e1.ID, "s2") // Sequence 2
	s3, _ := r.AddSprint(e1.ID, "s3") // Sequence 3
	other, _ := r.AddSprint(e2.ID, "other")

	if err := r.MoveSprint(s3.ID, 1); err != nil {
		t.Fatalf("move s3: %v", err)
	}

	// After move: s3=1, s1=2, s2=3 within e1; e2's sprint untouched.
	if got, _ := r.GetSprint(s3.ID); got.Sequence != 1 {
		t.Errorf("s3.Sequence = %d, want 1", got.Sequence)
	}
	if got, _ := r.GetSprint(s1.ID); got.Sequence != 2 {
		t.Errorf("s1.Sequence = %d, want 2", got.Sequence)
	}
	if got, _ := r.GetSprint(s2.ID); got.Sequence != 3 {
		t.Errorf("s2.Sequence = %d, want 3", got.Sequence)
	}
	if got, _ := r.GetSprint(other.ID); got.Sequence != 1 {
		t.Errorf("e2's sprint should remain Sequence 1, got %d", got.Sequence)
	}

	// Epic SprintOrder should reflect the new order.
	for _, ep := range r.ListEpics() {
		if ep.ID != e1.ID {
			continue
		}
		want := []string{s3.ID, s1.ID, s2.ID}
		if len(ep.SprintOrder) != len(want) {
			t.Fatalf("SprintOrder len = %d, want %d", len(ep.SprintOrder), len(want))
		}
		for i, id := range want {
			if ep.SprintOrder[i] != id {
				t.Errorf("SprintOrder[%d] = %s, want %s", i, ep.SprintOrder[i], id)
			}
		}
	}
}

func TestMoveSprintNotFound(t *testing.T) {
	r := &Registry{}
	if err := r.MoveSprint("ghost", 1); err == nil {
		t.Error("expected error for missing sprint")
	}
}

// I-489: ListSprints sorts by Sequence (was previously unsorted, while
// SprintsForEpic was sorted — this test pins down the consistency fix).
func TestListSprintsSortedBySequence(t *testing.T) {
	r := &Registry{}
	e := r.AddEpic("e")
	s1, _ := r.AddSprint(e.ID, "s1")
	s2, _ := r.AddSprint(e.ID, "s2")
	s3, _ := r.AddSprint(e.ID, "s3")

	// Reorder via MoveSprint then check both methods agree.
	if err := r.MoveSprint(s3.ID, 1); err != nil {
		t.Fatalf("move: %v", err)
	}
	listed := r.ListSprints(e.ID)
	forEpic := r.SprintsForEpic(e.ID)
	if len(listed) != len(forEpic) {
		t.Fatalf("listed=%d, forEpic=%d", len(listed), len(forEpic))
	}
	for i := range listed {
		if listed[i].ID != forEpic[i].ID {
			t.Errorf("ListSprints[%d]=%s, SprintsForEpic[%d]=%s — should match",
				i, listed[i].ID, i, forEpic[i].ID)
		}
	}
	// Sanity: s3 is first.
	if listed[0].ID != s3.ID {
		t.Errorf("listed[0]=%s, want s3=%s", listed[0].ID, s3.ID)
	}
	_ = s1
	_ = s2
}

// indexOfEpic finds an epic by ID and returns its slice index, for
// tests that need to mutate Priority directly.
func indexOfEpic(r *Registry, id string) int {
	for i, e := range r.Epics {
		if e.ID == id {
			return i
		}
	}
	return -1
}
