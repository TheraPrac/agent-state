package command

import (
	"encoding/json"
	"errors"
	"fmt"
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

// cannedPlanText is a syntactically valid plan body that plan.Parse
// can consume and Save will accept.
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
		code = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
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

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)
	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
	})

	// Only T-002 should have been planned: 1 prep call + 1 review call.
	if got := prepCalls.get(); got != 1 {
		t.Errorf("prep RunClaude calls = %d, want 1 (T-001 should be skipped)", got)
	}
	if got := reviewCalls.get(); got != 1 {
		t.Errorf("review RunClaude calls = %d, want 1 (T-001 should be skipped)", got)
	}

	// T-001's pre-existing sidecars are untouched.
	body, _ := plan.LoadReport(cfg.PlansDir(), "T-001")
	if body != "pre-existing report" {
		t.Errorf("T-001 report was overwritten; got %q", body)
	}

	// T-002 has both new sidecars.
	if !plan.ReportExists(cfg.PlansDir(), "T-002") {
		t.Error("T-002 report missing — sibling should still be prepped")
	}
}

// TestPrepWriteOnlyContinuesAfterFailure: when the engine fails on
// the first item's prep call, the second item is still prepped and
// the run exits 0.
func TestPrepWriteOnlyContinuesAfterFailure(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 1) // first prep call fails

	output := captureStdout(t, func() {
		code := Prep(s, cfg, "wo-sprint", PrepOpts{WriteOnly: true}, engine)
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

// silence unused-import warnings if we ever compile minimally.
var _ = fmt.Sprintf
