package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/registry"
)

// seedSprint creates an epic + sprint in the registry and returns the sprint ID.
func seedSprint(t *testing.T, cfg *config.Config) string {
	t.Helper()
	r := &registry.Registry{}
	e := r.AddEpic("Parent Epic", "")
	sp, err := r.AddSprint(e.ID, "Original Title")
	if err != nil {
		t.Fatalf("AddSprint: %v", err)
	}
	if err := r.Save(cfg.EpicsPath()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return sp.ID
}

func sprintByID(t *testing.T, cfg *config.Config, id string) registry.Sprint {
	t.Helper()
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, sp := range r.Sprints {
		if sp.ID == id {
			return sp
		}
	}
	t.Fatalf("sprint %s not found", id)
	return registry.Sprint{}
}

func TestSprintEditPositional(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "title", Value: "New Title"}}); rc != 0 {
		t.Fatalf("SprintEdit title rc=%d, want 0", rc)
	}
	if got := sprintByID(t, cfg, id).Title; got != "New Title" {
		t.Errorf("title = %q, want %q", got, "New Title")
	}
}

func TestSprintEditBatch(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	rc := SprintEdit(s, cfg, id, []FieldValue{
		{Field: "title", Value: "Batch Title"},
		{Field: "description", Value: "ship the gate"},
	})
	if rc != 0 {
		t.Fatalf("SprintEdit batch rc=%d, want 0", rc)
	}
	sp := sprintByID(t, cfg, id)
	if sp.Title != "Batch Title" {
		t.Errorf("title = %q, want %q", sp.Title, "Batch Title")
	}
	if sp.Description != "ship the gate" {
		t.Errorf("description = %q, want %q", sp.Description, "ship the gate")
	}
}

// TestSprintEditStdin proves the single-field path (as fed by --stdin parsing)
// writes the description, exercising the same SprintEdit entry point the cobra
// --stdin form dispatches to.
func TestSprintEditStdin(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "description", Value: "from stdin buffer"}}); rc != 0 {
		t.Fatalf("SprintEdit description rc=%d, want 0", rc)
	}
	if got := sprintByID(t, cfg, id).Description; got != "from stdin buffer" {
		t.Errorf("description = %q, want %q", got, "from stdin buffer")
	}
}

// TestSprintEditWritesChangelog verifies a per-field changelog entry with
// Op="sprint_edit" lands for the audit trail (Invariant 8).
func TestSprintEditWritesChangelog(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "title", Value: "Logged Title"}}); rc != 0 {
		t.Fatalf("SprintEdit rc=%d, want 0", rc)
	}

	logPath := filepath.Join(cfg.ChangelogDir(), id+".log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading changelog %s: %v", logPath, err)
	}
	body := string(data)
	if !strings.Contains(body, `"op":"sprint_edit"`) {
		t.Errorf("changelog missing sprint_edit op:\n%s", body)
	}
	if !strings.Contains(body, `"new":"Logged Title"`) {
		t.Errorf("changelog missing new value:\n%s", body)
	}
}

func TestSprintEditRejectsNonWhitelistField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	// `epic` is a real Sprint struct field but NOT editable here.
	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "epic", Value: "ep-evil"}}); rc == 0 {
		t.Error("SprintEdit must reject a non-whitelist field (epic)")
	}
	// `items` likewise.
	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "items", Value: "T-001"}}); rc == 0 {
		t.Error("SprintEdit must reject a non-whitelist field (items)")
	}
}

// TestSprintEditRejectsImmutableID proves the immutable id field cannot be
// edited (it is not in the whitelist).
func TestSprintEditRejectsImmutableID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedSprint(t, cfg)

	if rc := SprintEdit(s, cfg, id, []FieldValue{{Field: "id", Value: "sprint-hijack"}}); rc == 0 {
		t.Error("SprintEdit must reject editing the immutable id field")
	}
	// The id must be unchanged on disk.
	if sprintByID(t, cfg, id).ID != id {
		t.Errorf("sprint id changed after rejected edit")
	}
}

func TestSprintEditNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := SprintEdit(s, cfg, "ghost-sprint", []FieldValue{{Field: "title", Value: "x"}}); rc != 1 {
		t.Errorf("SprintEdit on missing sprint rc=%d, want 1", rc)
	}
}
