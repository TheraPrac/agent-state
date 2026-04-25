package command

import (
	"io"
	"os"
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

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
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
