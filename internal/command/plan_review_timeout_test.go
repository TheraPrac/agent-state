package command

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jfinlinson/agent-state/internal/plan"
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

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 2 {
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
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	var called int
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			called++
			t.Fatalf("RunClaude must not be invoked when BypassReview=true")
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

	if called != 0 {
		t.Errorf("RunClaude was called %d time(s); expected 0", called)
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true after bypass-review on clean plan")
	}
}

// TestPlanReviewDefaultCapTenMinutes asserts that when no override is
// set, plan-review injects AS_CLAUDE_WALL_TIMEOUT=10m0s into the env
// passed to RunClaude. This is the gate that prevents the I-738 hang
// from recurring under default operator conditions.
func TestPlanReviewDefaultCapTenMinutes(t *testing.T) {
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
		mu          sync.Mutex
		capturedEnv []string
	)
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			mu.Lock()
			capturedEnv = append([]string{}, env...)
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
		if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Engine: &engine}); code != 0 {
			t.Errorf("expected exit 0 on accept verdict; got %d", code)
		}
	})

	want := "AS_CLAUDE_WALL_TIMEOUT=10m0s"
	found := false
	for _, e := range capturedEnv {
		if strings.Contains(e, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected env to contain %q; got %v", want, capturedEnv)
	}
}
