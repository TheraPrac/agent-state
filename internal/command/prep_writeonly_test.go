package command

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TestPrepWriteOnlyReviewBypassesIdempotencySkip: an explicit --review on an
// already-drafted item must run the review sub-agent, even though the
// plan_written_at idempotency guard would otherwise skip it (I-933 review
// finding — the guard must yield to an explicit --review).
func TestPrepWriteOnlyReviewBypassesIdempotencySkip(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	// First pass: draft T-001 without --review → stamps plan_written_at, no report.
	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true, ItemFilter: "T-001"}, engine)
	})
	if plan.ReportExists(cfg.PlansDir(), "T-001") {
		t.Fatal("no-review prep must not write a report")
	}
	before := reviewCalls.get()

	// Second pass: same item WITH --review must NOT be skipped.
	s, _ = store.New(cfg)
	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true, Review: true, ItemFilter: "T-001"}, engine)
	})
	if reviewCalls.get() == before {
		t.Error("--review on an already-drafted item must run the review sub-agent (idempotency skip must yield to --review)")
	}
	if !plan.ReportExists(cfg.PlansDir(), "T-001") {
		t.Error("--review run should write the .report.md")
	}
}

// setupPrepWriteOnlyEnv creates a fixture sprint with two unplanned
// items, neither having a plan sidecar. Mirrors setupRunTestEnv but
// the run pipeline is omitted (prep doesn't need it).
func setupPrepWriteOnlyEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".plans"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .

run:
  permission_mode: dangerously-skip-permissions
  default_model: sonnet
  max_parallelism: 1
  default_budget_usd: 2.00
  step_order: [implement, close]
  steps:
    implement:
      type: claude
    close:
      type: close
      resolution: completed
`), 0644); err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{}
	reg.Epics = append(reg.Epics, registry.Epic{
		ID: "wo-epic", Title: "Write-Only Epic", Status: "active",
	})
	reg.Sprints = append(reg.Sprints, registry.Sprint{
		ID: "wo-sprint", Title: "Write-Only Sprint", Epic: "wo-epic",
		Status: "active", Items: []string{"T-001", "T-002"},
	})
	if err := reg.Save(filepath.Join(root, ".as", "epics.yaml")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "tasks", "T-001-alpha.md"), `id: T-001
type: task
status: queued
created: 2026-05-09T10:00:00-06:00
last_touched: 2026-05-09T10:00:00-06:00

completed: null

title: Alpha task

summary: Alpha summary

acceptance_criteria:
- cmd: echo alpha

depends_on:
- []

sprint: wo-sprint
`)

	writeFile(t, filepath.Join(root, "tasks", "T-002-beta.md"), `id: T-002
type: task
status: queued
created: 2026-05-09T10:00:00-06:00
last_touched: 2026-05-09T10:00:00-06:00

completed: null

title: Beta task

summary: Beta summary

acceptance_criteria:
- cmd: echo beta

depends_on:
- []

sprint: wo-sprint
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

// cannedPlanText is a syntactically valid, substance-complete plan body that
// plan.Parse can consume and Save will accept. It carries every section
// quality.ValidatePlan requires (Approach, Scope, Tests, Out-of-scope, Risks)
// so the interactive Accept path (prepItem) passes the substance gate instead
// of looping back to the menu forever when a stub SelectMenu keeps choosing
// Accept.
const cannedPlanText = `## Approach
Canned plan body for the write-only test.

## Scope
Repos: as

## Implementation Steps
1. Step one
2. Step two

## Files to Create
- new.go

## Files to Modify
- main.go

## Tests
Unit: go test ./internal/foo -run TestBar covers the change.

## Out-of-scope
None.

## Risks
None — low-risk test fixture.

## Acceptance Criteria
- cmd: go test ./internal/foo -run TestBar
`

const cannedReportText = `## Recommendation
Accept

## Notes
- Confidence: high
- Files reviewed: 12
`

