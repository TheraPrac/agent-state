package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jfinlinson/agent-state/internal/plan"
)

// writeFileErr is a small helper that returns the os.WriteFile error
// so callers can `t.Fatalf` on a single line. The existing test
// helper `writeFile` swallows the error.
func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// TestPlanApproveBlocksScaffoldApproachInSidecar confirms the I-710
// ValidatePlan wiring: a sidecar with a scaffold Approach (TODO) is
// refused on every approval. Closes the review finding that the
// Approach + ScopeRepos checks added in ValidatePlan were dead code.
func TestPlanApproveBlocksScaffoldApproachInSidecar(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "TODO",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 2 {
			t.Errorf("expected exit 2 on scaffold approach; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when approach is scaffold")
	}
}

// TestPlanApproveBlocksEmptyScopeReposInSidecar confirms the I-710
// ValidatePlan wiring catches a sidecar with no ScopeRepos.
func TestPlanApproveBlocksEmptyScopeReposInSidecar(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// plan.Save itself rejects empty ScopeRepos (the structural
	// minimum), so we write the sidecar through SaveWithOpts after
	// validating ScopeRepos won't pass the structural floor. The
	// gate we're testing is ValidatePlan's, which fires before
	// approval; structural rejection in plan.Save is a separate
	// surface. So seed via SaveWithOpts with a single-entry
	// ScopeRepos, then mutate the sidecar to remove ScopeRepos.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Real approach.",
		ScopeRepos: []string{"placeholder"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}
	// Rewrite the sidecar file to drop ScopeRepos. The sidecar is at
	// .plans/<id>.md under cfg.Root().
	sidecarPath := cfg.PlansDir() + "/T-001.md"
	body := "---\nplan_approved: false\n---\n\n## Approach\nReal approach.\n\n## Acceptance Criteria\n- cmd: go test ./...\n"
	if err := writeFileErr(sidecarPath, body); err != nil {
		t.Fatalf("rewriting sidecar: %v", err)
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 2 {
			t.Errorf("expected exit 2 on empty scope_repos; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when scope_repos is empty")
	}
}

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

// TestPlanApproveRefusesOnRejectVerdict confirms the Reject-verdict
// branch of the review loop: a verdict containing "reject" refuses
// approval (exit 2) and leaves PlanApproved false.
func TestPlanApproveRefusesOnRejectVerdict(t *testing.T) {
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

// TestPlanApproveRefusesOnEngineExecError confirms the fail-closed
// posture when the Claude sub-agent itself errors during execution
// (network failure, binary missing, timeout). The exec-error guard
// (`!sr.Passed && sr.FullOutput == ""`) must refuse approval — an
// opaque LLM failure cannot silently waive the substance check.
func TestPlanApproveRefusesOnEngineExecError(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	// errOnCall=1 → executeClaude returns sr.Passed=false,
	// sr.FullOutput="" (the exec-error guard's condition).
	fake := &fakeClaude{errOnCall: 1}
	engine := RunEngine{RunClaude: fake.run}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 2 {
			t.Errorf("expected exit 2 on engine exec error; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when engine errors")
	}
}

// TestPlanApproveRefusesOnAutoFixCapExhaustion confirms the
// fail-closed posture when the sub-agent never converges on a clean
// Accept within maxPlanReviewAutoFixIterations passes. A persistent
// "Accept with notes" verdict that exhausts the cap refuses approval
// rather than silently waiving the gate.
// TestPlanApproveAcceptsOnAutoFixCapExhaustion asserts that when the
// review sub-agent persistently returns "Accept with notes" and the
// auto-fix iteration cap is exhausted, PlanApprove returns 0 (accept)
// and marks the plan approved. "Accept with notes" means the plan was
// fundamentally accepted; the notes are advisory, not blocking. I-985.
func TestPlanApproveAcceptsOnAutoFixCapExhaustion(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	// Persistent Accept-with-notes — fakeClaude reuses the last
	// entry past the script length, so every iteration sees this.
	fake := &fakeClaude{stepResults: []string{"RECOMMENDATION: Accept with notes — minor gap"}}
	engine := RunEngine{
		RunClaude:  fake.run,
		PromptUser: func(prompt string) (string, error) { return "", nil },
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string { return "1" },
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 0 {
			t.Errorf("expected exit 0 (accept-as-advisory) on auto-fix cap exhaustion; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after accept-with-notes exhaustion")
	}
}

// TestPlanReviewAutoFixTimeoutPropagated asserts that when the plan-review
// sub-agent returns "Accept with notes", the auto-fix subprocess also
// receives AS_CLAUDE_WALL_TIMEOUT (matching the review cap). I-985.
func TestPlanReviewAutoFixTimeoutPropagated(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var (
		mu           sync.Mutex
		capturedEnvs [][]string
		callCount    int
	)
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			mu.Lock()
			capturedEnvs = append(capturedEnvs, append([]string{}, env...))
			n := callCount
			callCount++
			mu.Unlock()

			// First call (review): return "Accept with notes" to trigger auto-fix.
			// Subsequent calls (auto-fix + re-review): return clean Accept so the
			// loop terminates and we can assert on envs captured so far.
			var result string
			if n == 0 {
				result = "RECOMMENDATION: Accept with notes — tighten the AC format"
			} else {
				result = "RECOMMENDATION: Accept — looks good"
			}
			body, _ := json.Marshal(ClaudeResult{
				Type: "result", Subtype: "success", Result: result,
			})
			return body, 0, nil
		},
		PromptUser: func(p string) (string, error) { return "", nil },
		SelectMenu:  func(p string, opts []menuOption, def int) string { return "1" },
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 0 {
			t.Errorf("expected exit 0; got %d", code)
		}
	})

	// The auto-fix call (call index 1) must carry the wall-timeout env var.
	want := "AS_CLAUDE_WALL_TIMEOUT=10m0s"
	found := false
	mu.Lock()
	defer mu.Unlock()
	for i, envSnapshot := range capturedEnvs {
		for _, e := range envSnapshot {
			if strings.Contains(e, want) {
				found = true
				_ = i
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Errorf("expected at least one RunClaude invocation with env containing %q; invocations: %v", want, capturedEnvs)
	}
}

// TestPlanApproveAutoFixFailureRefusesApproval asserts that when the
// auto-fix sub-agent fails (engine error), PlanApprove returns 2 and
// does not approve the plan. This verifies I-985's fix: runAutoFixFromNotes
// now propagates feedbackSR.Passed and feedbackSR.Error into *sr so the
// guard in plan_review.go can actually fire.
func TestPlanApproveAutoFixFailureRefusesApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var callCount int
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callCount++
			if callCount == 1 {
				// First call: review returns "Accept with notes" to trigger auto-fix.
				body, _ := json.Marshal(ClaudeResult{
					Type: "result", Subtype: "success",
					Result: "RECOMMENDATION: Accept with notes — tighten the approach section",
				})
				return body, 0, nil
			}
			// Second call: auto-fix sub-agent fails (engine error).
			return nil, 1, fmt.Errorf("simulated auto-fix engine failure")
		},
		PromptUser: func(p string) (string, error) { return "", nil },
		SelectMenu:  func(p string, opts []menuOption, def int) string { return "1" },
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 2 {
			t.Errorf("expected exit 2 when auto-fix engine fails; got %d", code)
		}
	})
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false when auto-fix engine fails")
	}
}

