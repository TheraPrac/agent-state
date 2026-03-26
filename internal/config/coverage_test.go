package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsHaveIDPrefixes(t *testing.T) {
	cfg := Defaults()

	tests := []struct {
		typeName string
		prefix   string
	}{
		{"task", "T"},
		{"issue", "I"},
		{"idea", "D"},
	}

	for _, tt := range tests {
		tc, ok := cfg.Types[tt.typeName]
		if !ok {
			t.Errorf("missing type %q", tt.typeName)
			continue
		}
		if tc.IDPrefix != tt.prefix {
			t.Errorf("%s.IDPrefix = %q, want %q", tt.typeName, tc.IDPrefix, tt.prefix)
		}
	}
}

func TestDefaultsTerminalStatuses(t *testing.T) {
	cfg := Defaults()

	tc := cfg.Types["task"]
	found := false
	for _, s := range tc.TerminalStatuses {
		if s == "completed" {
			found = true
		}
	}
	if !found {
		t.Error("task terminal statuses should include 'completed'")
	}
}

func TestPathAccessors(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: data\n  templates: data/tmpl\n  changelog: data/.log\n  index: data/index.md\n"), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.TemplatesDir() != filepath.Join(root, "data/tmpl") {
		t.Errorf("TemplatesDir = %q", cfg.TemplatesDir())
	}
	if cfg.ChangelogDir() != filepath.Join(root, "data/.log") {
		t.Errorf("ChangelogDir = %q", cfg.ChangelogDir())
	}
	if cfg.IndexPath() != filepath.Join(root, "data/index.md") {
		t.Errorf("IndexPath = %q", cfg.IndexPath())
	}
}

func TestLoadFromExplicitPath(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("project:\n  name: explicit\n"), 0644)

	cfg, err := LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Project.Name != "explicit" {
		t.Errorf("name = %q, want explicit", cfg.Project.Name)
	}
}

func TestLoadFromBadPath(t *testing.T) {
	_, err := LoadFrom("/nonexistent/.as/config.yaml")
	if err == nil {
		t.Error("LoadFrom nonexistent should fail")
	}
}

func TestConfigGitDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Git.LockFile != ".as.lock" {
		t.Errorf("LockFile = %q, want .as.lock", cfg.Git.LockFile)
	}
}

func TestConfigOverrideGitSection(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("git:\n  auto_commit: false\n  auto_push: false\n  lock_file: custom.lock\n"), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Git.AutoCommit {
		t.Error("auto_commit should be false")
	}
	if cfg.Git.AutoPush {
		t.Error("auto_push should be false")
	}
	if cfg.Git.LockFile != "custom.lock" {
		t.Errorf("lock_file = %q, want custom.lock", cfg.Git.LockFile)
	}
}

func TestConfigTestingSection(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := `testing:
  enabled: true

  required_suites:
    api_unit: cd api && make test-unit
    api_lint: cd api && make lint
    web_typecheck: cd web && make type-check
    web_unit: cd web && make test-unit

  scope_suites:
    api_integration: cd api && make integration
    web_e2e: cd web && make e2e
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Testing == nil {
		t.Fatal("testing config should not be nil")
	}

	// Required suites
	if len(cfg.Testing.RequiredSuites) != 4 {
		t.Errorf("required_suites: got %d, want 4", len(cfg.Testing.RequiredSuites))
	}
	if s, ok := cfg.Testing.RequiredSuites["api_unit"]; !ok {
		t.Error("missing api_unit suite")
	} else if s.Command != "cd api && make test-unit" {
		t.Errorf("api_unit command = %q", s.Command)
	}

	// Scope suites
	if len(cfg.Testing.ScopeSuites) != 2 {
		t.Errorf("scope_suites: got %d, want 2", len(cfg.Testing.ScopeSuites))
	}
	if s, ok := cfg.Testing.ScopeSuites["web_e2e"]; !ok {
		t.Error("missing web_e2e suite")
	} else if s.Command != "cd web && make e2e" {
		t.Errorf("web_e2e command = %q", s.Command)
	}
}

func TestConfigDeliverySection(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := `delivery:
  enabled: true
  stages: [coding, committed, pushed, pr_open, merged, deployed_dev, closed]
  archive_gate: merged
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Delivery == nil {
		t.Fatal("delivery config should not be nil")
	}
	if len(cfg.Delivery.Stages) != 7 {
		t.Errorf("stages: got %d, want 7", len(cfg.Delivery.Stages))
	}
	if cfg.Delivery.Stages[0] != "coding" {
		t.Errorf("first stage = %q, want coding", cfg.Delivery.Stages[0])
	}
	if cfg.Delivery.ArchiveGate != "merged" {
		t.Errorf("archive_gate = %q, want merged", cfg.Delivery.ArchiveGate)
	}
}

