package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

func setupRunTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	// Config with run pipeline
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .

run:
  permission_mode: dangerously-skip-permissions
  default_model: sonnet
  max_parallelism: 1
  default_budget_usd: 2.00
  step_order: [implement, merge, uat, approval, close]
  steps:
    implement:
      type: claude
    merge:
      type: command
      command: echo merged
    uat:
      type: command
      command: echo uat-pass
    approval:
      type: gate
    close:
      type: close
      resolution: completed
`), 0644)

	// Sprint registry
	reg := &registry.Registry{}
	reg.Epics = append(reg.Epics, registry.Epic{
		ID: "test-epic", Title: "Test Epic", Status: "active",
	})
	reg.Sprints = append(reg.Sprints, registry.Sprint{
		ID: "test-sprint", Title: "Test Sprint", Epic: "test-epic",
		Status: "active", Items: []string{"T-001", "T-002"},
		PlanApproved: true,
	})
	reg.Save(filepath.Join(root, ".as", "epics.yaml"))

	// Items
	writeFile(t, filepath.Join(root, "tasks", "T-001-alpha.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Alpha task

summary: Implement alpha feature

acceptance_criteria:
- Feature works
- Tests pass

depends_on:
- []

sprint: test-sprint
`)

	writeFile(t, filepath.Join(root, "tasks", "T-002-beta.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Beta task

depends_on:
- T-001

sprint: test-sprint
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

func mockRunEngine(approved bool) RunEngine {
	return RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			result := ClaudeResult{
				Type:         "result",
				Subtype:      "success",
				TotalCostUSD: 0.05,
				DurationMs:   15000,
				Result:       "Implementation complete",
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			if approved {
				return "y\n", nil
			}
			return "n\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			if len(options) > 0 {
				return options[0].Key
			}
			return ""
		},
		ConfirmPrompt: func(prompt string) bool {
			return approved
		},
	}
}

func TestRunDryRun(t *testing.T) {
	s, cfg := setupRunTestEnv(t)
	opts := RunOpts{DryRun: true}
	code := Run(s, cfg, "test-sprint", opts, mockRunEngine(true))
	if code != 0 {
		t.Errorf("dry-run returned %d, want 0", code)
	}
}

func TestRunInteractiveShowsSprints(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// Mock engine that selects sprint 1 then approves
	callCount := 0
	engine := RunEngine{
		RunClaude: mockRunEngine(true).RunClaude,
		PromptUser: func(prompt string) (string, error) {
			callCount++
			return "1\n", nil
		},
	}

	// dry-run so we don't actually execute
	code := RunInteractive(s, cfg, RunOpts{DryRun: true}, engine)
	if code != 0 {
		t.Errorf("interactive dry-run returned %d, want 0", code)
	}
}

