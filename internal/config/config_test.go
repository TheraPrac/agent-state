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
	t.Setenv("ST_ROOT", "")
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

func TestDiscoverViaStRoot(t *testing.T) {
	// Create a parent dir with .st-root pointing to a subdirectory
	parent := t.TempDir()
	sub := filepath.Join(parent, "workspace")
	os.MkdirAll(filepath.Join(sub, ".as"), 0755)
	os.WriteFile(filepath.Join(sub, ".as", "config.yaml"), []byte("project:\n  name: redirected\n"), 0644)

	// .st-root with relative path
	os.WriteFile(filepath.Join(parent, ".st-root"), []byte("workspace\n"), 0644)

	t.Setenv("ST_ROOT", "")
	cfg, err := Load(parent)
	if err != nil {
		t.Fatalf("Load via .st-root: %v", err)
	}
	if !cfg.Discovered {
		t.Fatal("expected Discovered=true via .st-root redirect")
	}
	if cfg.Project.Name != "redirected" {
		t.Errorf("project.name = %q, want %q", cfg.Project.Name, "redirected")
	}
	if cfg.Root() != sub {
		t.Errorf("Root() = %q, want %q", cfg.Root(), sub)
	}
}

func TestDiscoverViaStRootAbsPath(t *testing.T) {
	// .st-root with absolute path
	parent := t.TempDir()
	target := t.TempDir()
	os.MkdirAll(filepath.Join(target, ".as"), 0755)
	os.WriteFile(filepath.Join(target, ".as", "config.yaml"), []byte("project:\n  name: abs-redirect\n"), 0644)

	os.WriteFile(filepath.Join(parent, ".st-root"), []byte(target+"\n"), 0644)

	t.Setenv("ST_ROOT", "")
	cfg, err := Load(parent)
	if err != nil {
		t.Fatalf("Load via .st-root (abs): %v", err)
	}
	if cfg.Project.Name != "abs-redirect" {
		t.Errorf("project.name = %q, want %q", cfg.Project.Name, "abs-redirect")
	}
}

func TestDiscoverViaStRootFromChild(t *testing.T) {
	// .st-root at parent, discovery starts from a child directory
	parent := t.TempDir()
	child := filepath.Join(parent, "some", "nested", "dir")
	os.MkdirAll(child, 0755)

	sub := filepath.Join(parent, "workspace")
	os.MkdirAll(filepath.Join(sub, ".as"), 0755)
	os.WriteFile(filepath.Join(sub, ".as", "config.yaml"), []byte("project:\n  name: from-child\n"), 0644)
	os.WriteFile(filepath.Join(parent, ".st-root"), []byte("workspace\n"), 0644)

	t.Setenv("ST_ROOT", "")
	cfg, err := Load(child)
	if err != nil {
		t.Fatalf("Load from child via .st-root: %v", err)
	}
	if !cfg.Discovered {
		t.Fatal("expected Discovered=true")
	}
	if cfg.Project.Name != "from-child" {
		t.Errorf("project.name = %q, want %q", cfg.Project.Name, "from-child")
	}
}

func TestAgentID(t *testing.T) {
	clearHeritage(t)
	t.Run("no_match", func(t *testing.T) {
		t.Setenv("AS_AGENT_ID", "")
		cfg := Defaults()
		cfg.root = filepath.Join(t.TempDir(), "theraprac-workspace")
		if id := cfg.AgentID(); id != "" {
			t.Errorf("AgentID() = %q, want empty", id)
		}
	})

	t.Run("env_override", func(t *testing.T) {
		t.Setenv("AS_AGENT_ID", "agent-override")
		cfg := Defaults()
		cfg.root = filepath.Join(t.TempDir(), "theraprac-agent-a", "theraprac-workspace")
		if id := cfg.AgentID(); id != "agent-override" {
			t.Errorf("AgentID() = %q, want %q", id, "agent-override")
		}
	})

	t.Run("path_derivation", func(t *testing.T) {
		t.Setenv("AS_AGENT_ID", "")
		for _, agent := range []string{"agent-a", "agent-b"} {
			cfg := Defaults()
			cfg.root = filepath.Join(t.TempDir(), "theraprac-"+agent, "theraprac-workspace")
			if id := cfg.AgentID(); id != agent {
				t.Errorf("AgentID() = %q, want %q", id, agent)
			}
		}
	})
}

