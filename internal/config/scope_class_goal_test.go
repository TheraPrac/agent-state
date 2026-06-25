package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopeClassForItem(t *testing.T) {
	// Disjoint targets mirror the real config: agent-state owns the G-014 goal
	// ID, workspace-config owns the st-tooling tag slug. No shared target means
	// no lexical-shadow ambiguity (I-987 review finding D3).
	tc := &TestingConfig{
		ScopeClasses: map[string]ScopeClassConfig{
			"workspace-config": {
				AppliesToGoals: []string{"st-tooling"},
			},
			"agent-state": {
				AppliesToGoals: []string{"G-014"},
			},
		},
	}

	tests := []struct {
		name  string
		tags  []string
		goals []string
		want  string
	}{
		// goal:<slug> tag matches its class (I-830 behavior preserved).
		{"goal-prefixed tag", []string{"goal:st-tooling"}, nil, "workspace-config"},
		// goal-ID membership matches even with no tags (I-987).
		{"goal id, no tags", nil, []string{"G-014"}, "agent-state"},
		// bare (non-goal:) tags must NOT match — even when equal to a target (D1).
		{"bare tag equal to slug", []string{"st-tooling"}, nil, ""},
		{"bare tag equal to goal id", []string{"G-014"}, nil, ""},
		// no match.
		{"unrelated tag", []string{"some-tag"}, nil, ""},
		{"unrelated goal", []string{"goal:other-goal"}, []string{"G-099"}, ""},
		// goal-ID and tag route to their own disjoint classes.
		{"goal id routes a G-014 item", []string{"goal:st-tooling"}, []string{"G-014"}, "agent-state"},
		{"empty", nil, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tc.ScopeClassForItem(tt.tags, tt.goals)
			if got != tt.want {
				t.Errorf("ScopeClassForItem(%v, %v) = %q, want %q", tt.tags, tt.goals, got, tt.want)
			}
		})
	}
}

// Two classes whose targets both match resolve deterministically by sorted class
// name (agent-state < workspace-config). Documents the tie-break; the real config
// keeps targets disjoint so this never fires in practice.
func TestScopeClassForItem_SortedPrecedence(t *testing.T) {
	tc := &TestingConfig{
		ScopeClasses: map[string]ScopeClassConfig{
			"workspace-config": {AppliesToGoals: []string{"G-014"}},
			"agent-state":      {AppliesToGoals: []string{"G-014"}},
		},
	}
	if got := tc.ScopeClassForItem(nil, []string{"G-014"}); got != "agent-state" {
		t.Errorf("got %q, want agent-state (sorts first)", got)
	}
}

func TestScopeClassForItem_NilReceiver(t *testing.T) {
	var tc *TestingConfig
	if got := tc.ScopeClassForItem([]string{"goal:st-tooling"}, []string{"G-014"}); got != "" {
		t.Errorf("nil receiver: got %q, want empty string", got)
	}
}

func TestConfigParsesAppliesToGoals(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(`
testing:
  scope_classes:
    workspace-config:
      workspace_test: bash run-tests.sh
      applies_to_goals: [st-tooling]
`), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cls, ok := cfg.Testing.ScopeClasses["workspace-config"]
	if !ok {
		t.Fatal("workspace-config scope class not found")
	}
	if len(cls.AppliesToGoals) != 1 || cls.AppliesToGoals[0] != "st-tooling" {
		t.Errorf("AppliesToGoals = %v, want [st-tooling]", cls.AppliesToGoals)
	}
	// Suite still present alongside applies_to_goals.
	if _, ok := cls.RequiredSuites["workspace_test"]; !ok {
		t.Error("workspace_test suite missing after applies_to_goals parsed")
	}
}

// I-987: the agent-state class parses both its as_test and hook_test suites and
// its goal list, and resolves via goal-ID membership.
func TestConfigParsesAgentStateClass(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(`
testing:
  scope_classes:
    agent-state:
      as_test: cd ../as && go build ./... && go vet ./... && go test ./... -count=1
      hook_test: bash claude-config/hooks/run-changed-hook-tests.sh
      applies_to_goals: [G-014]
`), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cls, ok := cfg.Testing.ScopeClasses["agent-state"]
	if !ok {
		t.Fatal("agent-state scope class not found")
	}
	for _, name := range []string{"as_test", "hook_test"} {
		suite, ok := cls.RequiredSuites[name]
		if !ok {
			t.Fatalf("%s suite missing from agent-state class", name)
		}
		if suite.Command == "" {
			t.Errorf("%s command is empty", name)
		}
	}
	if len(cls.AppliesToGoals) != 1 || cls.AppliesToGoals[0] != "G-014" {
		t.Errorf("AppliesToGoals = %v, want [G-014]", cls.AppliesToGoals)
	}
	// Auto-assign resolves via goal-ID membership.
	if got := cfg.Testing.ScopeClassForItem(nil, []string{"G-014"}); got != "agent-state" {
		t.Errorf("ScopeClassForItem(nil, [G-014]) = %q, want agent-state", got)
	}
}
