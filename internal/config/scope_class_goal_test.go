package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopeClassForItem(t *testing.T) {
	tc := &TestingConfig{
		ScopeClasses: map[string]ScopeClassConfig{
			"workspace-config": {
				AppliesToGoals: []string{"st-tooling"},
			},
			// agent-state sorts before workspace-config, so it wins ties.
			"agent-state": {
				AppliesToGoals: []string{"G-014", "st-tooling"},
			},
		},
	}

	tests := []struct {
		name  string
		tags  []string
		goals []string
		want  string
	}{
		// goal:<slug> tag still matches (I-830 behavior preserved).
		{"goal-prefixed tag", []string{"goal:st-tooling"}, nil, "agent-state"},
		// bare tag now matches the slug (I-987).
		{"bare tag", []string{"st-tooling"}, nil, "agent-state"},
		// goal-ID membership matches even with no tags (I-987).
		{"goal id, no tags", nil, []string{"G-014"}, "agent-state"},
		// no match.
		{"unrelated tag", []string{"some-tag"}, nil, ""},
		{"unrelated goal", []string{"goal:other-goal"}, []string{"G-099"}, ""},
		// precedence: agent-state (sorts first) wins over workspace-config.
		{"overlap precedence", []string{"st-tooling"}, []string{"G-014"}, "agent-state"},
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

// workspace-config still matched when it is the only class claiming the slug —
// confirms agent-state's precedence comes from the tie, not from shadowing.
func TestScopeClassForItem_WorkspaceConfigOnly(t *testing.T) {
	tc := &TestingConfig{
		ScopeClasses: map[string]ScopeClassConfig{
			"workspace-config": {AppliesToGoals: []string{"hooks-only"}},
			"agent-state":      {AppliesToGoals: []string{"G-014"}},
		},
	}
	if got := tc.ScopeClassForItem([]string{"goal:hooks-only"}, nil); got != "workspace-config" {
		t.Errorf("got %q, want workspace-config", got)
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

// I-987: the agent-state class parses its as_test suite and goal list.
func TestConfigParsesAgentStateClass(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(`
testing:
  scope_classes:
    agent-state:
      as_test: cd ../as && go build ./... && go vet ./... && go test ./... -count=1
      applies_to_goals: [G-014, st-tooling]
`), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cls, ok := cfg.Testing.ScopeClasses["agent-state"]
	if !ok {
		t.Fatal("agent-state scope class not found")
	}
	suite, ok := cls.RequiredSuites["as_test"]
	if !ok {
		t.Fatal("as_test suite missing from agent-state class")
	}
	if suite.Command == "" {
		t.Error("as_test command is empty")
	}
	if len(cls.AppliesToGoals) != 2 || cls.AppliesToGoals[0] != "G-014" || cls.AppliesToGoals[1] != "st-tooling" {
		t.Errorf("AppliesToGoals = %v, want [G-014 st-tooling]", cls.AppliesToGoals)
	}
	// Auto-assign resolves via goal-ID membership.
	if got := cfg.Testing.ScopeClassForItem(nil, []string{"G-014"}); got != "agent-state" {
		t.Errorf("ScopeClassForItem(nil, [G-014]) = %q, want agent-state", got)
	}
}
