package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// TestExecuteStepWithSession_ClaudeStepRefusedWhenPlanNotApproved
// verifies the I-513 gate: a "claude" step on an item with
// PlanApproved=false returns a StepResult.Error pointing at
// `st plan approve` and `st prep`. The plan/test/gate/close types are
// exempt; only LLM-implementation work needs the gate.
func TestExecuteStepWithSession_ClaudeStepRefusedWhenPlanNotApproved(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	writeFile(t, filepath.Join(root, "tasks", "T-100-no-plan.md"), `id: T-100
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: No-plan item

priority: 2

depends_on:
- []

blocks:
- []
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	step := config.RunStepDef{Type: "claude"}
	step.SetName("implement")
	sr := executeStepWithSession(s, cfg, "T-100", "test-sprint", step,
		RunOpts{}, RunEngine{}, "", "", false)

	if sr.Passed {
		t.Errorf("expected Passed=false; got Passed=true")
	}
	if !strings.Contains(sr.Error, "plan not approved") {
		t.Errorf("expected error mentioning 'plan not approved'; got %q", sr.Error)
	}
	if !strings.Contains(sr.Error, "st plan approve") {
		t.Errorf("expected error to mention `st plan approve`; got %q", sr.Error)
	}
}

// TestExecuteStepWithSession_NonClaudeStepNotGated verifies that the
// I-513 gate exempts non-claude step types — close, gate, test, etc.
// can run regardless of plan approval.
func TestExecuteStepWithSession_NonClaudeStepNotGated(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	writeFile(t, filepath.Join(root, "tasks", "T-101-no-plan.md"), `id: T-101
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: No-plan item

priority: 2

depends_on:
- []

blocks:
- []
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// A "test" step (e.g. running st test --run) should be unaffected
	// by plan approval — verification work is read-only.
	step := config.RunStepDef{Type: "test"}
	step.SetName("api_unit")
	sr := executeStepWithSession(s, cfg, "T-101", "test-sprint", step,
		RunOpts{}, RunEngine{}, "", "", false)

	// The test step itself may fail for unrelated reasons (no actual
	// suite runner), but the failure message MUST NOT mention "plan
	// not approved" — that's the symptom of the I-513 gate firing
	// on a non-claude step, which it shouldn't.
	if strings.Contains(sr.Error, "plan not approved") {
		t.Errorf("non-claude step incorrectly gated by plan-approved check; got %q", sr.Error)
	}
}

// TestExecuteStepWithSession_ClaudeStepProceedsWhenPlanApproved
// verifies the positive case: with PlanApproved=true on the item,
// the claude dispatch proceeds past the gate. The actual claude call
// will fail (empty engine), but the failure message comes from the
// claude executor, not the gate.
func TestExecuteStepWithSession_ClaudeStepProceedsWhenPlanApproved(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	writeFile(t, filepath.Join(root, "tasks", "T-102-approved.md"), `id: T-102
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Approved item

priority: 2

depends_on:
- []

blocks:
- []

plan_approved: true
plan_approved_at: 2026-03-25T10:00:00-06:00
plan_approved_by: agent-b
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Stub engine returns a minimal claude result so the dispatcher
	// can complete; we only care that the gate didn't fire.
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return []byte(`{"type":"result","subtype":"success","total_cost_usd":0,"duration_ms":0,"session_id":"stub","result":"done"}`), 0, nil
		},
	}

	step := config.RunStepDef{Type: "claude"}
	step.SetName("implement")
	sr := executeStepWithSession(s, cfg, "T-102", "test-sprint", step,
		RunOpts{NoCoordination: true}, engine, "", "", false)

	// Whatever the downstream claude executor produces, the error
	// MUST NOT be the I-513 gate message — approval is set, so the
	// gate must have allowed the dispatch through.
	if strings.Contains(sr.Error, "plan not approved") {
		t.Errorf("approved item incorrectly gated; got %q", sr.Error)
	}
}
