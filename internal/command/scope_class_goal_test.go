package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/parse"
	"github.com/jfinlinson/agent-state/internal/store"
)

// addGoalScopeClass injects the workspace-config scope class with
// applies_to_goals: [st-tooling] into a config for I-830 tests.
func addGoalScopeClass(cfg *config.Config) {
	if cfg.Testing == nil {
		cfg.Testing = &config.TestingConfig{}
	}
	if cfg.Testing.ScopeClasses == nil {
		cfg.Testing.ScopeClasses = make(map[string]config.ScopeClassConfig)
	}
	cfg.Testing.ScopeClasses["workspace-config"] = config.ScopeClassConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"workspace_test": {Command: "bash run-tests.sh"},
		},
		AppliesToGoals: []string{"st-tooling"},
	}
}

func TestCreateAutoSetsScopeClassForGoalTag(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)
	addGoalScopeClass(cfg)

	code := Create(s, cfg, "issue", "auto scope class test", CreateOpts{
		Priority:       2,
		Tag:            "goal:st-tooling",
		Situation:      "test issue situation detail",
		Background:     "test issue background detail",
		Assessment:     "test issue assessment detail",
		Recommendation: "test issue recommendation detail",
	})
	if code != 0 {
		t.Fatalf("Create returned %d, want 0", code)
	}

	// Find the newly created issue — it will be the only I-* beyond I-001.
	items := s.List()
	var newID string
	for _, it := range items {
		if it.Type == "issue" && it.ID != "I-001" {
			newID = it.ID
			break
		}
	}
	if newID == "" {
		t.Fatal("could not find newly created issue")
	}

	// Reload from disk to confirm Doc.SetField persisted scope_class.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New reload: %v", err)
	}
	item, ok := s2.Get(newID)
	if !ok {
		t.Fatalf("s2.Get(%s): not found", newID)
	}
	if item.ScopeClass != "workspace-config" {
		t.Errorf("ScopeClass = %q, want workspace-config", item.ScopeClass)
	}

	// Also verify it round-trips through parse.File.
	path, ok := s2.Path(newID)
	if !ok {
		t.Fatalf("s2.Path(%s): not found", newID)
	}
	parsed, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse.File: %v", err)
	}
	if parsed.ScopeClass != "workspace-config" {
		t.Errorf("parse.File ScopeClass = %q, want workspace-config", parsed.ScopeClass)
	}
}

func TestCreateNoScopeClassWithoutGoalTag(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)
	addGoalScopeClass(cfg)

	code := Create(s, cfg, "issue", "no goal tag issue", CreateOpts{
		Priority:       2,
		Tag:            "some-other-tag",
		Situation:      "test issue situation detail",
		Background:     "test issue background detail",
		Assessment:     "test issue assessment detail",
		Recommendation: "test issue recommendation detail",
	})
	if code != 0 {
		t.Fatalf("Create = %d, want 0", code)
	}

	items := s.List()
	var newID string
	for _, it := range items {
		if it.Type == "issue" && it.ID != "I-001" {
			newID = it.ID
			break
		}
	}
	if newID == "" {
		t.Fatal("no new issue found")
	}
	item, _ := s.Get(newID)
	if item.ScopeClass != "" {
		t.Errorf("ScopeClass = %q, want empty (no goal tag)", item.ScopeClass)
	}
}

func TestStartBackfillsScopeClassFromGoal(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-test")
	t.Setenv("AS_SESSION_ID", "test-session-backfill")
	defer t.Setenv("AS_SESSION_ID", "")

	s, cfg := setupTestEnv(t)
	addGoalScopeClass(cfg)

	// Write a queued task with goal:st-tooling but no scope_class — simulates
	// a task queued before I-830 landed. Tags use multi-line list format (item
	// parser does not handle inline bracket form as an inline list).
	goalTaskPath := filepath.Join(cfg.ItemDir(), "tasks", "T-010-goal-task.md")
	writeFile(t, goalTaskPath, `id: T-010
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Goal-tagged task without scope_class

tags:
- goal:st-tooling

depends_on:
- []

next_actions:
- []

sbar:
  situation: |-
    Fixture for I-830 backfill test.
  background: |-
    Item queued before auto-set was in place.
  assessment: |-
    scope_class should be set at Start time.
  recommendation: |-
    Start command backfills from goal tag.
`)

	// Remove the plan sidecar requirement by creating one.
	if err := os.MkdirAll(cfg.PlansDir(), 0755); err != nil {
		t.Fatalf("MkdirAll plans: %v", err)
	}
	writeFile(t, filepath.Join(cfg.PlansDir(), "T-010.md"), `# Plan: T-010
## Approach
Test plan.
## Scope
Repos: as
## Acceptance Criteria
- cmd: go test ./...
`)

	// Reload store so it picks up T-010.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	_ = s

	if rc := Start(s2, cfg, "T-010", StartOpts{}); rc != 0 {
		t.Fatalf("Start = %d, want 0", rc)
	}

	// Verify via disk reload.
	s3, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New reload: %v", err)
	}
	item, ok := s3.Get("T-010")
	if !ok {
		t.Fatal("T-010 not found after Start")
	}
	if item.ScopeClass != "workspace-config" {
		t.Errorf("ScopeClass = %q, want workspace-config", item.ScopeClass)
	}

	path, ok := s3.Path("T-010")
	if !ok {
		t.Fatal("T-010 path not found")
	}
	parsed, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse.File: %v", err)
	}
	if parsed.ScopeClass != "workspace-config" {
		t.Errorf("parse.File ScopeClass = %q, want workspace-config", parsed.ScopeClass)
	}
}
