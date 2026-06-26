package command

import (
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// stubBranchCheck returns a BranchCheck stub that always returns the given value.
func stubBranchCheck(exists bool) func(*config.Config, string) bool {
	return func(*config.Config, string) bool { return exists }
}

// stubPRFetch returns a PRFetch stub returning the given state.
func stubPRFetch(state string) func(*config.Config, string) (string, []string) {
	return func(*config.Config, string) (string, []string) {
		if state == "" {
			return "", nil
		}
		return state, []string{"https://github.com/org/repo/pull/1"}
	}
}

// runInfer is a small test helper that creates a fresh env, seeds the item,
// and invokes InferStage with stubbed signals.
func runInfer(t *testing.T, currentStage string, branchExists bool, prState string) string {
	t.Helper()
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", currentStage)
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	rc := InferStage(s, cfg, "T-001", InferStageOpts{
		BranchCheck: stubBranchCheck(branchExists),
		PRFetch:     stubPRFetch(prState),
	})
	if rc != 0 {
		t.Errorf("InferStage rc = %d, want 0", rc)
	}

	it, _ := s.Get("T-001")
	stage, _ := getNestedField(it, "delivery", "stage")
	return stage
}

// --- Stage matrix ---

func TestInferStage_BranchPushedNoPR(t *testing.T) {
	got := runInfer(t, "coding", true, "")
	if got != "pushed" {
		t.Errorf("coding + branch-on-remote → %q, want pushed", got)
	}
}

func TestInferStage_BranchPushedPROpen(t *testing.T) {
	got := runInfer(t, "coding", true, "OPEN")
	if got != "pr_open" {
		t.Errorf("coding + branch + OPEN PR → %q, want pr_open", got)
	}
}

func TestInferStage_PushedToPROpen(t *testing.T) {
	got := runInfer(t, "pushed", true, "OPEN")
	if got != "pr_open" {
		t.Errorf("pushed + OPEN → %q, want pr_open", got)
	}
}

func TestInferStage_PushedToMerged(t *testing.T) {
	got := runInfer(t, "pushed", true, "MERGED")
	if got != "merged" {
		t.Errorf("pushed + MERGED → %q, want merged", got)
	}
}

func TestInferStage_PROpenToMerged(t *testing.T) {
	got := runInfer(t, "pr_open", true, "MERGED")
	if got != "merged" {
		t.Errorf("pr_open + MERGED → %q, want merged", got)
	}
}

func TestInferStage_MergedNoRegress(t *testing.T) {
	// MERGED PR signal must not regress merged item; it's a no-op advance.
	got := runInfer(t, "merged", true, "MERGED")
	if got != "merged" {
		t.Errorf("merged + MERGED → %q, want merged (no regress)", got)
	}
}

func TestInferStage_DeployedDevNoRegressFromPushed(t *testing.T) {
	// Branch-on-remote signal computes target=pushed; deployed_dev > pushed
	// in stage order, so advanceDeliveryStage must NOT regress.
	got := runInfer(t, "deployed_dev", true, "")
	if got != "deployed_dev" {
		t.Errorf("deployed_dev + branch → %q, want deployed_dev (no regress)", got)
	}
}

// PR-104 review finding #5: deployed_dev / uat_approved boundary regression
// guard. I-447 amend history shows these stages were collapsed at the same
// stageIndex briefly, causing silent no-op advances. Explicit test pins the
// boundary: a deployed_dev item with a MERGED PR signal must stay at
// deployed_dev (not regress to merged, which is earlier in the order).
func TestInferStage_DeployedDevNoRegressFromMerged(t *testing.T) {
	got := runInfer(t, "deployed_dev", true, "MERGED")
	if got != "deployed_dev" {
		t.Errorf("deployed_dev + MERGED → %q, want deployed_dev (no regress past merged)", got)
	}
}

// PR-104 review finding #2: CLOSED PR state must not advance stage.
// Closed-without-merge is reported to stderr by InferStage (symmetric with
// reconcile.go warning) but never touches delivery.stage.
func TestInferStage_ClosedPRDoesNotAdvance(t *testing.T) {
	got := runInfer(t, "pushed", false, "CLOSED")
	if got != "pushed" {
		t.Errorf("pushed + CLOSED → %q, want pushed (no advance on close)", got)
	}
}

// PR-104 review finding #2 follow-on: even if branch is on remote
// (branchExists=true → target=pushed) AND PR is CLOSED, target stays at
// pushed (the CLOSED case in the switch does not overwrite target — it
// only emits the warning). Pins that contract.
func TestInferStage_ClosedPRWithBranchKeepsPushedTarget(t *testing.T) {
	got := runInfer(t, "coding", true, "CLOSED")
	if got != "pushed" {
		t.Errorf("coding + branch + CLOSED → %q, want pushed (branch wins, CLOSED is informational)", got)
	}
}

func TestInferStage_BranchAbsentNoAdvance(t *testing.T) {
	got := runInfer(t, "coding", false, "")
	if got != "coding" {
		t.Errorf("coding + no remote branch → %q, want coding", got)
	}
}

// Renamed from TestInferStage_PROpenNoBranchPreservesStage — that name
// implied a no-regress assertion but the test actually exercises the
// no-signal early-exit (target=="" → return 0 before any forward-only
// logic runs). The honest no-regress assertions are the two below.
func TestInferStage_NoSignalEarlyExit(t *testing.T) {
	got := runInfer(t, "pr_open", false, "")
	if got != "pr_open" {
		t.Errorf("pr_open + no signals → %q, want pr_open (early exit, no mutation)", got)
	}
}

// PR-104 review finding #3: actual no-regress test against a PR signal.
// Branch is gone (branchExists=false), but PR is still OPEN. The PR
// signal would compute target="pr_open" which equals the current stage —
// advanceDeliveryStage no-ops on equal-stage targets, so stage stays at
// pr_open. Without the forward-only guard this would still pass; combine
// with the next test for genuine regression coverage.
func TestInferStage_PROpenSignalEqualsStage(t *testing.T) {
	got := runInfer(t, "pr_open", false, "OPEN")
	if got != "pr_open" {
		t.Errorf("pr_open + OPEN signal → %q, want pr_open (target equals current)", got)
	}
}

// PR-104 review finding #3: forward-only guard exercised. Item is at
// `merged`; PR signal of OPEN would compute target="pr_open" which is
// EARLIER in the stage order. advanceDeliveryStage must NOT regress.
func TestInferStage_MergedDoesNotRegressToPROpen(t *testing.T) {
	got := runInfer(t, "merged", false, "OPEN")
	if got != "merged" {
		t.Errorf("merged + OPEN signal → %q, want merged (forward-only guard)", got)
	}
}

// --- Edge cases ---

func TestInferStage_EmptyIDEmptyStackNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rc := InferStage(s, cfg, "", InferStageOpts{
		BranchCheck: stubBranchCheck(true),
		PRFetch:     stubPRFetch("OPEN"),
	})
	if rc != 0 {
		t.Errorf("empty id + empty stack rc = %d, want 0", rc)
	}
}

