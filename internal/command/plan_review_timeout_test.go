package command

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/theraprac/agent-state/internal/plan"
)

// I-752 motivating incident: I-738 plan approval hung a Claude sub-agent
// for 53 minutes against the 2h global wall cap because plan-review had
// no per-step ceiling. Tests below pin the three operator surfaces of
// the fix: env override is honored, --bypass-review skips the sub-agent
// entirely, and a missing override pins the cap at 10m.

// TestPlanReviewTimeoutEnvOverride asserts that when the sub-agent
// surfaces the engine's wall-time error, runPlanReview returns 2 and
// the timeout-specific message is emitted (so operators see the
// AS_PLAN_APPROVE_TIMEOUT / --bypass-review hint, not the generic
// "sub-agent failed" branch).
func TestPlanReviewTimeoutEnvOverride(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "1s")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	// Stub RunClaude that simulates the defaultRunClaude wall-time
	// error path (returns nil output + a "wall time limit" error).
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return nil, 1, errors.New("killed: wall time limit (1s)")
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 2 {
		t.Errorf("expected exit 2 on wall-time error; got %d", code)
	}

	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false on wall-time timeout")
	}
}

// TestPlanReviewBypassFlag asserts that BypassReview=true skips the
// sub-agent entirely (RunClaude never invoked) while the downstream
// validator gates still run — i.e. a clean plan still approves.
func TestPlanReviewBypassFlag(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	// Plan-review sub-agent calls are the ones to guard. stampModelRec makes
	// a separate Haiku call (model-rec) — that is expected and allowed.
	var planReviewCalled int
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					// Haiku call from stampModelRec — return valid model-rec JSON.
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			// Non-Haiku call = plan-review sub-agent — must NOT fire with BypassReview.
			planReviewCalled++
			return nil, 0, nil
		},
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{
			Engine:       &engine,
			BypassReview: true,
		}); code != 0 {
			t.Errorf("expected exit 0 on bypass-review with clean plan; got %d", code)
		}
	})

	if planReviewCalled != 0 {
		t.Errorf("plan-review RunClaude was called %d time(s); expected 0 with BypassReview=true", planReviewCalled)
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after bypass-review on clean plan")
	}
}

// TestPlanReviewDefaultFirstPassCap asserts that when no override is set,
// plan-review injects AS_CLAUDE_WALL_TIMEOUT=23m30s (25m default − 90s
// wrap-up budget) on the first pass. This is the gate that prevents the
// I-738 hang from recurring and the I-810 referendum timeout from
// discarding all analysis.
func TestPlanReviewDefaultFirstPassCap(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var (
		mu           sync.Mutex
		capturedEnvs [][]string // collect all invocations; model-rec (haiku) adds a second call
	)
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			mu.Lock()
			capturedEnvs = append(capturedEnvs, append([]string{}, env...))
			mu.Unlock()
			// Accept verdict so PlanApprove can proceed past the
			// review without test scaffolding for auto-fix loops.
			body, _ := json.Marshal(ClaudeResult{
				Type: "result", Subtype: "success",
				Result: "RECOMMENDATION: Accept — looks good",
			})
			return body, 0, nil
		},
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 0 {
			t.Errorf("expected exit 0 on accept verdict; got %d", code)
		}
	})

	// The first-pass plan-review call must carry 23m30s (25m − 90s wrap-up
	// budget). At least one invocation (there may be a second Haiku
	// model-rec call) must have it.
	want := "AS_CLAUDE_WALL_TIMEOUT=23m30s"
	found := false
	for _, envSnapshot := range capturedEnvs {
		for _, e := range envSnapshot {
			if strings.Contains(e, want) {
				found = true
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

// TestPlanReviewWrapUpSkippedOnCleanPass asserts that when the first-pass
// stub returns a clean Accept, no wrap-up (--resume) call is ever made.
// Regression guard: the wrap-up path must not fire on the happy path.
func TestPlanReviewWrapUpSkippedOnCleanPass(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var resumeCalls int
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			// Detect --resume flag: wrap-up uses isResume=true → --resume arg.
			for _, a := range args {
				if a == "--resume" {
					resumeCalls++
				}
			}
			body, _ := json.Marshal(ClaudeResult{
				Type: "result", Subtype: "success",
				Result: "RECOMMENDATION: Accept — plan looks good",
			})
			return body, 0, nil
		},
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 0 {
			t.Errorf("expected exit 0; got %d", code)
		}
	})

	if resumeCalls != 0 {
		t.Errorf("wrap-up resume was called %d time(s); expected 0 on a clean first pass", resumeCalls)
	}
}

