package command

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// reapedPID returns a PID that's guaranteed dead at the time of return:
// fork /usr/bin/true (or echo as a fallback), wait for it to exit, then
// the OS may eventually recycle the slot but won't until something else
// races for it. Reliable on macOS and Linux without depending on
// platform-specific PID-ceiling assumptions.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/usr/bin/true")
	if _, err := exec.LookPath("/usr/bin/true"); err != nil {
		cmd = exec.Command("/bin/echo")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning /usr/bin/true to capture a dead PID: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// Drift detection (I-404 follow-up): when two or more LIVE agents
// (PID alive in registry) report different st commits, status must
// surface a banner so operators rebuild before iterating.
//
// These tests use os.Getpid() as the "live" PID for both registrations,
// since the test process itself is alive and `kill -0 $$` succeeds.
// Using two distinct PIDs would force one to be dead, masking the
// drift case we want to exercise.

func writeAgentRegistrationYAML(t *testing.T, dir, id, commit string, pid int) {
	t.Helper()
	body := fmt.Sprintf("agent_id: %s\nroot: %s\npid: %d\nstarted: 2026-04-27T10:00:00Z\n", id, id, pid)
	if commit != "" {
		body += "commit: " + commit + "\n"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func newDriftTestCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestBinaryDriftSilentSingleAgent(t *testing.T) {
	cfg := newDriftTestCfg(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-a-1", "aaaa1111", os.Getpid())

	var buf bytes.Buffer
	printBinaryDriftWarning(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on single agent, got: %s", buf.String())
	}
}

func TestBinaryDriftSilentSameCommit(t *testing.T) {
	cfg := newDriftTestCfg(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-a-1", "samesha1", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-b-1", "samesha1", os.Getpid())

	var buf bytes.Buffer
	printBinaryDriftWarning(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent when commits match, got: %s", buf.String())
	}
}

func TestBinaryDriftWarnsOnDivergence(t *testing.T) {
	cfg := newDriftTestCfg(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-a-1", "aaaa1111111111111111111111111111aaaaaaaa", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-b-1", "bbbb2222222222222222222222222222bbbbbbbb", os.Getpid())

	var buf bytes.Buffer
	printBinaryDriftWarning(cfg, &buf)
	out := buf.String()
	if !strings.Contains(out, "binary drift") {
		t.Errorf("expected drift banner, got: %s", out)
	}
	if !strings.Contains(out, "agent-a-1") || !strings.Contains(out, "agent-b-1") {
		t.Errorf("expected both agent ids in output: %s", out)
	}
	if !strings.Contains(out, "aaaa1111") || !strings.Contains(out, "bbbb2222") {
		t.Errorf("expected short SHAs in output: %s", out)
	}
	if !strings.Contains(out, "git pull && make install") {
		t.Errorf("expected fix hint in output: %s", out)
	}
}

func TestBinaryDriftSkipsDeadAgents(t *testing.T) {
	cfg := newDriftTestCfg(t)
	// Live agent on commit A.
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-a-1", "aaaa1111", os.Getpid())
	// Dead agent on commit B — fork /usr/bin/true and wait, capture its
	// reaped PID. Cross-platform safe (vs. picking a high PID that may
	// be valid on Linux's larger pid space).
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-b-1", "bbbb2222", reapedPID(t))

	var buf bytes.Buffer
	printBinaryDriftWarning(cfg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent when only one live agent (other is dead), got: %s", buf.String())
	}
}

func TestBinaryDriftHandlesUnstamped(t *testing.T) {
	cfg := newDriftTestCfg(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-a-1", "aaaa1111", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-b-1", "", os.Getpid()) // legacy, no commit

	var buf bytes.Buffer
	printBinaryDriftWarning(cfg, &buf)
	out := buf.String()
	if !strings.Contains(out, "<unstamped>") {
		t.Errorf("expected <unstamped> marker for legacy reg, got: %s", out)
	}
	if !strings.Contains(out, "agent-b-1") {
		t.Errorf("expected agent-b-1 listed under unstamped: %s", out)
	}
}
