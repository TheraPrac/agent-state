package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/agent"
)

func TestAgentRegisterAndDeregister(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt") // cfg.Identity().ID

	if code := AgentRegister(cfg, AgentRegisterOpts{PID: 1234, SessionID: "sess-aps"}); code != 0 {
		t.Fatalf("AgentRegister exit %d, want 0", code)
	}
	reg, err := agent.LoadRegistration(cfg, "agent-tt")
	if err != nil || reg == nil {
		t.Fatalf("registration not written: %v / %v", reg, err)
	}
	if reg.AgentID != "agent-tt" || reg.PID != 1234 || reg.SessionID != "sess-aps" {
		t.Errorf("registration = %+v, want agent-tt/1234/sess-aps", reg)
	}

	// Re-register (resume) refreshes in place, still one base-id file.
	if code := AgentRegister(cfg, AgentRegisterOpts{PID: 5678, SessionID: "sess-2"}); code != 0 {
		t.Fatalf("re-register exit %d", code)
	}
	reg, _ = agent.LoadRegistration(cfg, "agent-tt")
	if reg.PID != 5678 {
		t.Errorf("re-register did not refresh PID: %+v", reg)
	}
	if m, _ := filepath.Glob(filepath.Join(cfg.AgentsDir(), "agent-tt*.yaml")); len(m) != 1 {
		t.Errorf("want exactly one agent-tt file, got %v", m)
	}

	if code := AgentDeregister(cfg); code != 0 {
		t.Fatalf("AgentDeregister exit %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(cfg.AgentsDir(), "agent-tt.yaml")); !os.IsNotExist(err) {
		t.Errorf("registration still present after deregister: %v", err)
	}
}

func TestAgentRegister_HookSafeWithoutIdentity(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// No AS_AGENT_ID and a temp root → Identity().ID == "". The
	// SessionStart hook must never be broken by this: exit 0, no file.
	t.Setenv("AS_AGENT_ID", "")
	if code := AgentRegister(cfg, AgentRegisterOpts{}); code != 0 {
		t.Errorf("AgentRegister with no identity exit %d, want 0 (hook-safe)", code)
	}
}