// TestPlanReviewWrapUpYieldsVerdict asserts the drive-to-conclusion path:
// first pass times out (wall time limit, empty output), wrap-up resume
// returns an Accept verdict → PlanApprove returns 0 and approves the plan.
func TestPlanReviewWrapUpYieldsVerdict(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "") // default 25m; wrap-up is enabled
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if a == "--resume" {
					// Wrap-up resume → return Accept verdict.
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: "RECOMMENDATION: Accept — sufficient evidence gathered.",
					})
					return body, 0, nil
				}
			}
			// First pass (non-resume, non-haiku) → simulate wall-time kill.
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			return nil, 1, errors.New("killed: wall time limit (23m30s)")
		},
	}

	suppressOutput(t, func() {
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 0 {
			t.Errorf("expected exit 0 after wrap-up verdict; got %d", code)
		}
	})

	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after wrap-up Accept verdict")
	}
}

// TestPlanReviewWrapUpNonZeroExitRefusesApproval asserts that when the
// wrap-up resume returns non-empty output but exits non-zero (e.g. an
// error_during_execution subtype that carries a result field), the garbage
// output is NOT parsed as a verdict and approval is refused (exit 2).
func TestPlanReviewWrapUpNonZeroExitRefusesApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "") // default 25m; wrap-up enabled
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			for _, a := range args {
				if a == "--resume" {
					// Wrap-up exits non-zero but still carries output (e.g.
					// error_during_execution with a result body). Must NOT
					// be treated as a valid verdict.
					body, _ := json.Marshal(ClaudeResult{
						Type:    "result",
						Subtype: "error_during_execution",
						Result:  "I would Accept this if I had more context.",
					})
					return body, 1, nil
				}
			}
			// First pass: wall-time kill.
			return nil, 1, errors.New("killed: wall time limit (23m30s)")
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 2 {
		t.Errorf("expected exit 2 when wrap-up exits non-zero; got %d", code)
	}

	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved must stay false when wrap-up exits non-zero despite non-empty output")
	}
}

// TestPlanReviewWrapUpDoubleTimeout asserts that when both the first pass
// and the wrap-up resume time out, runPlanReview returns 2 and the plan
// is not approved.
func TestPlanReviewWrapUpDoubleTimeout(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "") // default 25m; wrap-up is enabled
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			// Both first pass and wrap-up resume time out.
			return nil, 1, errors.New("killed: wall time limit")
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 2 {
		t.Errorf("expected exit 2 on double timeout; got %d", code)
	}

	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should stay false after double timeout")
	}
}

// TestPlanReviewWrapUpDisabledForShortCap asserts that a tiny
// AS_PLAN_APPROVE_TIMEOUT (≤ 2× wrapUpBudget = 3m) disables the wrap-up
// and falls straight to exit 2 on the first wall-time hit — one non-Haiku
// review call, no --resume call.
func TestPlanReviewWrapUpDisabledForShortCap(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PLAN_APPROVE_TIMEOUT", "1s")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var reviewCalls, resumeCalls int
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
				if a == "--resume" {
					resumeCalls++
				}
			}
			reviewCalls++
			return nil, 1, errors.New("killed: wall time limit (1s)")
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 2 {
		t.Errorf("expected exit 2; got %d", code)
	}

	if resumeCalls != 0 {
		t.Errorf("wrap-up --resume was called %d time(s); expected 0 for short cap", resumeCalls)
	}
	if reviewCalls != 1 {
		t.Errorf("expected exactly 1 review call; got %d", reviewCalls)
	}
}