// makeWriteOnlyEngine returns a RunEngine whose RunClaude returns
// canned plan output for "prep" calls and canned review output for
// "plan_review" calls. Distinguishes by the ST_RUN_STEP env var that
// prep + executeClaude already set on the subprocess. The interactive
// hooks are wired to recorders so tests can assert non-invocation.
type recorder struct {
	mu    sync.Mutex
	count int
}

func (r *recorder) bump() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
}

func (r *recorder) get() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func makeWriteOnlyEngine(promptRec, menuRec, confirmRec *recorder, prepFails int) (engine RunEngine, prepCalls, reviewCalls *recorder) {
	prepCalls = &recorder{}
	reviewCalls = &recorder{}
	engine = RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			step := "prep"
			for _, e := range env {
				if strings.HasPrefix(e, "ST_RUN_STEP=") {
					step = strings.TrimPrefix(e, "ST_RUN_STEP=")
				}
			}
			if step == "plan_review" {
				reviewCalls.bump()
				result := ClaudeResult{
					Type: "result", Subtype: "success",
					Result: cannedReportText,
				}
				data, _ := json.Marshal(result)
				return data, 0, nil
			}
			prepCalls.bump()
			if prepFails > 0 && prepCalls.get() <= prepFails {
				return nil, 1, errors.New("simulated claude failure")
			}
			result := ClaudeResult{
				Type: "result", Subtype: "success",
				Result: cannedPlanText,
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(p string) (string, error) {
			if promptRec != nil {
				promptRec.bump()
			}
			return "", nil
		},
		SelectMenu: func(p string, opts []menuOption, def int) string {
			if menuRec != nil {
				menuRec.bump()
			}
			if len(opts) > 0 {
				return opts[0].Key
			}
			return ""
		},
		ConfirmPrompt: func(p string) bool {
			if confirmRec != nil {
				confirmRec.bump()
			}
			return false
		},
	}
	return engine, prepCalls, reviewCalls
}

// suppressStdout discards stdout for the duration of fn so test
// output isn't drowned by prep's progress prints.
func suppressStdout(t *testing.T, fn func()) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	defer func() {
		w.Close()
		<-done
		os.Stdout = orig
	}()
	fn()
}

// TestPrepWriteOnlyProducesBothFiles: --write-only writes both the
// plan sidecar (with plan_approved=false) and the report sidecar.
func TestPrepWriteOnlyProducesBothFiles(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var code int
	suppressStdout(t, func() {
		// I-933: report + review are opt-in now; this test asserts both files.
		code = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true, Review: true}, engine)
	})
	if code != 0 {
		t.Fatalf("Prep returned %d, want 0", code)
	}

	for _, id := range []string{"T-001", "T-002"} {
		if !plan.Exists(cfg.PlansDir(), id) {
			t.Errorf("%s: plan sidecar missing", id)
			continue
		}
		p, err := plan.Load(cfg.PlansDir(), id)
		if err != nil || p == nil {
			t.Errorf("%s: plan.Load: %v / %v", id, p, err)
			continue
		}
		if p.Approved {
			t.Errorf("%s: plan should be saved as draft (Approved=false), got Approved=true", id)
		}
		if !plan.ReportExists(cfg.PlansDir(), id) {
			t.Errorf("%s: report sidecar missing", id)
			continue
		}
		body, err := plan.LoadReport(cfg.PlansDir(), id)
		if err != nil {
			t.Errorf("%s: LoadReport: %v", id, err)
		}
		if !strings.Contains(body, "Recommendation") {
			t.Errorf("%s: report body missing expected content; got %q", id, body)
		}
	}
	if reviewCalls.get() != 2 {
		t.Errorf("review subprocess invocations = %d, want 2", reviewCalls.get())
	}
}

