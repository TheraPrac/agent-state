package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	if cfg.Project.Name != "project" {
		t.Errorf("default project name = %q, want %q", cfg.Project.Name, "project")
	}

	// Task type exists with correct statuses
	taskType, ok := cfg.Types["task"]
	if !ok {
		t.Fatal("default config missing 'task' type")
	}
	wantStatuses := []string{"queued", "active", "completed", "abandoned", "archived"}
	if len(taskType.Statuses) != len(wantStatuses) {
		t.Fatalf("task statuses = %v, want %v", taskType.Statuses, wantStatuses)
	}
	for i, s := range wantStatuses {
		if taskType.Statuses[i] != s {
			t.Errorf("task status[%d] = %q, want %q", i, taskType.Statuses[i], s)
		}
	}

	// Issue type exists
	if _, ok := cfg.Types["issue"]; !ok {
		t.Error("default config missing 'issue' type")
	}

	// Idea type exists
	if _, ok := cfg.Types["idea"]; !ok {
		t.Error("default config missing 'idea' type")
	}

	// Required fields
	if len(cfg.Fields.Required) == 0 {
		t.Error("default config has no required fields")
	}

	// Git defaults
	if cfg.Git == nil {
		t.Fatal("default config has no git config")
	}
	if !cfg.Git.AutoCommit {
		t.Error("default git.auto_commit should be true")
	}
	if !cfg.Git.AutoPush {
		t.Error("default git.auto_push should be true")
	}
}

func TestValidStatuses(t *testing.T) {
	cfg := Defaults()

	tests := []struct {
		itemType string
		want     int
	}{
		{"task", 5},
		{"issue", 5},
		{"idea", 3},
		{"nonexistent", 0},
	}

	for _, tt := range tests {
		got := cfg.ValidStatuses(tt.itemType)
		if len(got) != tt.want {
			t.Errorf("ValidStatuses(%q) returned %d statuses, want %d", tt.itemType, len(got), tt.want)
		}
	}
}

func TestDirectoryForStatus(t *testing.T) {
	cfg := Defaults()

	tests := []struct {
		itemType string
		status   string
		want     string
	}{
		{"task", "queued", "tasks"},
		{"task", "active", "tasks"},
		{"task", "completed", "archive"},
		{"task", "abandoned", "archive"},
		{"issue", "open", "issues"},
		{"issue", "resolved", "archive"},
		{"task", "nonexistent", ""},
		{"nonexistent", "queued", ""},
	}

	for _, tt := range tests {
		got := cfg.DirectoryForStatus(tt.itemType, tt.status)
		if got != tt.want {
			t.Errorf("DirectoryForStatus(%q, %q) = %q, want %q", tt.itemType, tt.status, got, tt.want)
		}
	}
}

func TestDiscoverWalksUp(t *testing.T) {
	// Create temp directory structure: root/.as/config.yaml and root/sub/deep/
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("project:\n  name: test-project\n"), 0644)

	deep := filepath.Join(root, "sub", "deep")
	os.MkdirAll(deep, 0755)

	// Discover from deep should find root/.as/config.yaml
	path, found := discover(deep)
	if !found {
		t.Fatal("discover did not find config.yaml")
	}
	wantPath := filepath.Join(asDir, "config.yaml")
	if path != wantPath {
		t.Errorf("discover found %q, want %q", path, wantPath)
	}
}

func TestDiscoverNotFound(t *testing.T) {
	// TempDir won't have .as/config.yaml anywhere up to root
	dir := t.TempDir()
	_, found := discover(dir)
	if found {
		t.Error("discover should not find config in temp dir")
	}
}

func TestLoadWithConfig(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)

	configContent := `project:
  name: my-project
  description: A test project
paths:
  root: agent-state
  templates: agent-state/templates
git:
  auto_push: false
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Project.Name != "my-project" {
		t.Errorf("project.name = %q, want %q", cfg.Project.Name, "my-project")
	}
	if cfg.Project.Description != "A test project" {
		t.Errorf("project.description = %q, want %q", cfg.Project.Description, "A test project")
	}
	if cfg.Paths.Root != "agent-state" {
		t.Errorf("paths.root = %q, want %q", cfg.Paths.Root, "agent-state")
	}
	if cfg.Git.AutoPush {
		t.Error("git.auto_push should be false after override")
	}
	// Defaults not overridden should remain
	if !cfg.Git.AutoCommit {
		t.Error("git.auto_commit should still be true (not overridden)")
	}
	if cfg.Root() != root {
		t.Errorf("Root() = %q, want %q", cfg.Root(), root)
	}
}

func TestLoadWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Should get defaults rooted at dir
	if cfg.Project.Name != "project" {
		t.Errorf("project.name = %q, want default %q", cfg.Project.Name, "project")
	}
	if cfg.Root() != dir {
		t.Errorf("Root() = %q, want %q", cfg.Root(), dir)
	}
}

func TestAgentID(t *testing.T) {
	cfg := Defaults()

	// Unset
	os.Unsetenv("AS_AGENT_ID")
	if id := cfg.AgentID(); id != "" {
		t.Errorf("AgentID() = %q, want empty", id)
	}

	// Set
	os.Setenv("AS_AGENT_ID", "agent-a")
	defer os.Unsetenv("AS_AGENT_ID")
	if id := cfg.AgentID(); id != "agent-a" {
		t.Errorf("AgentID() = %q, want %q", id, "agent-a")
	}
}

func TestItemDir(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := filepath.Join(root, "agent-state")
	if got := cfg.ItemDir(); got != want {
		t.Errorf("ItemDir() = %q, want %q", got, want)
	}
}
