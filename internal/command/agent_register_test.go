package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/agent"
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
	// ...and it must NOT have written anything (the comment's full
	// claim is "exit 0, no file").
	if m, _ := filepath.Glob(filepath.Join(cfg.AgentsDir(), "*.yaml")); len(m) != 0 {
		t.Errorf("AgentRegister with no identity wrote %v, want nothing", m)
	}
	// Deregister with no identity is also a no-op success (idempotent).
	if code := AgentDeregister(cfg); code != 0 {
		t.Errorf("AgentDeregister with no identity exit %d, want 0 (idempotent)", code)
	}
}

func TestRegisterSelf_PreservesStartedAcrossSameSessionResume(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	if code := AgentRegister(cfg, AgentRegisterOpts{PID: 11, SessionID: "S1"}); code != 0 {
		t.Fatalf("first register exit %d", code)
	}
	first, _ := agent.LoadRegistration(cfg, "agent-tt")

	// Same session re-register (resume/compact re-fires SessionStart):
	// PID may change, Started must stay → continuous UPTIME.
	if code := AgentRegister(cfg, AgentRegisterOpts{PID: 22, SessionID: "S1"}); code != 0 {
		t.Fatalf("resume register exit %d", code)
	}
	resumed, _ := agent.LoadRegistration(cfg, "agent-tt")
	if resumed.Started != first.Started {
		t.Errorf("same-session resume reset Started: %q → %q (want preserved)", first.Started, resumed.Started)
	}
	if resumed.PID != 22 {
		t.Errorf("resume did not refresh PID: %d", resumed.PID)
	}

	// (The "new SessionID resets Started" half is proven
	// deterministically in agent.TestRegisterSelf_StartedReset using a
	// 2020 sentinel — comparing two time.Now() RFC3339 values here
	// would be flaky when both land in the same second.)
}
