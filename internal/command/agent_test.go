package command

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentList(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-b")
	home := t.TempDir()
	t.Setenv("HOME", home)

	credDir := filepath.Join(home, ".theraprac")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(credDir, "aws-agent-a.json"), `{}`)
	writeFile(t, filepath.Join(credDir, "gh-agent-a.json"), `{}`)
	writeFile(t, filepath.Join(credDir, "aws-agent-b.json"), `{}`)
	writeFile(t, filepath.Join(credDir, "gh-agent-b-session.json"), `{}`)

	stdout := captureStdout(t, func() {
		if code := AgentList(cfg); code != 0 {
			t.Fatalf("AgentList returned %d", code)
		}
	})

	if !strings.Contains(stdout, "agent-a") || !strings.Contains(stdout, "yes  yes") {
		t.Fatalf("agent-a not listed with AWS/GH credentials:\n%s", stdout)
	}
	if !strings.Contains(stdout, "agent-b") || !strings.Contains(stdout, "yes  no") || !strings.Contains(stdout, "*") {
		t.Fatalf("agent-b current marker or credentials wrong:\n%s", stdout)
	}
	if strings.Contains(stdout, "session") {
		t.Fatalf("session cache should not be listed as an agent:\n%s", stdout)
	}
}

func TestAgentAuthRunsAgentEnv(t *testing.T) {
	_, cfg := setupTestEnv(t)
	scriptsDir := fakeAgentScripts(t)
	t.Setenv("ST_AGENT_SCRIPTS_DIR", scriptsDir)

	stdout := captureStdout(t, func() {
		code := AgentAuth(cfg, AgentAuthOpts{Name: "agent-c", SkipAWS: true, Force: true})
		if code != 0 {
			t.Fatalf("AgentAuth returned %d", code)
		}
	})

	if !strings.Contains(stdout, "AUTH --name agent-c --skip-aws --force") {
		t.Fatalf("unexpected auth output: %q", stdout)
	}
}

func TestAgentBootstrapRunsSelectedScripts(t *testing.T) {
	_, cfg := setupTestEnv(t)
	scriptsDir := fakeAgentScripts(t)
	t.Setenv("ST_AGENT_SCRIPTS_DIR", scriptsDir)

	stdout := captureStdout(t, func() {
		code := AgentBootstrap(cfg, AgentBootstrapOpts{
			Name: "agent-d", RotateKey: true, DryRun: true, Owner: "TheraPrac", SkipInstall: true,
		})
		if code != 0 {
			t.Fatalf("AgentBootstrap returned %d", code)
		}
	})

	if !strings.Contains(stdout, "AWS --name agent-d --rotate-key --dry-run") {
		t.Fatalf("AWS bootstrap did not run with expected args:\n%s", stdout)
	}
	if !strings.Contains(stdout, "GH --name agent-d --owner TheraPrac --skip-install") {
		t.Fatalf("GH bootstrap did not run with expected args:\n%s", stdout)
	}
}

func TestAgentAutoAuthBestEffort(t *testing.T) {
	_, cfg := setupTestEnv(t)
	scriptsDir := fakeAgentScripts(t)
	t.Setenv("ST_AGENT_SCRIPTS_DIR", scriptsDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".theraprac")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(credDir, "aws-agent-e.json"), `{}`)
	writeFile(t, filepath.Join(credDir, "gh-agent-e.json"), `{}`)

	AgentAutoAuth(cfg, "agent-e")

	data, err := os.ReadFile(filepath.Join(scriptsDir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "AUTH --name agent-e") {
		t.Fatalf("auto auth did not call agent-env:\n%s", string(data))
	}
}

func TestAgentWorkspaceCreateDryRunPrintsCompletePlan(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceCreate(cfg, AgentWorkspaceCreateOpts{
			Agent: "b", Branch: "main", Full: true, DryRun: true,
		})
		if code != 0 {
			t.Fatalf("AgentWorkspaceCreate returned %d", code)
		}
	})

	for _, want := range []string{
		"Agent workspace create plan: agent-b",
		filepath.Join(agentsRoot, "theraprac-agent-b"),
		"branch: main",
		"theraprac-api",
		"theraprac-web",
		"theraprac-infra",
		"theraprac-workspace",
		"ports: api=8280 web=3200 db=5632 mailpit=8225 stripe=12311",
		"compose_project: theraprac_agent_b",
		"docker_label: theraprac.agent=agent-b",
		"registry:",
		"workspace_config:",
		"dry-run: no filesystem, git, Docker, or env changes will be made",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, stdout)
		}
	}
	// I-475: as repo must appear in the plan with a make-install hook so
	// the per-agent dispatcher (I-419) finds bin/st after create.
	var asLine string
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "as ") {
			asLine = line
			break
		}
	}
	if asLine == "" {
		t.Errorf("plan output missing the as repo row:\n%s", stdout)
	} else if !strings.Contains(asLine, "make install") {
		t.Errorf("as row missing make-install hook: %q", asLine)
	}
	if _, err := os.Stat(filepath.Join(agentsRoot, "theraprac-agent-b")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create target, stat err=%v", err)
	}
}