// TestPrepWriteOnlyNoInteractivePrompt: PromptUser, SelectMenu, and
// ConfirmPrompt are never invoked when WriteOnly=true.
func TestPrepWriteOnlyNoInteractivePrompt(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	promptRec, menuRec, confirmRec := &recorder{}, &recorder{}, &recorder{}
	engine, _, _ := makeWriteOnlyEngine(promptRec, menuRec, confirmRec, 0)

	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
	})

	if got := promptRec.get(); got != 0 {
		t.Errorf("PromptUser invoked %d times, want 0", got)
	}
	if got := menuRec.get(); got != 0 {
		t.Errorf("SelectMenu invoked %d times, want 0", got)
	}
	if got := confirmRec.get(); got != 0 {
		t.Errorf("ConfirmPrompt invoked %d times, want 0", got)
	}
}

// TestPrepWriteOnlyIdempotent: pre-creating both sidecars for one
// item causes that item to be skipped (no LLM call) on the next run,
// while a sibling unplanned item still gets prepped.
func TestPrepWriteOnlyIdempotent(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)

	// Pre-populate both sidecars for T-001.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Pre-existing draft.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: echo ok"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := plan.SaveReport(cfg.PlansDir(), "T-001", "pre-existing report"); err != nil {
		t.Fatal(err)
	}
	// I-933: "already prepped" is signaled by plan_written_at (stamped by a
	// completed prep run), not by the report sidecar. Stamp it so the
	// idempotency guard skips T-001.
	stampPrepSuccess(s, "T-001")

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)
	suppressStdout(t, func() {
		// No --review: exercises the idempotency skip (an explicit --review
		// intentionally bypasses it — see TestPrepWriteOnlyReviewBypassesIdempotencySkip).
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
	})

	// Only T-002 should have been planned: 1 prep call, no review (opt-in off).
	if got := prepCalls.get(); got != 1 {
		t.Errorf("prep RunClaude calls = %d, want 1 (T-001 should be skipped)", got)
	}
	if got := reviewCalls.get(); got != 0 {
		t.Errorf("review RunClaude calls = %d, want 0 (review is opt-in)", got)
	}

	// T-001's pre-existing sidecars are untouched.
	body, _ := plan.LoadReport(cfg.PlansDir(), "T-001")
	if body != "pre-existing report" {
		t.Errorf("T-001 report was overwritten; got %q", body)
	}

	// T-002 has a fresh draft (no report without --review).
	if !plan.Exists(cfg.PlansDir(), "T-002") {
		t.Error("T-002 draft missing — sibling should still be prepped")
	}
}

// TestPrepWriteOnlyContinuesAfterFailure: when the engine fails on
// the first item's prep call, the second item is still prepped and
// the run exits 0.
func TestPrepWriteOnlyContinuesAfterFailure(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 1) // first prep call fails

	output := captureStdout(t, func() {
		// I-933: review/report opt-in.
		code := Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true, Review: true}, engine)
		if code != 0 {
			t.Errorf("Prep returned %d, want 0 (per-item failure must not abort)", code)
		}
	})

	if !strings.Contains(output, "FAILED:") {
		t.Errorf("expected FAILED line in output; got: %s", output)
	}

	// First item's sidecars: must NOT exist (failed before any save).
	if plan.Exists(cfg.PlansDir(), "T-001") {
		t.Error("T-001 plan should not exist after failed prep")
	}
	if plan.ReportExists(cfg.PlansDir(), "T-001") {
		t.Error("T-001 report should not exist after failed prep")
	}

	// Second item: both sidecars present.
	if !plan.Exists(cfg.PlansDir(), "T-002") {
		t.Error("T-002 plan missing — run should have continued past T-001 failure")
	}
	if !plan.ReportExists(cfg.PlansDir(), "T-002") {
		t.Error("T-002 report missing — run should have continued past T-001 failure")
	}
}

