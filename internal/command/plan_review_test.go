package command

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/plan"
)

// TestPlanApproveBlocksNonVerifiableACsByDefault confirms the I-710
// strict-default flip: a plan sidecar with vague ACs is refused on
// every approval (no `--strict` opt-in required).
func TestPlanApproveBlocksNonVerifiableACsByDefault(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"the feature works"}, // vague
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 2 {
			t.Errorf("expected exit 2 on vague AC; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should remain false when AC gate rejects")
	}
}

// TestPlanApproveStrictFlagAcceptedAsNoopForACs confirms the I-710
// promise that `--strict` keeps working as a no-op alias for the AC
// gate (now-unconditional). Passing --strict on a clean plan must
// still succeed.
func TestPlanApproveStrictFlagAcceptedAsNoopForACs(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Strict: true}); code != 0 {
		t.Errorf("strict approve with clean ACs should exit 0; got %d", code)
	}
}

// TestPlanCheckBlocksOnNonVerifiableACs confirms the I-710 hook-surface
// re-validation: even after a plan is approved, if its sidecar ACs are
// later degraded to non-verifiable, `st plan check` (the contract
// called by plan-before-code-guard.sh) closes the gate.
func TestPlanCheckBlocksOnNonVerifiableACs(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Clean sidecar → approve succeeds.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("initial approve: %d", code)
	}

	// Degrade ACs to vague. Overwrite sidecar.
	if err := plan.SaveWithOpts(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"the feature works"}, // vague
	}, plan.SaveOpts{}); err != nil {
		t.Fatalf("re-saving sidecar: %v", err)
	}

	suppressOutput(t, func() {
		if code := PlanCheck(s, cfg, "T-001"); code != 1 {
			t.Errorf("PlanCheck should fail when ACs degrade; got %d", code)
		}
	})
}

// TestPlanApproveRunsPlanReviewSubAgent confirms the I-710 sub-agent
// integration: when an engine is supplied, the review fires and an
// "Accept" verdict allows approval to proceed to the validator gates.
func TestPlanApproveRunsPlanReviewSubAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	fake := &fakeClaude{stepResults: []string{"RECOMMENDATION: Accept — looks good"}}
	engine := RunEngine{RunClaude: fake.run}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 0 {
			t.Errorf("expected 0 with engine + accept verdict; got %d", code)
		}
	})
	if fake.calls == 0 {
		t.Error("expected at least one RunClaude call")
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after accept verdict")
	}
}

// TestPlanApproveSkipsReviewWhenEngineNil confirms the I-710 test-path
// invariant: a nil engine pointer skips the sub-agent review (matches
// the I-588 create-review pattern). Existing in-process tests pass
// `PlanApproveOpts{}` with a zero Engine — they must keep working.
func TestPlanApproveSkipsReviewWhenEngineNil(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	// No engine → review skipped, approval proceeds.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Errorf("expected 0 with nil engine; got %d", code)
	}
}

// TestPlanApproveRejectsOnReviewEngineFailure confirms the fail-closed
// posture: when the Claude sub-agent returns a Reject verdict, the
// approval is refused (exit 2) and PlanApproved stays false.
func TestPlanApproveRejectsOnReviewEngineFailure(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	fake := &fakeClaude{stepResults: []string{"RECOMMENDATION: Reject — plan premise is incoherent"}}
	engine := RunEngine{RunClaude: fake.run}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 2 {
			t.Errorf("expected exit 2 on review reject; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when review rejects")
	}
}

