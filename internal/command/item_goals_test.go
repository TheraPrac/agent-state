package command

import (
	"testing"
)

func TestItemGoalsAddAppends(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"})
	if rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d, want 0", rc)
	}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after add")
	}
	if len(item.Goals) != 1 || item.Goals[0] != "G-001" {
		t.Errorf("Goals = %v, want [G-001]", item.Goals)
	}
}

func TestItemGoalsAddRejectsUnknownGoal(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-999"})
	if rc == 0 {
		t.Fatal("expected non-zero rc for unknown goal")
	}
}

func TestItemGoalsAddRejectsNonGoalTarget(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	// T-002 is type=task, not type=goal
	rc := ItemGoalsAdd(s, cfg, "T-001", []string{"T-002"})
	if rc == 0 {
		t.Fatal("expected non-zero rc when goal ID points to a non-goal item")
	}
}

func TestItemGoalsAddRejectsDuplicate(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// First add succeeds.
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("first add rc=%d, want 0", rc)
	}

	// Second add of same goal must fail.
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc == 0 {
		t.Fatal("expected non-zero rc for duplicate goal add")
	}
}

func TestItemGoalsAddRejectsDuplicateWithinRequest(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// Same goal ID twice in the same request.
	rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001", "G-001"})
	if rc == 0 {
		t.Fatal("expected non-zero rc for intra-request duplicate")
	}
}

func TestItemGoalsRemoveDeletes(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// Add first.
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("add rc=%d, want 0", rc)
	}

	// Then remove.
	rc := ItemGoalsRemove(s, cfg, "T-001", []string{"G-001"})
	if rc != 0 {
		t.Fatalf("ItemGoalsRemove rc=%d, want 0", rc)
	}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after remove")
	}
	if len(item.Goals) != 0 {
		t.Errorf("Goals = %v after remove, want []", item.Goals)
	}
}

func TestItemGoalsRemoveRejectsAbsent(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	// Remove a goal that was never added.
	rc := ItemGoalsRemove(s, cfg, "T-001", []string{"G-001"})
	if rc == 0 {
		t.Fatal("expected non-zero rc when removing a goal not in item.Goals")
	}
}