// TestPlanReviewRunsEvenWithPrepStamp asserts that an explicit --review runs
// the sub-agent even when the plan carries a prep_reviewed_at stamp. I-933
// removed the I-992 short-circuit (it could skip review of a plan edited after
// the prep-time review); an explicit --review always honors the request.
func TestPlanReviewRunsEvenWithPrepStamp(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:       "Approach.",
		ScopeRepos:     []string{"as"},
		ACs:            []string{"cmd: go test ./..."},
		PrepReviewedAt: "2026-06-14T10:00:00-06:00",
		Tests:          "Covered by existing test suite.",
		OutOfScope:     "None",
		Risks:          "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var reviewCalls int
	var mu sync.Mutex
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			mu.Lock()
			reviewCalls++
			mu.Unlock()
			body, _ := json.Marshal(ClaudeResult{
				Type: "result", Subtype: "success",
				Result: "RECOMMENDATION: Accept — looks good",
			})
			return body, 0, nil
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 0 {
		t.Errorf("expected exit 0 with accept verdict; got %d", code)
	}

	mu.Lock()
	calls := reviewCalls
	mu.Unlock()
	if calls == 0 {
		t.Error("sub-agent MUST run on explicit --review even with a prep_reviewed_at stamp (I-992 short-circuit removed)")
	}
}

// TestPlanReviewCalledForHandAuthoredPlan asserts that a plan WITHOUT a
// prep_reviewed_at stamp (hand-authored, no prior LLM review) still
// invokes the sub-agent. I-992.
func TestPlanReviewCalledForHandAuthoredPlan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// No PrepReviewedAt — hand-authored plan.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var reviewCalls int
	var mu sync.Mutex
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			mu.Lock()
			reviewCalls++
			mu.Unlock()
			body, _ := json.Marshal(ClaudeResult{
				Type: "result", Subtype: "success",
				Result: "RECOMMENDATION: Accept",
			})
			return body, 0, nil
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, Review: true}); code != 0 {
		t.Errorf("expected exit 0; got %d", code)
	}

	mu.Lock()
	calls := reviewCalls
	mu.Unlock()
	if calls == 0 {
		t.Error("sub-agent should be called for a hand-authored plan with no prep_reviewed_at stamp")
	}
}

// TestPlanReviewBypassFlagWithPrepStamp asserts that --bypass-review still
// approves a prep-stamped plan without invoking the sub-agent, and that the
// stamp does not interfere with the bypass path. I-992.
func TestPlanReviewBypassFlagWithPrepStamp(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:       "Approach.",
		ScopeRepos:     []string{"as"},
		ACs:            []string{"cmd: go test ./..."},
		PrepReviewedAt: "2026-06-14T10:00:00-06:00",
		Tests:          "Covered by existing test suite.",
		OutOfScope:     "None",
		Risks:          "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var reviewCalls int
	var mu sync.Mutex
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, a := range args {
				if strings.Contains(a, "haiku") {
					body, _ := json.Marshal(ClaudeResult{
						Type: "result", Subtype: "success",
						Result: `{"tier":"sonnet","reason":"test"}`,
					})
					return body, 0, nil
				}
			}
			mu.Lock()
			reviewCalls++
			mu.Unlock()
			return nil, 1, errors.New("unexpected review call")
		},
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine, BypassReview: true}); code != 0 {
		t.Errorf("expected exit 0 with --bypass-review + prep stamp; got %d", code)
	}

	mu.Lock()
	calls := reviewCalls
	mu.Unlock()
	if calls != 0 {
		t.Errorf("sub-agent should not be called with --bypass-review; called %d time(s)", calls)
	}

	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after --bypass-review on a prep-stamped plan")
	}
}