// clearHeritage zeros out all heritage env vars for the duration of a test
// to keep parent tests from leaking into child subtests.
func clearHeritage(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AS_AGENT_ID",
		"AS_AGENT_PARENT_ID",
		"AS_AGENT_ROOT_ID",
		"AS_AGENT_SPAWNED_BY_SESSION",
		"AS_AGENT_DELEGATED_ITEM",
		"AS_AGENT_ROLE",
	} {
		t.Setenv(k, "")
	}
}

func writeLocalAgent(t *testing.T, root string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "local-agent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIdentity(t *testing.T) {
	t.Run("local_config_no_env_no_path", func(t *testing.T) {
		clearHeritage(t)
		root := filepath.Join(t.TempDir(), "theraprac-workspace")
		writeLocalAgent(t, root, "id: agent-from-config\ndisplay_name: Local Bob\nrole: worker\n")
		cfg := Defaults()
		cfg.root = root
		got := cfg.Identity()
		if got.ID != "agent-from-config" {
			t.Errorf("ID = %q, want agent-from-config", got.ID)
		}
		if got.Source != "local-config" {
			t.Errorf("Source = %q, want local-config", got.Source)
		}
		if got.DisplayName != "Local Bob" {
			t.Errorf("DisplayName = %q", got.DisplayName)
		}
		if got.Role != "worker" {
			t.Errorf("Role = %q", got.Role)
		}
		if got.RootID != got.ID {
			t.Errorf("RootID = %q, want it to default to ID", got.RootID)
		}
	})

	t.Run("env_overrides_local_config", func(t *testing.T) {
		clearHeritage(t)
		t.Setenv("AS_AGENT_ID", "agent-from-env")
		root := filepath.Join(t.TempDir(), "theraprac-workspace")
		writeLocalAgent(t, root, "id: agent-from-config\n")
		cfg := Defaults()
		cfg.root = root
		got := cfg.Identity()
		if got.ID != "agent-from-env" {
			t.Errorf("ID = %q, want agent-from-env (env wins)", got.ID)
		}
		if got.Source != "env" {
			t.Errorf("Source = %q, want env", got.Source)
		}
	})

	t.Run("local_config_overrides_path", func(t *testing.T) {
		clearHeritage(t)
		root := filepath.Join(t.TempDir(), "theraprac-agent-a", "theraprac-workspace")
		writeLocalAgent(t, root, "id: agent-explicit\n")
		cfg := Defaults()
		cfg.root = root
		got := cfg.Identity()
		if got.ID != "agent-explicit" {
			t.Errorf("ID = %q, want agent-explicit (config beats path-derived agent-a)", got.ID)
		}
		if got.Source != "local-config" {
			t.Errorf("Source = %q, want local-config", got.Source)
		}
	})

	t.Run("inherited_heritage_marks_source", func(t *testing.T) {
		clearHeritage(t)
		t.Setenv("AS_AGENT_ID", "agent-child")
		t.Setenv("AS_AGENT_PARENT_ID", "agent-a")
		t.Setenv("AS_AGENT_ROLE", "reviewer")
		t.Setenv("AS_AGENT_SPAWNED_BY_SESSION", "sess-parent-1")
		t.Setenv("AS_AGENT_DELEGATED_ITEM", "I-100")
		cfg := Defaults()
		cfg.root = filepath.Join(t.TempDir(), "theraprac-workspace")
		got := cfg.Identity()
		if got.ID != "agent-child" {
			t.Errorf("ID = %q", got.ID)
		}
		if got.Source != "inherited" {
			t.Errorf("Source = %q, want inherited", got.Source)
		}
		if got.ParentID != "agent-a" || got.Role != "reviewer" {
			t.Errorf("heritage missing: %+v", got)
		}
		if got.SpawnedBySession != "sess-parent-1" || got.DelegatedItemID != "I-100" {
			t.Errorf("session/item heritage missing: %+v", got)
		}
		if got.RootID != "agent-a" {
			t.Errorf("RootID = %q, want it to default to ParentID when unset", got.RootID)
		}
	})

	t.Run("root_defaults_to_id_with_no_parent", func(t *testing.T) {
		clearHeritage(t)
		t.Setenv("AS_AGENT_ID", "agent-solo")
		cfg := Defaults()
		cfg.root = filepath.Join(t.TempDir(), "theraprac-workspace")
		got := cfg.Identity()
		if got.RootID != "agent-solo" {
			t.Errorf("RootID = %q, want it to default to ID when no parent", got.RootID)
		}
		if got.HasHeritage() {
			t.Errorf("HasHeritage() = true, want false")
		}
	})

	t.Run("explicit_root_id_is_preserved", func(t *testing.T) {
		clearHeritage(t)
		t.Setenv("AS_AGENT_ID", "agent-grandchild")
		t.Setenv("AS_AGENT_PARENT_ID", "agent-child")
		t.Setenv("AS_AGENT_ROOT_ID", "agent-root")
		cfg := Defaults()
		cfg.root = filepath.Join(t.TempDir(), "theraprac-workspace")
		got := cfg.Identity()
		if got.RootID != "agent-root" {
			t.Errorf("RootID = %q, want agent-root", got.RootID)
		}
	})
}

