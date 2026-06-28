package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// TestUpdateFieldExitsNonZeroOnGateRefusal verifies that Update returns non-zero
// when the I-807 gate fires, AND that the disk mutation is preserved (mutation
// correctness is independent of git-sync outcome).
func TestUpdateFieldExitsNonZeroOnGateRefusal(t *testing.T) {
	workspace, s, cfg := setupGateWorkspace(t)

	// Modify and stage the tracked non-state file to arm the gate.
	// I-1472: gate fires only on staged (index-dirty) entries.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho gate-armed\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "claude-config/hooks/foo.sh").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	code := Update(s, cfg, "T-001", "title", "Gate test updated title", UpdateModeValue)

	if code == 0 {
		t.Errorf("Update must return non-zero when the I-807 gate fires; got 0")
	}

	// The disk mutation must be preserved despite the gate refusal.
	// Check via Doc.GetField since item.Title is set at parse time, not from
	// Doc mutations.
	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after Update")
	}
	if item.Doc == nil {
		t.Fatal("T-001 has no Doc after Update")
	}
	got, _ := item.Doc.GetField("title")
	if got != "Gate test updated title" {
		t.Errorf("disk mutation must be preserved on gate refusal; title = %q, want %q",
			got, "Gate test updated title")
	}
}

// --- I-1599: active-goal weight-sum guard on the st update paths ---

// reloadGoalWeight returns the persisted weight for a goal, reloading the
// store from disk so we observe what was actually written.
func reloadGoalWeight(t *testing.T, cfg *config.Config, id string) int {
	t.Helper()
	s := reloadStoreGoal(t, cfg)
	g, ok := s.Get(id)
	if !ok {
		t.Fatalf("%s not found after reload", id)
	}
	if g.Weight == nil {
		t.Fatalf("%s has nil weight after reload", id)
	}
	return *g.Weight
}

// TestUpdateActiveGoalWeightGuard exercises the I-1599 guard on the single-field
// `st update G-xxx weight <n>` path: a write that would push the active weight
// sum over 100 is refused (and the on-disk value is unchanged), while a write
// that keeps the sum ≤100 is applied.
func TestUpdateActiveGoalWeightGuard(t *testing.T) {
	t.Run("over-budget refused", func(t *testing.T) {
		_, _, cfg := newGoalEnv(t)
		seedGoalFile(t, cfg, "G-001", "active", 60)
		seedGoalFile(t, cfg, "G-002", "active", 30) // active sum = 90
		s := reloadStoreGoal(t, cfg)

		// 60 + 50 = 110 > 100 → must refuse.
		if rc := Update(s, cfg, "G-002", "weight", "50", UpdateModeValue); rc == 0 {
			t.Error("Update weight=50 with active sum 90 should be refused (110>100)")
		}
		if got := reloadGoalWeight(t, cfg, "G-002"); got != 30 {
			t.Errorf("G-002 weight = %d after refused update, want unchanged 30", got)
		}
	})

	t.Run("within-budget applied", func(t *testing.T) {
		_, _, cfg := newGoalEnv(t)
		seedGoalFile(t, cfg, "G-001", "active", 60)
		seedGoalFile(t, cfg, "G-002", "active", 30) // active sum = 90
		s := reloadStoreGoal(t, cfg)

		// 60 + 40 = 100 ≤ 100 → must apply.
		if rc := Update(s, cfg, "G-002", "weight", "40", UpdateModeValue); rc != 0 {
			t.Errorf("Update weight=40 with active sum 90 should apply (100≤100), got rc=%d", rc)
		}
		if got := reloadGoalWeight(t, cfg, "G-002"); got != 40 {
			t.Errorf("G-002 weight = %d after applied update, want 40", got)
		}
	})
}

// TestUpdateGoalTitleUnaffectedByWeightGuard proves the guard is scoped to the
// weight field: a non-weight edit on an active goal succeeds even when the
// active weight sum is already at the ceiling.
func TestUpdateGoalTitleUnaffectedByWeightGuard(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 60)
	seedGoalFile(t, cfg, "G-002", "active", 40) // active sum = 100 (at ceiling)
	s := reloadStoreGoal(t, cfg)

	if rc := Update(s, cfg, "G-002", "title", "Renamed goal", UpdateModeValue); rc != 0 {
		t.Errorf("non-weight goal edit must be unaffected by the weight guard, got rc=%d", rc)
	}
	s2 := reloadStoreGoal(t, cfg)
	g, _ := s2.Get("G-002")
	if g.Title != "Renamed goal" {
		t.Errorf("title = %q after update, want %q", g.Title, "Renamed goal")
	}
}

// TestUpdateDraftGoalWeightExempt proves a draft goal's weight is NOT guarded —
// only active goals count toward the ≤100 budget, so a draft can take any
// weight regardless of the active sum.
func TestUpdateDraftGoalWeightExempt(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 60)
	seedGoalFile(t, cfg, "G-002", "active", 30) // active sum = 90
	seedGoalFile(t, cfg, "G-003", "draft", 10)
	s := reloadStoreGoal(t, cfg)

	// A draft goal at weight 80 would push the would-be active sum to 170,
	// but drafts are exempt → must apply.
	if rc := Update(s, cfg, "G-003", "weight", "80", UpdateModeValue); rc != 0 {
		t.Errorf("draft-goal weight edit must be exempt from the guard, got rc=%d", rc)
	}
	if got := reloadGoalWeight(t, cfg, "G-003"); got != 80 {
		t.Errorf("G-003 (draft) weight = %d after update, want 80", got)
	}
}

// TestUpdateBatchActiveGoalWeightGuard proves the same guard fires on the
// batch `st update G-xxx weight=NN` path.
func TestUpdateBatchActiveGoalWeightGuard(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 60)
	seedGoalFile(t, cfg, "G-002", "active", 30) // active sum = 90
	s := reloadStoreGoal(t, cfg)

	if rc := UpdateBatch(s, cfg, "G-002", []FieldValue{{Field: "weight", Value: "50"}}); rc == 0 {
		t.Error("UpdateBatch weight=50 with active sum 90 should be refused (110>100)")
	}
	if got := reloadGoalWeight(t, cfg, "G-002"); got != 30 {
		t.Errorf("G-002 weight = %d after refused batch update, want unchanged 30", got)
	}

	// Within budget → applied.
	if rc := UpdateBatch(s, cfg, "G-002", []FieldValue{{Field: "weight", Value: "40"}}); rc != 0 {
		t.Errorf("UpdateBatch weight=40 with active sum 90 should apply, got rc=%d", rc)
	}
	if got := reloadGoalWeight(t, cfg, "G-002"); got != 40 {
		t.Errorf("G-002 weight = %d after applied batch update, want 40", got)
	}
}
