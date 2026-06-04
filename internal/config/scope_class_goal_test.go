package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopeClassForGoalTags(t *testing.T) {
	tc := &TestingConfig{
		ScopeClasses: map[string]ScopeClassConfig{
			"workspace-config": {
				AppliesToGoals: []string{"st-tooling"},
			},
		},
	}

	tests := []struct {
		name string
		tags []string
		want string
	}{
		{"matches goal tag", []string{"goal:st-tooling"}, "workspace-config"},
		{"no goal tag", []string{"some-tag"}, ""},
		{"unrelated goal tag", []string{"goal:other-goal"}, ""},
		{"multiple tags, one matches", []string{"p1", "goal:st-tooling", "sprint:2"}, "workspace-config"},
		{"empty tags", []string{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tc.ScopeClassForGoalTags(tt.tags)
			if got != tt.want {
				t.Errorf("ScopeClassForGoalTags(%v) = %q, want %q", tt.tags, got, tt.want)
			}
		})
	}
}

func TestScopeClassForGoalTags_NilReceiver(t *testing.T) {
	var tc *TestingConfig
	if got := tc.ScopeClassForGoalTags([]string{"goal:st-tooling"}); got != "" {
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
