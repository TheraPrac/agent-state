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
