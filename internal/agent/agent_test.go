package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// setupAgentTestCfg builds a Config rooted at a temp dir with a minimal
// .as/config.yaml so cfg.AgentsDir() resolves correctly.
func setupAgentTestCfg(t *testing.T) *config.Config {
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

func TestRegisterAndCleanup(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	reg, cleanup, err := Register(cfg, Options{
		BaseAgentID: "agent-a",
		Role:        "worker",
		Scope:       "sprint:test",
		SessionID:   "sess-123",
		PID:         os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.AgentID != "agent-a-1" {
		t.Errorf("AgentID = %q, want agent-a-1", reg.AgentID)
	}
	if reg.Root != "agent-a" {
		t.Errorf("Root = %q, want agent-a (defaults to BaseAgentID)", reg.Root)
	}
	if reg.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", reg.PID, os.Getpid())
	}

	// File should exist on disk.
	path := filepath.Join(cfg.AgentsDir(), reg.AgentID+".yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registration file not written: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent_id: agent-a-1", "root: agent-a", "role: worker", "scope: sprint:test", "session_id: sess-123"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("file missing %q:\n%s", want, body)
		}
	}

	// Cleanup removes the file.
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove file: %v", err)
	}
	// Idempotent: calling again is safe.
	cleanup()
}

func TestSuffixAssignment(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	r1, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r1.AgentID != "agent-a-1" || r2.AgentID != "agent-a-2" {
		t.Errorf("expected agent-a-1/agent-a-2, got %q/%q", r1.AgentID, r2.AgentID)
	}

	// Lowest free integer fills holes: remove agent-a-1, next reg should
	// claim that slot rather than agent-a-3.
	if err := os.Remove(filepath.Join(cfg.AgentsDir(), r1.AgentID+".yaml")); err != nil {
		t.Fatal(err)
	}
	r3, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r3.AgentID != "agent-a-1" {
		t.Errorf("expected agent-a-1 (filling hole), got %q", r3.AgentID)
	}

	// Different prefixes are independent counters.
	r4, _, err := Register(cfg, Options{BaseAgentID: "agent-b", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if r4.AgentID != "agent-b-1" {
		t.Errorf("expected agent-b-1, got %q", r4.AgentID)
	}
}

func TestStaleSweep(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	// Live registration (this process).
	live, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}

	// Manually write a "stale" registration with a guaranteed-dead PID.
	// Linux/macOS reserve PID 0; PID 999999 is typically unused. Pick a
	// large value AND verify it isn't actually live before relying on it.
	deadPID := 999999
	for IsPIDLive(deadPID) {
		deadPID++ // unlikely loop, but safe
	}
	stalePath := filepath.Join(cfg.AgentsDir(), "agent-z-7.yaml")
	if err := os.WriteFile(stalePath, []byte("agent_id: agent-z-7\nroot: agent-z\npid: 999999\nstarted: 2026-01-01T00:00:00Z\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Patch the file to use the actually-dead PID we found above.
	if deadPID != 999999 {
		body, _ := os.ReadFile(stalePath)
		patched := strings.Replace(string(body), "pid: 999999", "pid: 999998", 1)
		_ = os.WriteFile(stalePath, []byte(patched), 0644)
	}

	cleaned, err := Sweep(cfg)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(cleaned) != 1 || cleaned[0] != "agent-z-7" {
		t.Errorf("expected sweep to remove [agent-z-7], got %v", cleaned)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale file not removed: %v", err)
	}
	livePath := filepath.Join(cfg.AgentsDir(), live.AgentID+".yaml")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("sweep removed the live registration!: %v", err)
	}
}

func TestChildAgentLineage(t *testing.T) {
	cfg := setupAgentTestCfg(t)

	root, _, err := Register(cfg, Options{
		BaseAgentID: "agent-a",
		Role:        "worker",
		PID:         os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if root.Parent != "" {
		t.Errorf("root agent should have no parent, got %q", root.Parent)
	}
	if root.Root != root.Parent && root.Root != "agent-a" {
		t.Errorf("root agent's Root should equal BaseAgentID for root agents, got %q", root.Root)
	}

	// Child: parent + root carry through, suffix assigned under parent prefix.
	child, _, err := Register(cfg, Options{
		BaseAgentID:      "agent-a",
		ParentAgentID:    root.AgentID, // "agent-a-1"
		RootAgentID:      root.Root,
		Role:             "child",
		SpawnedBySession: "parent-sess",
		PID:              os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != root.AgentID {
		t.Errorf("child Parent = %q, want %q", child.Parent, root.AgentID)
	}
	if child.Root != "agent-a" {
		t.Errorf("child Root = %q, want agent-a (inherited)", child.Root)
	}
	if !strings.HasPrefix(child.AgentID, root.AgentID+"-") {
		t.Errorf("child AgentID should start with %q-, got %q", root.AgentID, child.AgentID)
	}
	if child.SpawnedBySession != "parent-sess" {
		t.Errorf("SpawnedBySession lost in registration: %q", child.SpawnedBySession)
	}

	// Round-trip: load and verify lineage survives parse.
	loaded, err := LoadRegistration(cfg, child.AgentID)
	if err != nil {
		t.Fatalf("LoadRegistration: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded == nil")
	}
	if loaded.Parent != root.AgentID || loaded.Root != "agent-a" || loaded.Role != "child" {
		t.Errorf("round-trip lost fields: parent=%q root=%q role=%q", loaded.Parent, loaded.Root, loaded.Role)
	}
}

func TestLoadRegistrationMissing(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	reg, err := LoadRegistration(cfg, "nope-1")
	if err != nil {
		t.Fatalf("err on missing: %v", err)
	}
	if reg != nil {
		t.Errorf("expected nil for missing file, got %+v", reg)
	}
}

func TestListRegistrationsSkipsBadFiles(t *testing.T) {
	cfg := setupAgentTestCfg(t)
	if err := os.MkdirAll(cfg.AgentsDir(), 0755); err != nil {
		t.Fatal(err)
	}
	// One good registration.
	if _, _, err := Register(cfg, Options{BaseAgentID: "agent-a", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// One garbage file — listing must skip it without erroring.
	if err := os.WriteFile(filepath.Join(cfg.AgentsDir(), "garbage.yaml"), []byte("not yaml\nat all"), 0644); err != nil {
		t.Fatal(err)
	}
	regs, err := ListRegistrations(cfg)
	if err != nil {
		t.Fatalf("ListRegistrations: %v", err)
	}
	if len(regs) != 1 || regs[0].AgentID != "agent-a-1" {
		t.Errorf("expected 1 good reg, got %d: %+v", len(regs), regs)
	}
}