// TestPlanShowPrintsPlanAndReportContent: PlanShow inlines both the
// plan body and the report body when sidecars exist.
func TestPlanShowPrintsPlanAndReportContent(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)

	planBodyMarker := "Canned plan for plan-show test"
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   planBodyMarker,
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: echo ok"},
	}); err != nil {
		t.Fatal(err)
	}
	reportBodyMarker := "Plan-review report body marker text"
	if err := plan.SaveReport(cfg.PlansDir(), "T-001", reportBodyMarker); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() {
		_ = PlanShow(s, cfg, "T-001")
	})

	if !strings.Contains(output, "T-001") {
		t.Errorf("output missing item id; got: %s", output)
	}
	if !strings.Contains(output, "=== Plan: .plans/T-001.md ===") {
		t.Errorf("output missing plan-body header; got: %s", output)
	}
	if !strings.Contains(output, planBodyMarker) {
		t.Errorf("output missing plan body marker %q; got: %s", planBodyMarker, output)
	}
	if !strings.Contains(output, "=== Report: .plans/T-001.report.md ===") {
		t.Errorf("output missing report header; got: %s", output)
	}
	if !strings.Contains(output, reportBodyMarker) {
		t.Errorf("output missing report body marker %q; got: %s", reportBodyMarker, output)
	}
}

// TestPrepInteractiveWallTimeoutInjected asserts that the interactive
// (non-write-only) prepItem path also injects AS_CLAUDE_WALL_TIMEOUT.
// I-985: the write-only and interactive code paths share the same
// resolvePrepTimeout() call but are in independent branches; a regression
// in one would not be caught by a test of the other.
func TestPrepInteractiveWallTimeoutInjected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PREP_TIMEOUT", "")
	s, cfg := setupPrepWriteOnlyEnv(t)

	var (
		mu           sync.Mutex
		capturedEnvs [][]string
	)
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			mu.Lock()
			capturedEnvs = append(capturedEnvs, append([]string{}, env...))
			mu.Unlock()
			result := ClaudeResult{
				Type: "result", Subtype: "success",
				Result: cannedPlanText,
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		// Reject immediately so the interactive review loop exits without blocking.
		PromptUser:    func(p string) (string, error) { return "", nil },
		SelectMenu:    func(p string, opts []menuOption, def int) string { return "2" },
		ConfirmPrompt: func(p string) bool { return false },
	}

	suppressOutput(t, func() {
		// WriteOnly: false → prepItem (interactive path)
		Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: false}, engine)
	})

	want := "AS_CLAUDE_WALL_TIMEOUT=10m0s"
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
		t.Errorf("expected at least one RunClaude invocation with env containing %q; interactive path invocations: %v", want, capturedEnvs)
	}
}

// TestPrepWallTimeoutInjected asserts that when AS_PREP_TIMEOUT is unset,
// the prep sub-agent receives AS_CLAUDE_WALL_TIMEOUT=10m0s in its env.
// This is the I-985 gate: without it, the subprocess inherits the global
// 2h wall cap and hangs undetected when a tool call never returns.
func TestPrepWallTimeoutInjected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_PREP_TIMEOUT", "")
	s, cfg := setupPrepWriteOnlyEnv(t)

	var (
		mu           sync.Mutex
		capturedEnvs [][]string
	)
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			mu.Lock()
			capturedEnvs = append(capturedEnvs, append([]string{}, env...))
			mu.Unlock()
			result := ClaudeResult{
				Type: "result", Subtype: "success",
				Result: cannedPlanText,
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser:    func(p string) (string, error) { return "q", nil },
		SelectMenu:    func(p string, opts []menuOption, def int) string { return "q" },
		ConfirmPrompt: func(p string) bool { return false },
	}

	suppressOutput(t, func() {
		Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
	})

	want := "AS_CLAUDE_WALL_TIMEOUT=10m0s"
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