func TestEpicsPath(t *testing.T) {
	t.Setenv("ST_ROOT", "")
	root := t.TempDir()
	cfg, _ := Load(root)
	got := cfg.EpicsPath()
	want := filepath.Join(root, ".as", "epics.yaml")
	if got != want {
		t.Errorf("EpicsPath() = %q, want %q", got, want)
	}
}

func TestNotesPath(t *testing.T) {
	t.Setenv("ST_ROOT", "")
	root := t.TempDir()
	cfg, _ := Load(root)
	got := cfg.NotesPath()
	want := filepath.Join(root, ".as", "notes.yaml")
	if got != want {
		t.Errorf("NotesPath() = %q, want %q", got, want)
	}
}

func TestSessionID(t *testing.T) {
	cfg := Defaults()
	os.Unsetenv("AS_SESSION_ID")
	if id := cfg.SessionID(); id != "" {
		t.Errorf("SessionID() = %q, want empty", id)
	}
	os.Setenv("AS_SESSION_ID", "test-session-123")
	defer os.Unsetenv("AS_SESSION_ID")
	if id := cfg.SessionID(); id != "test-session-123" {
		t.Errorf("SessionID() = %q, want %q", id, "test-session-123")
	}
}

func TestSplitKVNoColon(t *testing.T) {
	key, val := splitKV("no-colon-here")
	if key != "no-colon-here" || val != "" {
		t.Errorf("splitKV(no colon) = %q, %q", key, val)
	}
}

func TestSplitKVWithComment(t *testing.T) {
	key, val := splitKV("name: value # comment")
	if key != "name" || val != "value" {
		t.Errorf("splitKV(with comment) = %q, %q", key, val)
	}
}

func TestSplitKVWithQuotes(t *testing.T) {
	key, val := splitKV(`title: "quoted value"`)
	if key != "title" || val != "quoted value" {
		t.Errorf("splitKV(quoted) = %q, %q", key, val)
	}
}

func TestLoadConfigWithListItems(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)

	// Config with list items and inline lists to exercise applyListItem + applyInlineList
	configContent := `project:
  name: list-test
fields:
  required: [id, type, status, title]
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project.Name != "list-test" {
		t.Errorf("project.name = %q", cfg.Project.Name)
	}
	if len(cfg.Fields.Required) < 4 {
		t.Errorf("fields.required = %v, want at least 4", cfg.Fields.Required)
	}
}

func TestLoadConfigWithDashListItems(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)

	// Config with dash-prefixed list items
	configContent := `project:
  name: dash-test
