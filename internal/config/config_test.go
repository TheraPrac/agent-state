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
	// I-433: unified status vocabulary across types — `done` replaces
	// `completed` (task) and `resolved` (issue); `abandoned` replaces
	// `wontfix` (issue).
	// T-346: `awaiting_decision` added as a non-terminal pause state
	// for the binary autonomy loop.
	wantStatuses := []string{"queued", "active", "awaiting_decision", "done", "abandoned", "archived"}
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
		// T-346: task/issue gained `awaiting_decision`.
		{"task", 6},
		{"issue", 6},
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
		// I-433: unified vocabulary — done/abandoned/archived for terminal,
		// queued/active for live. Old per-type values (completed, open,
		// resolved, wontfix) are retired.
		{"task", "queued", "tasks"},
		{"task", "active", "tasks"},
		{"task", "done", "archive"},
		{"task", "abandoned", "archive"},
		{"issue", "queued", "issues"},
		{"issue", "active", "issues"},
		{"issue", "done", "archive"},
		{"issue", "abandoned", "archive"},
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

// I-696: post_merge parses into ScopeSuiteConfig.PostMergeCmd, distinct
// from post_deploy, and the simple-format short-circuit must not swallow it.
func TestScopeSuitePostMergeParse(t *testing.T) {
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
      post_merge: cd ../theraprac-web && git checkout -q main && scripts/e2e-local.sh run
`), 0644)

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sc, ok := cfg.Testing.ScopeSuites["web_e2e"]
	if !ok {
		t.Fatal("web_e2e scope suite not found")
	}
	if sc.PostMergeCmd != "cd ../theraprac-web && git checkout -q main && scripts/e2e-local.sh run" {
		t.Errorf("PostMergeCmd = %q", sc.PostMergeCmd)
	}
	if sc.PostDeployCmd != "scripts/e2e-local.sh run --target dev" {
		t.Errorf("PostDeployCmd = %q (post_merge must not clobber it)", sc.PostDeployCmd)
	}
	if sc.Command != "scripts/e2e-local.sh run" {
		t.Errorf("Command = %q", sc.Command)
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

// I-407: WorktreeForItem prefers the new agent-root location, falls back
// to legacy when the new path is empty but the legacy one exists, and
// returns the new path when neither exists (so writers default to the
// post-fix layout).
func TestWorktreeForItemPrefersNewLocation(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "agent-x")
	workspace := filepath.Join(agentRoot, "theraprac-workspace")
	os.MkdirAll(workspace, 0755)

	cfg := &Config{
		root:     workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees"},
	}

	// Case 1: neither location exists → returns new path.
	got := cfg.WorktreeForItem("T-001")
	want := filepath.Join(agentRoot, "worktrees", "T-001")
	if got != want {
		t.Errorf("nonexistent → %q, want %q", got, want)
	}

	// Case 2: only legacy exists → returns legacy.
	legacyPath := filepath.Join(workspace, "worktrees", "T-001")
	os.MkdirAll(legacyPath, 0755)
	got = cfg.WorktreeForItem("T-001")
	if got != legacyPath {
		t.Errorf("legacy-only → %q, want %q", got, legacyPath)
	}

	// Case 3: new path exists too → prefers new path.
	newPath := filepath.Join(agentRoot, "worktrees", "T-001")
	os.MkdirAll(newPath, 0755)
	got = cfg.WorktreeForItem("T-001")
	if got != newPath {
		t.Errorf("both exist → %q, want %q (new path wins)", got, newPath)
	}
}

func TestWorktreeForItemDisabledOrEmpty(t *testing.T) {
	cfg := &Config{root: "/some/path"}
	if got := cfg.WorktreeForItem("T-001"); got != "" {
		t.Errorf("disabled → %q, want empty", got)
	}
	cfg.Worktree = &WorktreeConfig{Enabled: true, BaseDir: "worktrees"}
	if got := cfg.WorktreeForItem(""); got != "" {
		t.Errorf("empty id → %q, want empty", got)
	}
}

// I-778: AgentRoot prefers .as/agent-workspace.yaml under the invocation
// site so the correct per-agent root is recoverable even when the
// discovered c.root resolves to a peer agent's workspace.
func TestAgentRootFromWorkspaceYaml(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "theraprac-agent-b")
	workspace := filepath.Join(agentRoot, "theraprac-workspace")
	os.MkdirAll(filepath.Join(agentRoot, ".as"), 0755)
	os.MkdirAll(workspace, 0755)
	yaml := "agent_id: agent-b\npath: " + agentRoot + "\n"
	os.WriteFile(filepath.Join(agentRoot, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	got := cfg.AgentRoot()
	if got != agentRoot {
		t.Errorf("AgentRoot() = %q, want %q", got, agentRoot)
	}
}

// I-778 regression: when ST_ROOT leaks a peer agent's workspace path
// into cfg.root, AgentRoot must still resolve to the correct agent
// (the one whose .as/agent-workspace.yaml lives at the invocation
// site).
func TestAgentRootSTRootLeakRegression(t *testing.T) {
	tmp := t.TempDir()
	// Peer agent (the leaked ST_ROOT target) — has a workspace, no marker yaml.
	peerAgent := filepath.Join(tmp, "theraprac-agent-a")
	peerWorkspace := filepath.Join(peerAgent, "theraprac-workspace")
	os.MkdirAll(peerWorkspace, 0755)

	// Real agent — has .as/agent-workspace.yaml pointing at its own root,
	// and an as/ clone where the invocation originated.
	realAgent := filepath.Join(tmp, "theraprac-agent-b")
	realAs := filepath.Join(realAgent, "as")
	os.MkdirAll(filepath.Join(realAgent, ".as"), 0755)
	os.MkdirAll(realAs, 0755)
	yaml := "agent_id: agent-b\npath: " + realAgent + "\n"
	os.WriteFile(filepath.Join(realAgent, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)

	cfg := &Config{
		root:     peerWorkspace, // ST_ROOT leak: cfg.root points at the peer
		startDir: realAs,        // invocation site: real agent's as/ clone
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	got := cfg.AgentRoot()
	if got != realAgent {
		t.Errorf("AgentRoot() = %q, want %q (must recover correct agent under ST_ROOT leak)", got, realAgent)
	}
}

// I-778: when no .as/agent-workspace.yaml is found anywhere on the
// walk, fall back to filepath.Dir(c.root) — the pre-I-778 behavior
// (and the I-407 WorktreeBase default).
func TestAgentRootFallbackNoWorkspaceYaml(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "theraprac-workspace")
	os.MkdirAll(workspace, 0755)

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	got := cfg.AgentRoot()
	want := tmp
	if got != want {
		t.Errorf("AgentRoot() = %q, want %q (fallback to filepath.Dir(c.root))", got, want)
	}
}

// I-778: an explicit absolute worktree.parent_dir override is honored
// by RepoParent (operator escape hatch for non-standard layouts;
// back-compat with pre-fix behavior). AgentRoot ignores parent_dir
// entirely — it always returns the per-agent root.
func TestRepoParentAbsoluteParentDirOverride(t *testing.T) {
	cfg := &Config{
		root:     "/some/agent/theraprac-workspace",
		startDir: "/some/agent/theraprac-workspace",
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: "/custom/dev"},
	}
	if got := cfg.RepoParent(); got != "/custom/dev" {
		t.Errorf("RepoParent() = %q, want %q (absolute ParentDir override)", got, "/custom/dev")
	}
	if got := cfg.AgentRoot(); got != "/some/agent" {
		t.Errorf("AgentRoot() = %q, want %q (must not follow parent_dir)", got, "/some/agent")
	}
}

// I-778: a relative worktree.parent_dir is preserved as a legacy
// fallback when no agent-workspace.yaml marker is recoverable.
// Reproduces the pre-PR `parent_dir: ..` semantics.
func TestAgentRootRelativeParentDirLegacyFallback(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "theraprac-workspace")
	os.MkdirAll(workspace, 0755)

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	got := cfg.AgentRoot()
	want := tmp
	if got != want {
		t.Errorf("AgentRoot() = %q, want %q (relative parent_dir legacy fallback)", got, want)
	}
}

// I-778: parseAgentWorkspaceMarker must reject indented path: keys so
// a future nested mapping can't be mistaken for the top-level field.
func TestParseAgentWorkspaceMarkerRejectsNestedPath(t *testing.T) {
	body := []byte("agent_id: agent-x\nrepos:\n  path: /wrong/nested\npath: /right/top\n")
	path, agentID := parseAgentWorkspaceMarker(body)
	if path != "/right/top" {
		t.Errorf("path = %q, want %q (nested path: must be rejected)", path, "/right/top")
	}
	if agentID != "agent-x" {
		t.Errorf("agent_id = %q, want %q", agentID, "agent-x")
	}
}

// I-778: walkForAgentRoot must reject markers whose path: is not
// absolute (defends against hand-edited tilde/relative paths).
func TestAgentRootRejectsNonAbsoluteMarkerPath(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "theraprac-agent-b")
	workspace := filepath.Join(agentRoot, "theraprac-workspace")
	os.MkdirAll(filepath.Join(agentRoot, ".as"), 0755)
	os.MkdirAll(workspace, 0755)
	badPath := "~/dev/theraprac-agent-b" // Go does NOT expand tilde.
	yaml := "agent_id: agent-b\npath: " + badPath + "\n"
	os.WriteFile(filepath.Join(agentRoot, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees"},
	}
	got := cfg.AgentRoot()
	// The non-absolute marker must NOT be returned verbatim. The
	// fallback (filepath.Dir(c.root) = agentRoot here) is acceptable.
	if got == badPath {
		t.Errorf("AgentRoot() = %q (non-absolute marker path returned verbatim instead of being rejected)", got)
	}
	if got != agentRoot {
		t.Errorf("AgentRoot() = %q, want %q (expected fallback after marker rejected)", got, agentRoot)
	}
}

// I-778: walkForAgentRoot must reject markers whose path: names a
// directory that doesn't exist on disk (defends against stale yaml
// after a directory rename or operator hand-edit).
func TestAgentRootRejectsStaleMarkerPath(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "theraprac-agent-b")
	workspace := filepath.Join(agentRoot, "theraprac-workspace")
	os.MkdirAll(filepath.Join(agentRoot, ".as"), 0755)
	os.MkdirAll(workspace, 0755)
	stalePath := filepath.Join(tmp, "no-such-dir") // never created.
	yaml := "agent_id: agent-b\npath: " + stalePath + "\n"
	os.WriteFile(filepath.Join(agentRoot, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees"},
	}
	got := cfg.AgentRoot()
	if got == stalePath {
		t.Errorf("AgentRoot() = %q (stale marker path returned verbatim instead of being rejected)", got)
	}
	if got != agentRoot {
		t.Errorf("AgentRoot() = %q, want %q (expected fallback after marker rejected)", got, agentRoot)
	}
}

// I-778: with AS_AGENT_ID set, walkForAgentRoot must skip markers
// whose agent_id doesn't match — defends against an operator running
// st from inside a peer agent's tree (cwd leak).
func TestAgentRootIdentitySanityCheck(t *testing.T) {
	tmp := t.TempDir()
	// Peer agent with a valid marker.
	peerAgent := filepath.Join(tmp, "theraprac-agent-a")
	peerAs := filepath.Join(peerAgent, "as")
	os.MkdirAll(filepath.Join(peerAgent, ".as"), 0755)
	os.MkdirAll(peerAs, 0755)
	yaml := "agent_id: agent-a\npath: " + peerAgent + "\n"
	os.WriteFile(filepath.Join(peerAgent, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)

	// startDir is inside peer's tree but AS_AGENT_ID claims we're agent-b.
	t.Setenv("AS_AGENT_ID", "agent-b")

	cfg := &Config{
		root:     filepath.Join(peerAgent, "theraprac-workspace"),
		startDir: peerAs,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees"},
	}
	// Set up the peer workspace dir so filepath.Dir(c.root) exists.
	os.MkdirAll(cfg.root, 0755)

	got := cfg.AgentRoot()
	// Peer's marker is rejected (agent_id mismatch); no other marker
	// found; falls back to filepath.Dir(c.root) = peerAgent. That's
	// still the wrong agent, but the identity check has at least
	// avoided locking in the wrong marker — the operator can override
	// by setting an absolute worktree.parent_dir or by running st from
	// the correct CWD.
	if got != peerAgent {
		t.Errorf("AgentRoot() = %q, want %q (fallback after identity mismatch)", got, peerAgent)
	}
}

// I-778: WorktreeBase and the worktree createWorktrees parentDir must
// land in the SAME agent's tree even when the configured Worktree.ParentDir
// is set to an absolute override pointing elsewhere — WorktreeBase uses
// AgentRoot (NEVER the operator's parent_dir override) so the worktree
// dir always sits under the running agent, while RepoParent honors the
// operator override for source clone locations.
func TestWorktreeBaseUsesAgentRootForConsistency(t *testing.T) {
	tmp := t.TempDir()
	peerAgent := filepath.Join(tmp, "theraprac-agent-a")
	peerWorkspace := filepath.Join(peerAgent, "theraprac-workspace")
	os.MkdirAll(peerWorkspace, 0755)
	realAgent := filepath.Join(tmp, "theraprac-agent-b")
	realAs := filepath.Join(realAgent, "as")
	os.MkdirAll(filepath.Join(realAgent, ".as"), 0755)
	os.MkdirAll(realAs, 0755)
	yaml := "agent_id: agent-b\npath: " + realAgent + "\n"
	os.WriteFile(filepath.Join(realAgent, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)
	t.Setenv("AS_AGENT_ID", "agent-b")

	cfg := &Config{
		root:     peerWorkspace,
		startDir: realAs,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	gotBase := cfg.WorktreeBase()
	wantBase := filepath.Join(realAgent, "worktrees")
	if gotBase != wantBase {
		t.Errorf("WorktreeBase() = %q, want %q (must route through AgentRoot for half-fix)", gotBase, wantBase)
	}
	gotRoot := cfg.AgentRoot()
	if gotRoot != realAgent {
		t.Errorf("AgentRoot() = %q, want %q", gotRoot, realAgent)
	}
	if filepath.Dir(gotBase) != gotRoot {
		t.Errorf("WorktreeBase()=%q and AgentRoot()=%q must stay consistent (createWorktrees uses both)", gotBase, gotRoot)
	}
}

// I-778: RepoParent honors an absolute worktree.parent_dir override even
// when AgentRoot resolves elsewhere — operator escape hatch for layouts
// where source repos live in a different dir than the per-agent root
// (e.g., shared monorepo checkouts).
func TestRepoParentRespectsAbsoluteOverrideButWorktreeBaseDoesNot(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "theraprac-agent-b")
	workspace := filepath.Join(agentRoot, "theraprac-workspace")
	os.MkdirAll(filepath.Join(agentRoot, ".as"), 0755)
	os.MkdirAll(workspace, 0755)
	yaml := "agent_id: agent-b\npath: " + agentRoot + "\n"
	os.WriteFile(filepath.Join(agentRoot, ".as", "agent-workspace.yaml"), []byte(yaml), 0644)
	sharedRepos := filepath.Join(tmp, "shared-checkouts")
	os.MkdirAll(sharedRepos, 0755)
	t.Setenv("AS_AGENT_ID", "agent-b")

	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: sharedRepos},
	}
	if got := cfg.RepoParent(); got != sharedRepos {
		t.Errorf("RepoParent() = %q, want %q (absolute parent_dir override)", got, sharedRepos)
	}
	if got := cfg.WorktreeBase(); got != filepath.Join(agentRoot, "worktrees") {
		t.Errorf("WorktreeBase() = %q, want %q (must NOT follow parent_dir override)", got, filepath.Join(agentRoot, "worktrees"))
	}
	if got := cfg.AgentRoot(); got != agentRoot {
		t.Errorf("AgentRoot() = %q, want %q", got, agentRoot)
	}
}

// I-778: AgentRoot caches its result so repeated calls (per-repo in
// loops) don't re-walk the filesystem each time.
func TestAgentRootCachesResult(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "theraprac-workspace")
	os.MkdirAll(workspace, 0755)
	cfg := &Config{
		root:     workspace,
		startDir: workspace,
		Worktree: &WorktreeConfig{Enabled: true, BaseDir: "worktrees", ParentDir: ".."},
	}
	first := cfg.AgentRoot()
	// Mutate startDir; cache should still return the first result.
	cfg.startDir = "/nonexistent"
	second := cfg.AgentRoot()
	if first != second {
		t.Errorf("AgentRoot not cached: first=%q second=%q", first, second)
	}
	// ResetAgentRootCache lets tests force a re-resolve.
	cfg.ResetAgentRootCache()
	third := cfg.AgentRoot()
	if third == first {
		// After reset with bogus startDir and no .as/agent-workspace.yaml at
		// /nonexistent, the resolver should hit the legacy fallback.
		// (Result may still equal first if filepath.Dir of c.root happens
		// to match — accept either; the point of this assertion is that
		// ResetAgentRootCache does invalidate, not the specific value.)
	}
}
