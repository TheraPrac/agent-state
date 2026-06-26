package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// deleteSidecar removes the .plans/<id>.md file the setupTestEnv
// fixture seeded. Used by the no-sidecar branch tests.
func deleteSidecar(t *testing.T, plansDir, id string) {
	t.Helper()
	path := filepath.Join(plansDir, id+".md")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing sidecar %s: %v", path, err)
	}
}

// TestPlanApproveRefusesIfNoSidecarForTask is the core I-716
// invariant: a task with plan_approved=false and no sidecar gets
// refused at approval time, pointing the operator at `st plan
// prep`.
func TestPlanApproveRefusesIfNoSidecarForTask(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	deleteSidecar(t, cfg.PlansDir(), "T-001")

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 2 {
			t.Errorf("expected exit 2 on missing sidecar (task); got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when sidecar missing")
	}
}

// TestPlanApproveRefusesIfNoSidecarForIssue is the symmetric
// invariant for issues — same refusal at approval time.
func TestPlanApproveRefusesIfNoSidecarForIssue(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	deleteSidecar(t, cfg.PlansDir(), "I-001")

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "I-001", PlanApproveOpts{}); code != 2 {
			t.Errorf("expected exit 2 on missing sidecar (issue); got %d", code)
		}
	})
}

// TestPlanApproveSkipsSidecarCheckForIdea — the I-487 type carve-
// out: ideas don't carry SBAR or require a plan body. Missing
// sidecar should NOT refuse for an idea. (Switches T-001's type
// in-place to exercise the non-task/issue branch.)
func TestPlanApproveSkipsSidecarCheckForIdea(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	deleteSidecar(t, cfg.PlansDir(), "T-001")
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Type = "idea"
		it.Status = "captured" // ideas use a different status vocab
		it.Doc.SetField("type", "idea")
		it.Doc.SetField("status", "captured")
		return nil
	}); err != nil {
		t.Fatalf("mutating type: %v", err)
	}

	// No sidecar + idea type → should NOT refuse on the I-716
	// gate. (Other gates may still apply; this test just
	// confirms the missing-sidecar refusal doesn't fire.)
	suppressOutput(t, func() {
		code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{})
		// Ideas don't run the I-710 SBAR gate either, so this
		// should succeed.
		if code != 0 {
			t.Errorf("expected exit 0 on idea with no sidecar; got %d", code)
		}
	})
}

// TestPlanCheckClosesGateOnSidecarDeletion — the hook surface
// must close when a sidecar is deleted post-approval. Approves
// T-001 (clean fixture sidecar present), deletes the sidecar,
// calls PlanCheck → exit 1.
func TestPlanCheckClosesGateOnSidecarDeletion(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Fixture sidecar present → approval succeeds.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("initial approve: %d", code)
	}
	// Delete sidecar to simulate post-approval removal.
	deleteSidecar(t, cfg.PlansDir(), "T-001")

	suppressOutput(t, func() {
		if code := PlanCheck(s, cfg, "T-001"); code != 1 {
			t.Errorf("PlanCheck should close the gate after sidecar deletion; got %d", code)
		}
	})
}

// TestStartRefusesOnMissingSidecar exercises the integration
// through the freshness gate: an approved item whose sidecar is
// deleted gets refused at `st start` time (Stale verdict).
//
// This is a unit-level proxy: instead of going through
// command.Start (which has worktree-creation side effects), we
// call runFreshnessGate directly with the same conditions.
func TestStartRefusesOnMissingSidecar(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Approve T-001 with the fixture sidecar present.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	// Delete sidecar post-approval.
	deleteSidecar(t, cfg.PlansDir(), "T-001")

	suppressOutput(t, func() {
		code := runFreshnessGate(cfg, s, "T-001", StartOpts{})
		if code != 2 {
			t.Errorf("freshness gate should refuse activation (exit 2) when sidecar missing; got %d", code)
		}
	})
}