fields:
  required:
    - id
    - type
    - status
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project.Name != "dash-test" {
		t.Errorf("project.name = %q", cfg.Project.Name)
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

func TestScopeSuitePostDeployAndTriggers(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)

	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(`paths:
  root: .

testing:
  enabled: true
  scope_suites:
    web_e2e:
      command: scripts/e2e-local.sh run
      post_deploy: scripts/e2e-local.sh run --target dev
      triggers: [src/app/**, src/components/**]
      artifacts: [test-results/**]
`), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	sc, ok := cfg.Testing.ScopeSuites["web_e2e"]
	if !ok {
		t.Fatal("web_e2e scope suite not found")
	}
	if sc.Command != "scripts/e2e-local.sh run" {
		t.Errorf("Command = %q", sc.Command)
	}
	if sc.PostDeployCmd != "scripts/e2e-local.sh run --target dev" {
		t.Errorf("PostDeployCmd = %q", sc.PostDeployCmd)
	}
	if len(sc.Triggers) != 2 {
		t.Errorf("Triggers = %v, want 2 items", sc.Triggers)
	} else {
		if sc.Triggers[0] != "src/app/**" || sc.Triggers[1] != "src/components/**" {
			t.Errorf("Triggers = %v", sc.Triggers)
		}
	}
	if len(sc.Artifacts) != 1 || sc.Artifacts[0] != "test-results/**" {
		t.Errorf("Artifacts = %v", sc.Artifacts)
	}
}

// I-407: WorktreeBase places <id> dirs under the agent root (one level
// up from the workspace), not inside the workspace itself. Workspace is
// symlinked across agents (I-418); placing worktrees in the workspace
// would mean every agent shares one physical worktree dir.
func TestWorktreeBasePlacesUnderAgentRoot(t *testing.T) {
	cfg := &Config{
		root: "/Users/jfinlinson/Dev/theraprac-agents/theraprac-agent-b/theraprac-workspace",
		Worktree: &WorktreeConfig{
			Enabled: true,
			BaseDir: "worktrees",
		},
	}
	got := cfg.WorktreeBase()
	want := "/Users/jfinlinson/Dev/theraprac-agents/theraprac-agent-b/worktrees"
	if got != want {
		t.Errorf("WorktreeBase() = %q, want %q (must be agent-root + base_dir, not workspace + base_dir)", got, want)
	}
}

func TestWorktreeBaseDisabled(t *testing.T) {
	cfg := &Config{root: "/some/path"}
	if got := cfg.WorktreeBase(); got != "" {
		t.Errorf("WorktreeBase() with nil Worktree config = %q, want empty", got)
	}
	cfg.Worktree = &WorktreeConfig{Enabled: false, BaseDir: "worktrees"}
	if got := cfg.WorktreeBase(); got != "" {
		t.Errorf("WorktreeBase() with Enabled=false = %q, want empty", got)
	}
}

// I-407 migration: WorktreeBaseLegacy returns the pre-fix shared
// workspace location so finish/close can clean up old worktrees that
// predate the fix.
func TestWorktreeBaseLegacyReturnsWorkspaceLocation(t *testing.T) {
	cfg := &Config{
		root: "/Users/jfinlinson/Dev/theraprac-agents/theraprac-agent-b/theraprac-workspace",
		Worktree: &WorktreeConfig{
			Enabled: true,
			BaseDir: "worktrees",
		},
	}
	got := cfg.WorktreeBaseLegacy()
	want := "/Users/jfinlinson/Dev/theraprac-agents/theraprac-agent-b/theraprac-workspace/worktrees"
	if got != want {
		t.Errorf("WorktreeBaseLegacy() = %q, want %q (pre-I-407 location: workspace + base_dir)", got, want)
	}
}
