package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/model"
)

// I-178: PlanApprove flips PlanApproved + sets audit fields + writes a
// changelog entry.
func TestPlanApproveHappyPath(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve returned %d", code)
	}

	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true")
	}
	if item.PlanApprovedBy == "" {
		t.Error("PlanApprovedBy should be set")
	}
	if item.PlanApprovedAt == "" {
		t.Error("PlanApprovedAt should be set")
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "plan_approve" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected plan_approve entry in changelog")
	}
}

// I-178: re-approving an already-approved plan refuses (caller must
// reset first), so the audit timestamp can't be silently overwritten.
func TestPlanApproveRefusesIfAlreadyApproved(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("first approve: %d", code)
	}
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 1 {
		t.Errorf("second approve should fail; got %d", code)
	}
}

// I-178: PlanReset clears the audit + flips Approved=false. Refuses if
// already not-approved (no-op safety).
func TestPlanResetRevertsApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := PlanReset(s, cfg, "T-001"); code != 0 {
		t.Fatalf("reset: %d", code)
	}
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should be false after reset")
	}
	if item.PlanApprovedBy != "" || item.PlanApprovedAt != "" {
		t.Error("audit fields should be cleared on reset")
	}
}

func TestPlanResetRefusesIfNotApproved(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanReset(s, cfg, "T-001"); code != 1 {
		t.Errorf("reset on unapproved item should fail; got %d", code)
	}
}

// I-178: PlanCheck exits 0 when approved, 1 when not — the hook contract.
func TestPlanCheckExitCodes(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanCheck(s, cfg, "T-001"); code != 1 {
		t.Errorf("check on unapproved should exit 1, got %d", code)
	}
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := PlanCheck(s, cfg, "T-001"); code != 0 {
		t.Errorf("check on approved should exit 0, got %d", code)
	}
}

func TestPlanCheckMissingItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanCheck(s, cfg, "T-999"); code != 1 {
		t.Errorf("check on missing item should exit 1, got %d", code)
	}
}

// I-178: round-trip the new schema fields through Mutate + reload so the
// hook can read PlanApprovedBy/At after the operator runs `st plan
// approve` from a different process.
func TestPlanApprovalPersistsAcrossReload(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}

	// Force a reload by walking the store fresh.
	out := captureStdout(t, func() { PlanShow(s, cfg, "T-001") })
	if !strings.Contains(out, "approved") {
		t.Errorf("show output missing approval line: %q", out)
	}
	if !strings.Contains(out, "user") {
		t.Errorf("show output missing approver: %q", out)
	}
}

// I-149: --strict refuses approval when SBAR is empty or still on the
// I-492 TODO scaffold. The test items shipped from setupTestEnv have
// no SBAR (legacy fixture), so a strict-mode approve must reject.
func TestPlanApproveStrict_RefusesEmptySBAR(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Strict: true}); code != 2 {
		t.Errorf("strict approve with empty SBAR should exit 2, got %d", code)
	}
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("strict approve should not have flipped PlanApproved on rejected item")
	}
}

// I-149: once the SBAR is fully populated, --strict approves cleanly.
// AC verifiability is independent (I-511) and not exercised here —
// the test fixture has no plan sidecar so the I-511 path is a no-op.
func TestPlanApproveStrict_PassesWithPopulatedSBAR(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SBAR = model.SBAR{
			Situation:      "real",
			Background:     "real",
			Assessment:     "real",
			Recommendation: "real",
		}
		it.Doc.SetSBARBlock(it.SBAR)
		return nil
	}); err != nil {
		t.Fatalf("seeding SBAR: %v", err)
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Strict: true}); code != 0 {
		t.Errorf("strict approve with populated SBAR should exit 0, got %d", code)
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("populated SBAR should pass strict gate and flip PlanApproved")
	}
}