func TestAgentWorkspaceCreateDryRunDetectsPartialSymlink(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	target := filepath.Join(agentsRoot, "theraprac-agent-b")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	// theraprac-api is NOT a shared-symlink repo, so a stray symlink should
	// be flagged for repair into an independent clone.
	if err := os.Symlink("/tmp/source-api", filepath.Join(target, "theraprac-api")); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceCreate(cfg, AgentWorkspaceCreateOpts{
			Agent: "agent-b", Branch: "main", Full: true, DryRun: true,
		})
		if code != 0 {
			t.Fatalf("AgentWorkspaceCreate returned %d", code)
		}
	})

	if !strings.Contains(stdout, "theraprac-api") || !strings.Contains(stdout, "state=symlink") ||
		!strings.Contains(stdout, "repair symlink -> independent clone") {
		t.Fatalf("partial symlink not explained:\n%s", stdout)
	}
}

// I-418: theraprac-workspace is shared across agents via symlink. The plan
// output must reflect that a symlink IS the expected end state, not a
// "partial" condition needing repair into an independent clone.
func TestAgentWorkspaceCreatePlanShowsSharedWorkspaceSymlink(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceCreate(cfg, AgentWorkspaceCreateOpts{
			Agent: "agent-c", Branch: "main", Full: true, DryRun: true,
		})
		if code != 0 {
			t.Fatalf("AgentWorkspaceCreate returned %d", code)
		}
	})

	// Absent state should plan to create the symlink.
	wantSubstrings := []string{
		"theraprac-workspace",
		"state=absent",
		"create symlink -> ../theraprac-workspace",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
	// Independent-clone wording must NOT appear for the workspace.
	if strings.Contains(stdout, "theraprac-workspace") &&
		strings.Contains(stdout, "repair symlink -> independent clone") {
		// only fail if that wording sits on the workspace row; check by line
		for _, line := range strings.Split(stdout, "\n") {
			if strings.Contains(line, "theraprac-workspace") &&
				strings.Contains(line, "independent clone") {
				t.Errorf("workspace row should not say 'independent clone':\n%s", line)
			}
		}
	}
}

// I-418: when the workspace symlink already exists and points correctly,
// the plan should describe a verify (no-op), not a repair.
func TestAgentWorkspaceCreatePlanVerifiesCorrectSymlink(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	target := filepath.Join(agentsRoot, "theraprac-agent-c")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../theraprac-workspace", filepath.Join(target, "theraprac-workspace")); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceCreate(cfg, AgentWorkspaceCreateOpts{
			Agent: "agent-c", Branch: "main", Full: true, DryRun: true,
		})
		if code != 0 {
			t.Fatalf("AgentWorkspaceCreate returned %d", code)
		}
	})

	for _, want := range []string{"theraprac-workspace", "state=symlink", "verify symlink -> ../theraprac-workspace"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("plan output missing %q:\n%s", want, stdout)
		}
	}
}

func TestAgentWorkspaceStatusShowsIdentityPortsAndRepoState(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	target := filepath.Join(agentsRoot, "theraprac-agent-c", "theraprac-api")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceStatus(cfg, AgentWorkspaceStatusOpts{Agent: "c"})
		if code != 0 {
			t.Fatalf("AgentWorkspaceStatus returned %d", code)
		}
	})

	for _, want := range []string{
		"Agent workspace: agent-c",
		"identity: agent-c",
		"ports: api=8380 web=3300 db=5732 mailpit=8325 stripe=12411",
		"compose: theraprac_agent_c",
		"theraprac-api",
		"service_health: unknown",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q:\n%s", want, stdout)
		}
	}
}

func TestAgentWorkspaceDestroyDryRunListsResources(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	target := filepath.Join(agentsRoot, "theraprac-agent-d")
	if err := os.MkdirAll(filepath.Join(target, "theraprac-api"), 0755); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		code := AgentWorkspaceDestroy(cfg, AgentWorkspaceDestroyOpts{Agent: "d", DryRun: true})
		if code != 0 {
			t.Fatalf("AgentWorkspaceDestroy returned %d", code)
		}
	})

	for _, want := range []string{
		"Destroy agent workspace: agent-d",
		target,
		"postgres container: theraprac-agent-d-postgres",
		"postgres volume:    theraprac-agent-d-postgres-data",
		"mailpit container:  theraprac-agent-d-mailpit",
		"dry-run: no files, containers, or volumes removed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("destroy dry-run missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("dry-run should leave target in place: %v", err)
	}
}