func TestInferStage_UnknownIDNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rc := InferStage(s, cfg, "T-999-nope", InferStageOpts{
		BranchCheck: stubBranchCheck(true),
		PRFetch:     stubPRFetch("OPEN"),
	})
	if rc != 0 {
		t.Errorf("unknown id rc = %d, want 0", rc)
	}
}

func TestInferStage_NoBranchOnItemNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-001 has no work_tracking.branch by default in the fixture.
	rc := InferStage(s, cfg, "T-001", InferStageOpts{
		BranchCheck: stubBranchCheck(true),
		PRFetch:     stubPRFetch("OPEN"),
	})
	if rc != 0 {
		t.Errorf("no branch rc = %d, want 0", rc)
	}
	it, _ := s.Get("T-001")
	stage, _ := getNestedField(it, "delivery", "stage")
	if stage == "pushed" || stage == "pr_open" || stage == "merged" {
		t.Errorf("no branch should not advance, got stage = %q", stage)
	}
}

func TestInferStage_StackTopResolvedWhenIDOmitted(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Seed stage + branch on T-001
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	// Push T-001 onto the stack so the resolver finds it.
	if rc := StackPush(s, cfg, "T-001", StackPushOpts{Reason: "test"}); rc != 0 {
		t.Fatalf("StackPush rc = %d", rc)
	}

	rc := InferStage(s, cfg, "", InferStageOpts{
		BranchCheck: stubBranchCheck(true),
		PRFetch:     stubPRFetch(""),
	})
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}

	it, _ := s.Get("T-001")
	stage, _ := getNestedField(it, "delivery", "stage")
	if stage != "pushed" {
		t.Errorf("stack-top resolution: stage = %q, want pushed", stage)
	}
}
