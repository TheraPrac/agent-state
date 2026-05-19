package command

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/registry"
)

// These tests wire the canonical I-681 shape onto the shared fixture: the
// active sprint gains T-002, and T-002 depends_on T-001 (baseline fixture
// edge), so T-001 blocks an active-sprint member while carrying no sprint
// of its own — exactly the I-676 → T-203 situation.

func TestStackPush_InheritsActiveSprint(t *testing.T) {
	s, cfg, epicID, sprintID := setupSprintTestEnv(t)
	if rc := SprintAdd(s, cfg, sprintID, []string{"T-002"}); rc != 0 {
		t.Fatalf("seed SprintAdd rc=%d", rc)
	}

	if rc := StackPush(s, cfg, "T-001", StackPushOpts{Reason: "blocks T-002", FromPending: true}); rc != 0 {
		t.Fatalf("StackPush rc=%d, want 0", rc)
	}

	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	if !contains(sp.Items, "T-001") {
		t.Fatalf("sprint %s items = %v, want T-001 inherited", sprintID, sp.Items)
	}
	got, _ := s.Get("T-001")
	if got.Sprint != sprintID || got.Epic != epicID {
		t.Fatalf("T-001 sprint=%q epic=%q, want %q/%q", got.Sprint, got.Epic, sprintID, epicID)
	}
}

func TestStartGate_RejectsOffSprintBlocker(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	if rc := SprintAdd(s, cfg, sprintID, []string{"T-002"}); rc != 0 {
		t.Fatalf("seed SprintAdd rc=%d", rc)
	}

	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 1 {
		t.Fatalf("Start rc=%d, want 1 (gated)", rc)
	}
	got, _ := s.Get("T-001")
	if got.Sprint != "" {
		t.Fatalf("T-001 sprint=%q, want empty (gate must not mutate)", got.Sprint)
	}
	if got.Status != "queued" {
		t.Fatalf("T-001 status=%q, want queued (start refused)", got.Status)
	}
}

func TestStartGate_AddToSprintResolvesAndStarts(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	if rc := SprintAdd(s, cfg, sprintID, []string{"T-002"}); rc != 0 {
		t.Fatalf("seed SprintAdd rc=%d", rc)
	}

	if rc := Start(s, cfg, "T-001", StartOpts{AddToSprint: true}); rc != 0 {
		t.Fatalf("Start --add-to-sprint rc=%d, want 0", rc)
	}
	got, _ := s.Get("T-001")
	if got.Sprint != sprintID {
		t.Fatalf("T-001 sprint=%q, want %q", got.Sprint, sprintID)
	}
	if got.Status != "active" {
		t.Fatalf("T-001 status=%q, want active (started)", got.Status)
	}
}

func TestStartGate_ForceBypassLeavesItemSprintless(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	if rc := SprintAdd(s, cfg, sprintID, []string{"T-002"}); rc != 0 {
		t.Fatalf("seed SprintAdd rc=%d", rc)
	}

	if rc := Start(s, cfg, "T-001", StartOpts{Force: true}); rc != 0 {
		t.Fatalf("Start --force rc=%d, want 0 (bypass)", rc)
	}
	got, _ := s.Get("T-001")
	if got.Sprint != "" {
		t.Fatalf("T-001 sprint=%q, want empty (force bypasses, does not add)", got.Sprint)
	}
	if got.Status != "active" {
		t.Fatalf("T-001 status=%q, want active (started despite gate)", got.Status)
	}
}

// TestStartGate_DependencyGateRunsBeforeSprintGate locks in the review fix:
// the read-only dependency gate must fire BEFORE the I-681 sprint gate, so
// an item that can't start anyway is never mutated into a sprint. I-001 is
// put in the active sprint and made to depend on T-002 (so T-002 blocks an
// active-sprint member); T-002 itself depends on T-001 (baseline fixture,
// queued/non-terminal) so T-002 is blocked. Even with --add-to-sprint, the
// dependency gate must reject first and leave T-002 sprintless — on the
// pre-fix ordering --add-to-sprint would have called SprintAdd before the
// blocked-by check and left T-002 in the sprint.
func TestStartGate_DependencyGateRunsBeforeSprintGate(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	if rc := SprintAdd(s, cfg, sprintID, []string{"I-001"}); rc != 0 {
		t.Fatalf("seed SprintAdd rc=%d", rc)
	}
	if rc := DepAdd(s, cfg, "I-001", "T-002"); rc != 0 {
		t.Fatalf("seed DepAdd rc=%d", rc)
	}

	if rc := Start(s, cfg, "T-002", StartOpts{AddToSprint: true}); rc != 1 {
		t.Fatalf("Start rc=%d, want 1 (dependency gate before sprint gate)", rc)
	}
	got, _ := s.Get("T-002")
	if got.Sprint != "" {
		t.Fatalf("T-002 sprint=%q, want empty — dep gate must reject before SprintAdd runs", got.Sprint)
	}
}