func TestAgentWorkspaceDestroyRefusesDirtyRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	repo := filepath.Join(agentsRoot, "theraprac-agent-e", "theraprac-api")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "init")
	writeFile(t, filepath.Join(repo, "dirty.txt"), "dirty\n")

	code := AgentWorkspaceDestroy(cfg, AgentWorkspaceDestroyOpts{Agent: "e"})
	if code == 0 {
		t.Fatal("destroy should refuse dirty git repos")
	}
	if _, err := os.Stat(filepath.Join(agentsRoot, "theraprac-agent-e")); err != nil {
		t.Fatalf("refused destroy should leave workspace: %v", err)
	}
}

func TestPersistAgentWorkspaceConfigWritesRegistryAndLocalConfig(t *testing.T) {
	_, cfg := setupTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)

	plan, err := buildAgentWorkspacePlan(cfg, "f", "feature/demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAgentWorkspaceConfig(plan); err != nil {
		t.Fatalf("persistAgentWorkspaceConfig: %v", err)
	}

	for _, path := range []string{agentWorkspaceRegistryPath(plan), agentWorkspaceLocalConfigPath(plan)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected config at %s: %v", path, err)
		}
		s := string(data)
		for _, want := range []string{
			"agent_id: agent-f",
			"branch: feature/demo",
			"compose_project: theraprac_agent_f",
			"docker_label: theraprac.agent=agent-f",
			"web: 3600",
			"theraprac-api:",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s missing %q:\n%s", path, want, s)
			}
		}
	}
}

func fakeAgentScripts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "agent-env.sh"), `#!/usr/bin/env bash
echo "AUTH $*"
echo "AUTH $*" >> "$(dirname "$0")/calls.log"
`)
	writeExecutable(t, filepath.Join(dir, "agent-bootstrap-aws.sh"), `#!/usr/bin/env bash
echo "AWS $*"
`)
	writeExecutable(t, filepath.Join(dir, "agent-bootstrap-gh.sh"), `#!/usr/bin/env bash
echo "GH $*"
`)
	return dir
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestLinkClaudeContext(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	target := filepath.Join(root, "theraprac-agent-z")
	workspace := filepath.Join(target, "theraprac-workspace")
	claudeConfig := filepath.Join(workspace, "claude-config")
	if err := os.MkdirAll(filepath.Join(claudeConfig, "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "agent-memory"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claudeConfig, "CLAUDE.md"), "# CLAUDE\n")
	writeFile(t, filepath.Join(claudeConfig, "settings.json"), "{}\n")

	plan := agentWorkspacePlan{TargetDir: target}

	if err := linkClaudeContext(plan, false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Idempotent: second call must not error and must not change anything.
	if err := linkClaudeContext(plan, false); err != nil {
		t.Fatalf("second call: %v", err)
	}

	encoded := strings.ReplaceAll(target, "/", "-")
	memoryLink := filepath.Join(home, ".claude", "projects", encoded, "memory")
	expect := map[string]string{
		filepath.Join(target, "CLAUDE.md"):              filepath.Join(claudeConfig, "CLAUDE.md"),
		filepath.Join(target, ".claude", "hooks"):       filepath.Join(claudeConfig, "hooks"),
		filepath.Join(target, ".claude", "settings.json"): filepath.Join(claudeConfig, "settings.json"),
		memoryLink: filepath.Join(workspace, "agent-memory"),
	}
	for link, want := range expect {
		got, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("readlink %s: %v", link, err)
		}
		if got != want {
			t.Errorf("symlink %s: got %s, want %s", link, got, want)
		}
	}

	// Wrong target without --repair must fail.
	bad := filepath.Join(target, "CLAUDE.md")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "agent-memory"), bad); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeContext(plan, false); err == nil {
		t.Fatal("expected error when symlink points to wrong target without --repair")
	}
	if err := linkClaudeContext(plan, true); err != nil {
		t.Fatalf("repair call: %v", err)
	}
	got, err := os.Readlink(bad)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(claudeConfig, "CLAUDE.md") {
		t.Errorf("repair did not fix CLAUDE.md symlink: got %s", got)
	}

	// Real file at the link path is always an error.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bad, "not a symlink")
	if err := linkClaudeContext(plan, true); err == nil {
		t.Fatal("expected error when link path holds a real file")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = old
		_ = w.Close()
		_ = r.Close()
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