// TestPrepWriteOnlyStampsCompletionTimestamp: a successful --write-only run
// stamps plan_written_at on the item and leaves plan_failed_at empty. I-833.
func TestPrepWriteOnlyStampsCompletionTimestamp(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 0)

	suppressStdout(t, func() {
		code := Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
		if code != 0 {
			t.Errorf("Prep returned %d, want 0", code)
		}
	})

	// Reload store to pick up Mutate writes.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"T-001", "T-002"} {
		item, ok := s2.Get(id)
		if !ok {
			t.Fatalf("%s: not found after prep", id)
		}
		if item.PlanWrittenAt == "" {
			t.Errorf("%s: plan_written_at is empty after successful write-only prep", id)
		}
		if item.PlanFailedAt != "" {
			t.Errorf("%s: plan_failed_at = %q, want empty on success", id, item.PlanFailedAt)
		}
		if item.PlanFailureReason != "" {
			t.Errorf("%s: plan_failure_reason = %q, want empty on success", id, item.PlanFailureReason)
		}
	}
}

// TestPrepWriteOnlyStampsFailureTimestamp: when the engine fails, the item
// gets plan_failed_at + plan_failure_reason and plan_written_at stays empty.
// I-833.
func TestPrepWriteOnlyStampsFailureTimestamp(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	// First prep call (T-001) fails; second (T-002) succeeds.
	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 1)

	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
	})

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// T-001: failed → plan_failed_at set, plan_written_at empty.
	failed, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001: not found")
	}
	if failed.PlanFailedAt == "" {
		t.Error("T-001: plan_failed_at is empty after failed prep")
	}
	if failed.PlanFailureReason == "" {
		t.Error("T-001: plan_failure_reason is empty after failed prep")
	}
	if failed.PlanWrittenAt != "" {
		t.Errorf("T-001: plan_written_at = %q, want empty on failure", failed.PlanWrittenAt)
	}

	// T-002: succeeded → plan_written_at set, plan_failed_at empty.
	succeeded, ok := s2.Get("T-002")
	if !ok {
		t.Fatal("T-002: not found")
	}
	if succeeded.PlanWrittenAt == "" {
		t.Error("T-002: plan_written_at is empty after successful prep")
	}
	if succeeded.PlanFailedAt != "" {
		t.Errorf("T-002: plan_failed_at = %q, want empty on success", succeeded.PlanFailedAt)
	}
}

// TestPrepWriteOnlyStampsFailureOnReviewError: when the plan-review subprocess
// returns an error, the item gets plan_failed_at stamped. I-833.
func TestPrepWriteOnlyStampsFailureOnReviewError(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)

	// Engine: prep succeeds; plan_review errors.
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			for _, e := range env {
				if strings.HasPrefix(e, "ST_RUN_STEP=plan_review") {
					return nil, 1, errors.New("simulated review failure")
				}
			}
			result := ClaudeResult{Type: "result", Subtype: "success", Result: cannedPlanText}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser:    func(p string) (string, error) { return "", nil },
		SelectMenu:    func(p string, opts []menuOption, def int) string { return "" },
		ConfirmPrompt: func(p string) bool { return false },
	}

	suppressStdout(t, func() {
		// I-933: review is opt-in; this test exercises the review-error path.
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true, Review: true}, engine)
	})

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"T-001", "T-002"} {
		item, ok := s2.Get(id)
		if !ok {
			t.Fatalf("%s: not found", id)
		}
		if item.PlanFailedAt == "" {
			t.Errorf("%s: plan_failed_at empty after review error", id)
		}
		if item.PlanFailureReason == "" {
			t.Errorf("%s: plan_failure_reason empty after review error", id)
		}
		if item.PlanWrittenAt != "" {
			t.Errorf("%s: plan_written_at = %q, want empty on review error", id, item.PlanWrittenAt)
		}
	}
}

// TestPrepStandaloneReturnsOneOnWriteOnlyFailure: PrepStandalone returns 1
// when --write-only prep fails (not the old silent-0 behavior). I-833.
func TestPrepStandaloneReturnsOneOnWriteOnlyFailure(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 99) // all prep calls fail

	var code int
	suppressStdout(t, func() {
		code = PrepStandalone(s, cfg, "T-001", PrepOpts{WriteOnly: true}, engine)
	})
	if code == 0 {
		t.Errorf("PrepStandalone returned 0 on failure, want non-zero")
	}
}
