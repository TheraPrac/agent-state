package command

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/store"
)


func TestUpdateGoalsValidatesAgainstRegistry(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// Valid goal — must succeed.
	if rc := Update(s, cfg, "T-001", "goals", "G-001", UpdateModeValue); rc != 0 {
		t.Errorf("Update goals=G-001 rc=%d, want 0", rc)
	}

	// Non-existent goal — must reject.
	if rc := Update(s, cfg, "T-001", "goals", "G-999", UpdateModeValue); rc == 0 {
		t.Error("Update goals=G-999 (not found) should fail")
	}

	// Non-goal item type — must reject.
	if rc := Update(s, cfg, "T-001", "goals", "T-001", UpdateModeValue); rc == 0 {
		t.Error("Update goals=T-001 (type=task, not goal) should fail")
	}
}

func TestCreateAcceptsGoalsFlag(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s := reloadStoreGoal(t, cfg)

	rc := Create(s, cfg, "task", "Task with goal", CreateOpts{
		Priority: 2,
		Goals:    []string{"G-001"},
	})
	if rc != 0 {
		t.Fatalf("Create with Goals rc=%d, want 0", rc)
	}

	// Reload and verify Goals persisted.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var found bool
	for _, item := range s2.All() {
		if item.Title == "Task with goal" {
			found = true
			if len(item.Goals) != 1 || item.Goals[0] != "G-001" {
				t.Errorf("Goals = %v, want [G-001]", item.Goals)
			}
			break
		}
	}
	if !found {
		t.Error("created item not found in store")
	}
}

func TestCreateRejectsUnknownGoal(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	s := reloadStoreGoal(t, cfg)

	rc := Create(s, cfg, "task", "Task with bad goal", CreateOpts{
		Priority: 2,
		Goals:    []string{"G-999"},
	})
	if rc == 0 {
		t.Fatal("Create with unknown goal should fail")
	}
}

func TestListFilterByGoal(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	// Associate T-001 with G-001 only.
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}

	// Verify via store filter directly — GoalFilter is what List uses.
	items := s.List(store.GoalFilter("G-001"))
	if len(items) != 1 {
		t.Fatalf("GoalFilter(G-001) returned %d items, want 1", len(items))
	}
	if items[0].ID != "T-001" {
		t.Errorf("GoalFilter(G-001) returned %s, want T-001", items[0].ID)
	}

	// T-002 must not be in the result.
	for _, it := range items {
		if it.ID == "T-002" {
			t.Error("T-002 should not be in G-001 filter results")
		}
	}
}

func TestShowRendersItemGoals(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}

	// Verify goals: field is in the file after add.
	path, ok := s.Path("T-001")
	if !ok {
		t.Fatal("T-001 path not found")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read T-001: %v", err)
	}
	if !strings.Contains(string(data), "goals:") {
		t.Errorf("goals: field missing from file after ItemGoalsAdd:\n%s", string(data))
	}
	if !strings.Contains(string(data), "G-001") {
		t.Errorf("G-001 missing from goals field:\n%s", string(data))
	}

	// Verify showDefaultTo renders the goals line.
	item, _ := s.Get("T-001")
	var buf bytes.Buffer
	showDefaultTo(&buf, s, cfg, "T-001", item)
	out := buf.String()
	if !strings.Contains(out, "goals:") {
		t.Errorf("showDefaultTo did not render goals line:\n%s", out)
	}
	if !strings.Contains(out, "G-001") {
		t.Errorf("showDefaultTo goals line missing G-001:\n%s", out)
	}
}
