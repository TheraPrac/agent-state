package command

import (
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/registry"
)

// seedEpic creates an epic in the registry and returns its ID.
func seedEpic(t *testing.T, cfg *config.Config) string {
	t.Helper()
	r := &registry.Registry{}
	e := r.AddEpic("Original Epic", "")
	if err := r.Save(cfg.EpicsPath()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return e.ID
}

func epicByID(t *testing.T, cfg *config.Config, id string) registry.Epic {
	t.Helper()
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range r.Epics {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("epic %s not found", id)
	return registry.Epic{}
}

func TestEpicEditTitle(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedEpic(t, cfg)

	if rc := EpicEdit(s, cfg, id, []FieldValue{{Field: "title", Value: "Renamed Epic"}}); rc != 0 {
		t.Fatalf("EpicEdit title rc=%d, want 0", rc)
	}
	if got := epicByID(t, cfg, id).Title; got != "Renamed Epic" {
		t.Errorf("title = %q, want %q", got, "Renamed Epic")
	}
}

func TestEpicEditRejectsNonWhitelistField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	id := seedEpic(t, cfg)

	// Epic has no description field, and status/goal/order are managed by
	// dedicated commands — all must be rejected.
	for _, field := range []string{"description", "status", "goal", "id"} {
		if rc := EpicEdit(s, cfg, id, []FieldValue{{Field: field, Value: "x"}}); rc == 0 {
			t.Errorf("EpicEdit must reject non-whitelist field %q", field)
		}
	}
	// Title must be unchanged after the rejected edits.
	if got := epicByID(t, cfg, id).Title; got != "Original Epic" {
		t.Errorf("title = %q after rejected edits, want unchanged", got)
	}
}

func TestEpicEditNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := EpicEdit(s, cfg, "ghost-epic", []FieldValue{{Field: "title", Value: "x"}}); rc != 1 {
		t.Errorf("EpicEdit on missing epic rc=%d, want 1", rc)
	}
}