func TestConfigSuiteNames(t *testing.T) {
	tc := &TestingConfig{
		RequiredSuites: map[string]SuiteConfig{
			"web_unit": {}, "api_unit": {}, "api_lint": {},
		},
		ScopeSuites: map[string]ScopeSuiteConfig{
			"web_e2e": {}, "api_integration": {},
		},
	}

	required := tc.RequiredSuiteNames()
	if len(required) != 3 {
		t.Fatalf("required suite names: got %d, want 3", len(required))
	}
	// Should be sorted
	if required[0] != "api_lint" || required[1] != "api_unit" || required[2] != "web_unit" {
		t.Errorf("required suite order: %v", required)
	}

	scope := tc.ScopeSuiteNames()
	if len(scope) != 2 {
		t.Fatalf("scope suite names: got %d, want 2", len(scope))
	}
	if scope[0] != "api_integration" || scope[1] != "web_e2e" {
		t.Errorf("scope suite order: %v", scope)
	}
}

func TestParseInlineList(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"[a, b, c]", 3},
		{"[single]", 1},
		{"[]", 0},
		{`["quoted", 'single']`, 2},
	}

	for _, tt := range tests {
		got := parseInlineList(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseInlineList(%q): got %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestConfigWorktreeSection(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := `worktree:
  enabled: true
  base_dir: worktrees
  parent_dir: /home/dev/project
  repos: [api, web, infra]
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Worktree == nil {
		t.Fatal("worktree config should not be nil")
	}
	if !cfg.Worktree.Enabled {
		t.Error("worktree.enabled should be true")
	}
	if cfg.Worktree.BaseDir != "worktrees" {
		t.Errorf("base_dir = %q", cfg.Worktree.BaseDir)
	}
	if cfg.Worktree.ParentDir != "/home/dev/project" {
		t.Errorf("parent_dir = %q", cfg.Worktree.ParentDir)
	}
	if len(cfg.Worktree.Repos) != 3 {
		t.Errorf("repos: got %d, want 3", len(cfg.Worktree.Repos))
	}
}

func TestConfigCombined(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := `project:
  name: theraprac
  description: Healthcare platform

paths:
  root: agent-state

testing:
  enabled: true

  required_suites:
    api_unit: make test-unit

  scope_suites:
    api_integration: make integration

delivery:
  stages: [coding, merged, closed]
  archive_gate: merged

git:
  auto_push: false
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Project.Name != "theraprac" {
		t.Errorf("name = %q", cfg.Project.Name)
	}
	if cfg.Paths.Root != "agent-state" {
		t.Errorf("root = %q", cfg.Paths.Root)
	}
	if cfg.Testing == nil || len(cfg.Testing.RequiredSuites) != 1 {
		t.Error("testing not loaded correctly")
	}
	if cfg.Delivery == nil || len(cfg.Delivery.Stages) != 3 {
		t.Error("delivery not loaded correctly")
	}
	if cfg.Git.AutoPush {
		t.Error("auto_push should be overridden to false")
	}
}