func TestRunInteractiveNoSprints(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .
run:
  step_order: [implement]
  steps:
    implement:
      type: claude
`), 0644)
	// Empty registry
	reg := &registry.Registry{}
	reg.Save(filepath.Join(root, ".as", "epics.yaml"))

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	code := RunInteractive(s, cfg, RunOpts{}, mockRunEngine(true))
	if code != 0 {
		t.Errorf("expected 0 for no sprints, got %d", code)
	}
}

func TestRunInteractivePlanApproval(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// Create unapproved sprint
	reg, _ := registry.Load(cfg.EpicsPath())
	reg.Sprints = append(reg.Sprints, registry.Sprint{
		ID: "needs-approval", Title: "Needs Approval", Epic: "test-epic",
		Status: "active", Items: []string{"T-001"},
		PlanApproved: false,
	})
	reg.Save(cfg.EpicsPath())

	// Mock: select sprint 2 (needs-approval), then approve plan
	engine := RunEngine{
		RunClaude: mockRunEngine(true).RunClaude,
		PromptUser: func(prompt string) (string, error) {
			return "y\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			return "2" // select second sprint
		},
		ConfirmPrompt: func(prompt string) bool {
			return true // approve plan
		},
	}

	code := RunInteractive(s, cfg, RunOpts{DryRun: true}, engine)
	if code != 0 {
		t.Errorf("interactive with approval returned %d, want 0", code)
	}

	// Verify plan was approved
	reg2, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg2.SprintByID("needs-approval")
	if !sp.PlanApproved {
		t.Error("expected plan to be approved after interactive flow")
	}
}

func TestRunSprintNotFound(t *testing.T) {
	s, cfg := setupRunTestEnv(t)
	code := Run(s, cfg, "nonexistent", RunOpts{}, mockRunEngine(true))
	if code != 1 {
		t.Errorf("expected exit 1 for missing sprint, got %d", code)
	}
}

func TestRunPlanNotApproved(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// Create unapproved sprint
	reg, _ := registry.Load(cfg.EpicsPath())
	reg.Sprints = append(reg.Sprints, registry.Sprint{
		ID: "unapproved", Title: "No Plan", Epic: "test-epic",
		Status: "active", Items: []string{"T-001"},
		PlanApproved: false,
	})
	reg.Save(cfg.EpicsPath())

	code := Run(s, cfg, "unapproved", RunOpts{}, mockRunEngine(true))
	if code != 1 {
		t.Errorf("expected exit 1 for unapproved plan, got %d", code)
	}
}

func TestRunNoPipeline(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	code := Run(s, cfg, "anything", RunOpts{}, mockRunEngine(true))
	if code != 1 {
		t.Errorf("expected exit 1 for no pipeline, got %d", code)
	}
}

func TestAdvanceDryRun(t *testing.T) {
	s, cfg := setupRunTestEnv(t)
	code := Advance(s, cfg, "test-sprint", RunOpts{DryRun: true}, mockRunEngine(true))
	if code != 0 {
		t.Errorf("advance dry-run returned %d, want 0", code)
	}
}

func TestAdvanceNoItems(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// Make both items terminal
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "done")
		it.Status = "done"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}
	if err := s.Mutate("T-002", func(it *model.Item) error {
		it.Doc.SetField("status", "done")
		it.Status = "done"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-002: %v", err)
	}

	code := Advance(s, cfg, "test-sprint", RunOpts{}, mockRunEngine(true))
	if code != 0 {
		t.Errorf("expected 0 for no remaining items, got %d", code)
	}
}

func TestBuildClaudeArgs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Run = &config.RunConfig{
		PermissionMode:   "dangerously-skip-permissions",
		DefaultModel:     "sonnet",
		DefaultBudgetUSD: 5.0,
	}

	args := buildClaudeArgs(cfg, "test prompt", RunOpts{}, "/tmp/wt")

	// Check key args are present
	found := map[string]bool{}
	for i, a := range args {
		if a == "-p" {
			found["print"] = true
		}
		if a == "--dangerously-skip-permissions" {
			found["perms"] = true
		}
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "stream-json" {
			found["json"] = true
		}
		if a == "--model" && i+1 < len(args) && args[i+1] == "sonnet" {
			found["model"] = true
		}
		if a == "--max-budget-usd" {
			found["budget"] = true
		}
	}

	for _, key := range []string{"print", "perms", "json", "model", "budget"} {
		if !found[key] {
			t.Errorf("missing expected arg: %s (args: %v)", key, args)
		}
	}
}

func TestBuildClaudeArgsOverrides(t *testing.T) {
	cfg := &config.Config{}
	cfg.Run = &config.RunConfig{
		PermissionMode: "dangerously-skip-permissions",
		DefaultModel:   "sonnet",
	}

	// Override model via opts
	args := buildClaudeArgs(cfg, "test", RunOpts{Model: "opus"}, "/tmp")
	for i, a := range args {
		if a == "--model" && i+1 < len(args) && args[i+1] != "opus" {
			t.Errorf("model override not applied: got %s", args[i+1])
		}
	}
}

func TestParseClaudeOutput(t *testing.T) {
	result := ClaudeResult{
		Type:         "result",
		Subtype:      "success",
		TotalCostUSD: 0.12,
		Result:       "Done",
	}
	data, _ := json.Marshal(result)

	parsed, err := parseClaudeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subtype != "success" {
		t.Errorf("subtype = %q, want success", parsed.Subtype)
	}
	if parsed.TotalCostUSD != 0.12 {
		t.Errorf("cost = %f, want 0.12", parsed.TotalCostUSD)
	}
}

func TestParseClaudeOutputWithPrefix(t *testing.T) {
	// Claude may output progress text before the JSON
	input := `Processing...
Still working...
{"type":"result","subtype":"success","cost_usd":0.05,"result":"Done"}`

	parsed, err := parseClaudeOutput([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subtype != "success" {
		t.Errorf("subtype = %q, want success", parsed.Subtype)
	}
}

func TestParseClaudeOutputEmpty(t *testing.T) {
	_, err := parseClaudeOutput([]byte(""))
	if err == nil {
		t.Error("expected error for empty output")
	}
}

func TestExpandTemplate(t *testing.T) {
	cfg := &config.Config{}
	result := expandTemplate("item {id} in sprint {sprint} at {worktree}", "T-001", "sp-1", "/tmp/wt", cfg)
	if result != "item T-001 in sprint sp-1 at /tmp/wt" {
		t.Errorf("template expansion: %q", result)
	}
}

func TestSlugFromTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Alpha task", "alpha-task"},
		{"Fix bug #123", "fix-bug-123"},
		{"A very long title that should be truncated at forty characters plus more", "a-very-long-title-that-should-be-truncat"},
		{"--leading--dashes--", "leading-dashes"},
	}
	for _, tt := range tests {
		got := slugFromTitle(tt.input)
		if got != tt.want {
			t.Errorf("slugFromTitle(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerateSessionID(t *testing.T) {
	id := generateSessionID()
	if len(id) < 30 {
		t.Errorf("session ID too short: %q", id)
	}
	// Should be unique
	id2 := generateSessionID()
	if id == id2 {
		t.Error("two session IDs are identical")
	}
}

func TestBuildDefaultPrompt(t *testing.T) {
	s, cfg := setupRunTestEnv(t)
	prompt := buildDefaultPrompt(s, cfg, "T-001", "test-sprint")

	// Should contain key elements
	for _, substr := range []string{"T-001", "Alpha task", "Acceptance Criteria", "BEFORE committing", "Do NOT merge"} {
		if !strings.Contains(prompt, substr) {
			t.Errorf("prompt missing %q", substr)
		}
	}
}

// TestMetricsAccumulation proves that AI cost, AI duration, sessions, and
// run_count accumulate correctly across multiple st run invocations, and
// that st close produces the right human-readable totals.
func TestMetricsAccumulation(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	// Config: simple pipeline with two claude steps (implement + code_review)
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .

run:
  permission_mode: dangerously-skip-permissions
  default_model: sonnet
  max_parallelism: 1
  default_budget_usd: 2.00
  step_order: [implement, code_review, approval, close]
  steps:
    implement:
      type: claude
    code_review:
      type: claude
      prompt: "Review {id}"
    approval:
      type: gate
    close:
      type: close
      resolution: completed
`), 0644)

	// Sprint
	reg := &registry.Registry{}
	reg.Epics = append(reg.Epics, registry.Epic{
		ID: "test-epic", Title: "Test", Status: "active",
	})
	reg.Sprints = append(reg.Sprints, registry.Sprint{
		ID: "metrics-sprint", Title: "Metrics Test", Epic: "test-epic",
		Status: "active", Items: []string{"T-010"},
		PlanApproved: true,
	})
	reg.Save(filepath.Join(root, ".as", "epics.yaml"))

	// Item — already active (skip st start which needs git worktrees)
	writeFile(t, filepath.Join(root, "tasks", "T-010-metrics.md"), `id: T-010
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Metrics test item

depends_on:
- []

sprint: metrics-sprint

time_tracking:
  started_at: 2026-03-29T10:00:00-06:00
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Mock engine: implement costs $0.08, code_review costs $0.03
	// Auto-approve gate
	callNum := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			callNum++
			cost := 0.08
			if callNum%2 == 0 {
				cost = 0.03 // code_review
			}
			result := ClaudeResult{
				Type:         "result",
				Subtype:      "success",
				TotalCostUSD: cost,
				DurationMs:   15000,
				SessionID:    fmt.Sprintf("session-%d", callNum),
				Result:       "done",
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "y\n", nil
		},
	}

	// --- Run 1: advance through implement + code_review only (stop before gate) ---
	s, _ := store.New(cfg)
	opts := RunOpts{
		ItemFilter: "T-010",
		StepFilter: "code_review", // stop after code_review, before gate/close
	}
	code := Advance(s, cfg, "metrics-sprint", opts, engine)
	if code != 0 {
		t.Fatalf("advance run 1 returned %d", code)
	}

	// Verify metrics after run 1
	s1, _ := store.New(cfg)
	item1, _ := s1.Get("T-010")

	aiCost1, _ := getNestedField(item1, "time_tracking", "ai_cost_usd")
	if aiCost1 == "" {
		t.Fatal("ai_cost_usd not set after run 1")
	}
	var cost1 float64
	fmt.Sscanf(aiCost1, "%f", &cost1)
	if cost1 < 0.10 { // implement ($0.08) + code_review ($0.03) = $0.11
		t.Errorf("run 1 ai_cost_usd = %s, expected >= 0.10", aiCost1)
	}

	// With the SessionLog rewire, turn_count increments per Claude step, not
	// per st run invocation. This test runs 2 Claude steps (implement +
	// code_review) per invocation, so turn_count is 2 after run 1.
	turnCount1, _ := getNestedField(item1, "time_tracking", "turn_count")
	if turnCount1 != "2" {
		t.Errorf("turn_count after run 1 = %q, want 2", turnCount1)
	}

	// Check sessions were recorded
	if len(item1.Sessions) < 2 {
		t.Errorf("expected >= 2 sessions after run 1, got %d", len(item1.Sessions))
	}

	// --- Run 2: advance again (another implement + code_review) ---
	callNum = 10      // reset to get different session IDs
	opts.Fresh = true // force re-execution of completed steps
	code2 := Advance(s, cfg, "metrics-sprint", opts, engine)
	// This will fail at "start" since item is already active, but the claude
	// steps should still execute. Actually — item is already active, so it
	// skips st start and goes straight to pipeline.
	_ = code2

	// Verify accumulation after run 2
	s2, _ := store.New(cfg)
	item2, _ := s2.Get("T-010")

	aiCost2, _ := getNestedField(item2, "time_tracking", "ai_cost_usd")
	var cost2 float64
	fmt.Sscanf(aiCost2, "%f", &cost2)
	if cost2 <= cost1 {
		t.Errorf("ai_cost_usd did not accumulate: run1=%f, run2=%f", cost1, cost2)
	}

	// 2 steps × 2 runs = 4 turn logs
	turnCount2, _ := getNestedField(item2, "time_tracking", "turn_count")
	if turnCount2 != "4" {
		t.Errorf("turn_count after run 2 = %q, want 4", turnCount2)
	}

	// Check ai_sessions array grew
	if len(item2.Sessions) <= len(item1.Sessions) {
		t.Errorf("sessions did not grow: run1=%d, run2=%d", len(item1.Sessions), len(item2.Sessions))
	}

	// --- Close and verify human-readable totals ---
	s3, _ := store.New(cfg)
	closeCode := Close(s3, cfg, "T-010", "done", CloseOpts{Force: true})
	if closeCode != 0 {
		t.Fatalf("close returned %d", closeCode)
	}

	s4, _ := store.New(cfg)
	item4, _ := s4.Get("T-010")

	totalWall, _ := getNestedField(item4, "time_tracking", "total_wall_time")
	if totalWall == "" {
		t.Error("total_wall_time not set after close")
	}
	// Should contain time units
	if !strings.ContainsAny(totalWall, "dhms") {
		t.Errorf("total_wall_time has no time units: %q", totalWall)
	}

	totalAI, _ := getNestedField(item4, "time_tracking", "total_ai_time")
	if totalAI == "" {
		t.Error("total_ai_time not set after close")
	}

	totalAICost, _ := getNestedField(item4, "time_tracking", "total_ai_cost_usd")
	if totalAICost == "" {
		t.Error("total_ai_cost_usd not set after close")
	}

	t.Logf("=== Metrics Results ===")
	t.Logf("AI cost:       %s", aiCost2)
	t.Logf("AI cost (close): %s", totalAICost)
	t.Logf("Turn count:    %s", turnCount2)
	t.Logf("Sessions:      %d", len(item2.Sessions))
	t.Logf("Total wall:    %s", totalWall)
	t.Logf("Total AI time: %s", totalAI)
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3*time.Minute + 15*time.Second, "3m 15s"},
		{2*time.Hour + 30*time.Minute, "2h 30m"},
		{3*24*time.Hour + 5*time.Hour + 10*time.Minute + 7*time.Second, "3d 5h"},
		{24 * time.Hour, "1d 0h"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPlanStepLaunchesClaude_MissingAC(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item with summary but NO acceptance criteria
	writeFile(t, filepath.Join(root, "tasks", "T-050-no-ac.md"), `id: T-050
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Task without ACs
summary: Has a summary but no acceptance criteria
`)

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	// Mock claude was called, user approves, but claude didn't actually
	// set the fields → plan step should fail with a clear message
	claudeCalled := false
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalled = true
			result := ClaudeResult{Type: "result", Subtype: "success", TotalCostUSD: 0.02, DurationMs: 5000, Result: "Proposed plan"}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser:    func(prompt string) (string, error) { return "y\n", nil },
		ConfirmPrompt: func(prompt string) bool { return true },
	}

	sr := executePlan(s, cfg, "T-050", engine)
	if !claudeCalled {
		t.Error("expected claude to be launched for missing fields")
	}
	// User approved → plan step trusts the approval
	if !sr.Passed {
		t.Errorf("plan step should pass after user approval, got error: %s", sr.Error)
	}
}

func TestPlanStepLaunchesClaude_MissingSummary(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item with ACs but NO summary
	writeFile(t, filepath.Join(root, "tasks", "T-051-no-summary.md"), `id: T-051
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Task without summary
acceptance_criteria:
- Something works
`)

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	claudeCalled := false
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalled = true
			result := ClaudeResult{Type: "result", Subtype: "success", TotalCostUSD: 0.02, DurationMs: 5000, Result: "Proposed summary"}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser:    func(prompt string) (string, error) { return "y\n", nil },
		ConfirmPrompt: func(prompt string) bool { return true },
	}

	sr := executePlan(s, cfg, "T-051", engine)
	if !claudeCalled {
		t.Error("expected claude to be launched for missing summary")
	}
	// User approved → plan step trusts the approval
	if !sr.Passed {
		t.Errorf("plan step should pass after user approval, got error: %s", sr.Error)
	}
}

func TestPlanStepPassesWithRequiredFields(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item with all required fields (summary must be on its own line after header)
	writeFile(t, filepath.Join(root, "tasks", "T-052-complete.md"), `id: T-052
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Complete task
acceptance_criteria:
- Feature works
- Tests pass
summary: |
  Has everything needed for the plan gate.
`)

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	sr := executePlan(s, cfg, "T-052", mockRunEngine(true))
	if !sr.Passed {
		t.Errorf("plan step should pass with all fields, got error: %s", sr.Error)
	}
}

func TestPlanStepSkipsIfAlreadyApproved(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item already approved — even without ACs, should pass (already approved)
	writeFile(t, filepath.Join(root, "tasks", "T-053-approved.md"), `id: T-053
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Already approved
plan_approved: true
`)

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	s, _ := store.New(cfg)

	sr := executePlan(s, cfg, "T-053", mockRunEngine(false)) // wouldn't approve if asked
	if !sr.Passed {
		t.Errorf("plan step should skip for already-approved item, got error: %s", sr.Error)
	}
}

func TestIsEligible(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// T-001 is queued, unclaimed — eligible
	if !isEligible(s, cfg, "T-001") {
		t.Error("T-001 should be eligible")
	}

	// Make T-001 completed — not eligible
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "done")
		it.Status = "done"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}
	s2, _ := store.New(cfg)
	if isEligible(s2, cfg, "T-001") {
		t.Error("completed T-001 should not be eligible")
	}
}

func TestInjectGHPRContext(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		branch string
		repo   string
		want   string
	}{
		{
			name:   "checks with watch",
			cmd:    "gh pr checks --watch",
			branch: "fix/I-042-foo",
			repo:   "TheraPrac/theraprac-web",
			want:   "gh pr checks fix/I-042-foo --repo TheraPrac/theraprac-web --watch",
		},
		{
			name:   "merge with squash and delete",
			cmd:    "gh pr merge --squash --delete-branch",
			branch: "fix/I-042-foo",
			repo:   "TheraPrac/theraprac-web",
			want:   "gh pr merge fix/I-042-foo --repo TheraPrac/theraprac-web --squash --delete-branch",
		},
		{
			name:   "no branch",
			cmd:    "gh pr checks --watch",
			branch: "",
			repo:   "TheraPrac/theraprac-web",
			want:   "gh pr checks --repo TheraPrac/theraprac-web --watch",
		},
		{
			name:   "no args after subcommand",
			cmd:    "gh pr view",
			branch: "fix/I-042-foo",
			repo:   "TheraPrac/theraprac-web",
			want:   "gh pr view fix/I-042-foo --repo TheraPrac/theraprac-web",
		},
		{
			name:   "no gh pr in command",
			cmd:    "echo hello",
			branch: "fix/I-042-foo",
			repo:   "TheraPrac/theraprac-web",
			want:   "echo hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectGHPRContext(tt.cmd, tt.branch, tt.repo)
			if got != tt.want {
				t.Errorf("injectGHPRContext(%q, %q, %q)\n  got:  %q\n  want: %q", tt.cmd, tt.branch, tt.repo, got, tt.want)
			}
		})
	}
}

func TestUATReviewApprove(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "uat_review"}
	step.SetName("uat_review")

	claudeCalls := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalls++
			result := ClaudeResult{Type: "result", Subtype: "success", TotalCostUSD: 0.05, DurationMs: 1000}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "y\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			return "1" // approve
		},
	}

	sr := executeUATReview(s, cfg, "T-001", "test-sprint", step, RunOpts{}, engine, cfg.Root(), "test-session")
	if !sr.Passed {
		t.Errorf("UAT review should pass on 'approve', got error: %s", sr.Error)
	}
	if claudeCalls < 1 {
		t.Error("expected claude to be called for UAT report")
	}
}

func TestUATReviewReject(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "uat_review"}
	step.SetName("uat_review")

	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			result := ClaudeResult{Type: "result", Subtype: "success"}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "n\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			return "2" // reject
		},
	}

	sr := executeUATReview(s, cfg, "T-001", "test-sprint", step, RunOpts{}, engine, cfg.Root(), "test-session")
	if sr.Passed {
		t.Error("UAT review should fail on 'reject'")
	}
	if sr.Error != "user rejected" {
		t.Errorf("expected 'user rejected', got %q", sr.Error)
	}
}

func TestUATReviewFeedbackThenApprove(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "uat_review"}
	step.SetName("uat_review")

	selectCount := 0
	interactiveCalls := 0
	claudeCalls := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalls++
			result := ClaudeResult{Type: "result", Subtype: "success", TotalCostUSD: 0.02}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		RunClaudeInteractive: func(cwd string, args []string) (int, error) {
			interactiveCalls++
			return 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "fix the ACs\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			selectCount++
			if selectCount == 1 {
				return "3" // feedback first (constrained)
			}
			return "1" // then approve
		},
	}

	sr := executeUATReview(s, cfg, "T-001", "test-sprint", step, RunOpts{}, engine, cfg.Root(), "test-session")
	if !sr.Passed {
		t.Errorf("UAT review should pass after feedback + approve, got error: %s", sr.Error)
	}
	if interactiveCalls != 0 {
		t.Errorf("feedback should not launch interactive session, got %d calls", interactiveCalls)
	}
	if selectCount != 2 {
		t.Errorf("expected 2 menu selections (feedback + approve), got %d", selectCount)
	}
	// Expect at least 3 claude calls: UAT report iter 1 + feedback subprocess + UAT report iter 2
	if claudeCalls < 3 {
		t.Errorf("expected at least 3 claude calls (report + feedback + report), got %d", claudeCalls)
	}
}

func TestUATReviewInteractiveEscapeHatch(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "uat_review"}
	step.SetName("uat_review")

	// Track interactive session launches
	interactiveCalls := 0
	var interactiveCwd string

	selectCount := 0
	claudeCalls := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalls++
			result := ClaudeResult{Type: "result", Subtype: "success", TotalCostUSD: 0.01}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		RunClaudeInteractive: func(cwd string, args []string) (int, error) {
			interactiveCalls++
			interactiveCwd = cwd
			return 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "y\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			selectCount++
			if selectCount == 1 {
				return "4" // interactive escape hatch
			}
			return "1" // approve
		},
	}

	sr := executeUATReview(s, cfg, "T-001", "test-sprint", step, RunOpts{}, engine, cfg.Root(), "test-session")

	if interactiveCalls != 1 {
		t.Errorf("expected 1 interactive session, got %d", interactiveCalls)
	}
	if interactiveCwd != cfg.Root() {
		t.Errorf("interactive cwd = %q, want %q", interactiveCwd, cfg.Root())
	}
	if claudeCalls < 2 {
		t.Errorf("expected claude called at least twice (report iter 1 + report iter 2), got %d", claudeCalls)
	}
	if !sr.Passed {
		t.Errorf("UAT review should pass after interactive + approve, got error: %s", sr.Error)
	}
}

func TestUATReviewInteractiveThenReject(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "uat_review"}
	step.SetName("uat_review")

	interactiveCalls := 0
	selectCount := 0
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			result := ClaudeResult{Type: "result", Subtype: "success"}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		RunClaudeInteractive: func(cwd string, args []string) (int, error) {
			interactiveCalls++
			return 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "n\n", nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			selectCount++
			if selectCount == 1 {
				return "4" // interactive escape hatch
			}
			return "2" // reject after interactive
		},
	}

	sr := executeUATReview(s, cfg, "T-001", "test-sprint", step, RunOpts{}, engine, cfg.Root(), "test-session")

	if interactiveCalls != 1 {
		t.Errorf("expected 1 interactive session, got %d", interactiveCalls)
	}
	if sr.Passed {
		t.Error("UAT review should fail on reject after interactive")
	}
	if sr.Error != "user rejected" {
		t.Errorf("expected 'user rejected', got %q", sr.Error)
	}
}

func TestCloseGateRejectsSkippedDeploy(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.SetNested("delivery", "skipped_steps", "deploy_watch")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "close", Resolution: "done"}
	step.SetName("close")

	sr := executeClose(s, cfg, "T-001", step)
	if sr.Passed {
		t.Error("close should reject item with skipped deploy_watch")
	}
	if !strings.Contains(sr.Error, "deploy_watch") {
		t.Errorf("error should mention deploy_watch, got: %s", sr.Error)
	}
}

func TestCloseGateRejectsSkippedUAT(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.SetNested("delivery", "skipped_steps", "uat_review")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "close", Resolution: "done"}
	step.SetName("close")

	sr := executeClose(s, cfg, "T-001", step)
	if sr.Passed {
		t.Error("close should reject item with skipped uat_review")
	}
	if !strings.Contains(sr.Error, "uat_review") {
		t.Errorf("error should mention uat_review, got: %s", sr.Error)
	}
}

func TestCloseGateAllowsNoSkips(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "close", Resolution: "done"}
	step.SetName("close")

	sr := executeClose(s, cfg, "T-001", step)
	// Should pass (or fail for other reasons like gates, but not for skipped steps)
	if sr.Error != "" && strings.Contains(sr.Error, "skipped") {
		t.Errorf("close should not fail for skipped steps when none were skipped, got: %s", sr.Error)
	}
}

func TestCloseGateAllowsNonCriticalSkips(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.SetNested("delivery", "skipped_steps", "code_review")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "close", Resolution: "done"}
	step.SetName("close")

	sr := executeClose(s, cfg, "T-001", step)
	// code_review is not critical — should not block close
	if sr.Error != "" && strings.Contains(sr.Error, "skipped") {
		t.Errorf("close should allow skipped non-critical step code_review, got: %s", sr.Error)
	}
}

func TestCloseGateRejectsMultipleSkips(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		it.SetNested("delivery", "skipped_steps", "code_review,deploy_watch,smoke")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	step := config.RunStepDef{Type: "close", Resolution: "done"}
	step.SetName("close")

	sr := executeClose(s, cfg, "T-001", step)
	if sr.Passed {
		t.Error("close should reject item with skipped deploy_watch in multi-skip list")
	}
}

// --- Post-deploy E2E tests ---

func TestPostDeployE2ENoManifest(t *testing.T) {
	_, cfg := setupRunTestEnv(t)
	result := postDeployE2E(cfg, "T-999")
	if result != "" {
		t.Errorf("expected empty result for missing manifest, got %q", result)
	}
}

func TestPostDeployE2ENoPageFiles(t *testing.T) {
	_, cfg := setupRunTestEnv(t)

	// Create manifest with no page files
	os.MkdirAll(cfg.ManifestDir(), 0755)
	m := &manifest.Manifest{PRs: []manifest.PRRecord{{
		Repo: "api", PRNumber: 42,
		Files: []manifest.FileRecord{
			{Path: "internal/db/billing.go", Action: "M", Type: "app"},
		},
	}}}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(cfg.ManifestDir(), "T-001.json"), data, 0644)

	result := postDeployE2E(cfg, "T-001")
	if result != "" {
		t.Errorf("expected empty result for non-page files, got %q", result)
	}
}

func TestPostDeployE2EFindsPageSpecs(t *testing.T) {
	_, cfg := setupRunTestEnv(t)

	// Add post_deploy to scope suite config
	cfg.Testing = &config.TestingConfig{
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_e2e": {
				Command:       "scripts/e2e-local.sh run",
				PostDeployCmd: "echo DEPLOY_TEST",
			},
		},
	}

	// Create manifest with page files
	os.MkdirAll(cfg.ManifestDir(), 0755)
	m := &manifest.Manifest{PRs: []manifest.PRRecord{{
		Repo: "web", PRNumber: 15,
		Files: []manifest.FileRecord{
			{Path: "src/app/(app)/app/notes/page.tsx", Action: "M", Type: "app"},
			{Path: "src/app/(app)/app/billing/page.tsx", Action: "M", Type: "app"},
			{Path: "src/components/NoteCard.tsx", Action: "M", Type: "app"}, // not a page file
		},
	}}}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(cfg.ManifestDir(), "T-001.json"), data, 0644)

	result := postDeployE2E(cfg, "T-001")
	if result == "" {
		t.Fatal("expected post-deploy E2E result, got empty")
	}
	// Should have run 2 specs (notes.spec.ts and billing.spec.ts)
	if !strings.Contains(result, "2 spec(s)") && !strings.Contains(result, "notes.spec.ts") {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestPostDeployE2EDeletedFilesSkipped(t *testing.T) {
	_, cfg := setupRunTestEnv(t)

	cfg.Testing = &config.TestingConfig{
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_e2e": {PostDeployCmd: "echo DEPLOY_TEST"},
		},
	}

	os.MkdirAll(cfg.ManifestDir(), 0755)
	m := &manifest.Manifest{PRs: []manifest.PRRecord{{
		Repo: "web", PRNumber: 15,
		Files: []manifest.FileRecord{
			{Path: "src/app/(app)/app/notes/page.tsx", Action: "D", Type: "app"}, // deleted
		},
	}}}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(cfg.ManifestDir(), "T-001.json"), data, 0644)

	result := postDeployE2E(cfg, "T-001")
	if result != "" {
		t.Errorf("expected empty result for deleted page file, got %q", result)
	}
}

func TestPostDeployE2ENoPostDeployConfig(t *testing.T) {
	_, cfg := setupRunTestEnv(t)

	cfg.Testing = &config.TestingConfig{
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_e2e": {Command: "scripts/e2e-local.sh run"}, // no PostDeployCmd
		},
	}

	os.MkdirAll(cfg.ManifestDir(), 0755)
	m := &manifest.Manifest{PRs: []manifest.PRRecord{{
		Repo: "web", PRNumber: 15,
		Files: []manifest.FileRecord{
			{Path: "src/app/(app)/app/notes/page.tsx", Action: "M", Type: "app"},
		},
	}}}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(cfg.ManifestDir(), "T-001.json"), data, 0644)

	result := postDeployE2E(cfg, "T-001")
	if result != "" {
		t.Errorf("expected empty when no PostDeployCmd configured, got %q", result)
	}
}

// --- AC path rewriting tests ---

func TestRewriteACPathsRelativeToWorktree(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".as", "worktrees/T-001/theraprac-web", "worktrees/T-001/theraprac-api"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .

worktree:
  enabled: true
  base_dir: worktrees
  repos: [theraprac-api, theraprac-web]
`), 0644)

	cfg, _ := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	uatDir := filepath.Join(root, "worktrees", "T-001")

	tests := []struct {
		input string
		want  string
	}{
		{"cd ../theraprac-web && npx vitest run test.ts", "cd theraprac-web && npx vitest run test.ts"},
		{"cd ../theraprac-api && make test-unit", "cd theraprac-api && make test-unit"},
		{"grep -q 'foo' ../theraprac-web/src/lib/hooks.ts", "grep -q 'foo' theraprac-web/src/lib/hooks.ts"},
		{"echo no repo path", "echo no repo path"},
	}
	for _, tt := range tests {
		got := rewriteACPaths(cfg, "T-001", uatDir, tt.input)
		if got != tt.want {
			t.Errorf("rewriteACPaths(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRewriteACPathsNoWorktree(t *testing.T) {
	cfg := &config.Config{}
	got := rewriteACPaths(cfg, "T-001", "/tmp", "cd ../theraprac-web && test")
	if got != "cd ../theraprac-web && test" {
		t.Errorf("should not rewrite without worktree config, got %q", got)
	}
}

func TestIsReviewBot(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Cursor Bugbot", true},
		{"bugbot", true},
		{"CodeRabbit", true},
		{"SonarCloud", true},
		{"codeclimate", true},
		{"unit-test", false},
		{"changes", false},
		{"integration", false},
	}
	for _, tt := range tests {
		got := isReviewBot(tt.name)
		if got != tt.want {
			t.Errorf("isReviewBot(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStripBugbotMarkup(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "html comment",
			input: "<!-- BUGBOT_REVIEW -->\nHello world",
			want:  "Hello world",
		},
		{
			name:  "sub tags",
			input: "Finding here\n<sub>- powered by bugbot</sub>",
			want:  "Finding here",
		},
		{
			name:  "a tag with link text",
			input: `Click <a href="http://example.com">Fix in Cursor</a> to fix`,
			want:  "Click Fix in Cursor to fix",
		},
		{
			name:  "plain text unchanged",
			input: "### Empty DB roles fail to override stale JWT roles",
			want:  "### Empty DB roles fail to override stale JWT roles",
		},
		{
			name:  "multiple blank lines collapsed",
			input: "line1\n\n\n\nline2",
			want:  "line1 line2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBugbotMarkup(tt.input)
			if got != tt.want {
				t.Errorf("stripBugbotMarkup() = %q, want %q", got, tt.want)
			}
		})
	}
}
