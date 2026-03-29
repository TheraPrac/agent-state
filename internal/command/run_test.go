package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
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
				Type:    "result",
				Subtype: "success",
				CostUSD: 0.05,
				Result:  "Implementation complete",
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
	callCount := 0
	engine := RunEngine{
		RunClaude: mockRunEngine(true).RunClaude,
		PromptUser: func(prompt string) (string, error) {
			callCount++
			if callCount == 1 {
				return "2\n", nil // select second sprint
			}
			return "y\n", nil // approve plan
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
	item1, _ := s.Get("T-001")
	item1.Doc.SetField("status", "completed")
	item1.Status = "completed"
	s.Write(item1)

	item2, _ := s.Get("T-002")
	item2.Doc.SetField("status", "completed")
	item2.Status = "completed"
	s.Write(item2)

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
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "json" {
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
		Type:    "result",
		Subtype: "success",
		CostUSD: 0.12,
		Result:  "Done",
	}
	data, _ := json.Marshal(result)

	parsed, err := parseClaudeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subtype != "success" {
		t.Errorf("subtype = %q, want success", parsed.Subtype)
	}
	if parsed.CostUSD != 0.12 {
		t.Errorf("cost = %f, want 0.12", parsed.CostUSD)
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

func TestIsEligible(t *testing.T) {
	s, cfg := setupRunTestEnv(t)

	// T-001 is queued, unclaimed — eligible
	if !isEligible(s, cfg, "T-001") {
		t.Error("T-001 should be eligible")
	}

	// Make T-001 completed — not eligible
	item, _ := s.Get("T-001")
	item.Doc.SetField("status", "completed")
	item.Status = "completed"
	s.Write(item)
	s2, _ := store.New(cfg)
	if isEligible(s2, cfg, "T-001") {
		t.Error("completed T-001 should not be eligible")
	}
}
