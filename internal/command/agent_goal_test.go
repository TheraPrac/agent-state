package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ensureGoalsDir creates the goals/ subdirectory under cfg.ItemDir() so
// seedGoalFile can write into it. setupTestEnv only creates tasks/issues/archive.
func ensureGoalsDir(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfg.ItemDir(), "goals"), 0755); err != nil {
		t.Fatalf("ensureGoalsDir: %v", err)
	}
}

func TestAgentGoalSet_PersistsFocus(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")
	ensureGoalsDir(t, cfg)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var out, errOut bytes.Buffer
	rc := agentGoalSetTo(&out, &errOut, s, cfg, "G-001")
	if rc != 0 {
		t.Fatalf("AgentGoalSet rc=%d stderr=%s", rc, errOut.String())
	}
	if !strings.Contains(out.String(), "G-001") {
		t.Errorf("output missing G-001: %s", out.String())
	}
	if got := agent.GetGoalFocus(cfg, "agent-tt"); got != "G-001" {
		t.Errorf("GoalFocus = %q, want G-001", got)
	}
}

func TestAgentGoalSet_RejectsInactiveGoal(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")
	ensureGoalsDir(t, cfg)
	seedGoalFile(t, cfg, "G-005", "draft", 20)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var out, errOut bytes.Buffer
	rc := agentGoalSetTo(&out, &errOut, s, cfg, "G-005")
	if rc == 0 {
		t.Fatal("expected non-zero exit for draft goal")
	}
	if !strings.Contains(errOut.String(), "draft") {
		t.Errorf("error must mention 'draft': %s", errOut.String())
	}
}

func TestAgentGoalSet_RejectsUnknownGoal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	var out, errOut bytes.Buffer
	rc := agentGoalSetTo(&out, &errOut, s, cfg, "G-999")
	if rc == 0 {
		t.Fatal("expected non-zero exit for missing goal")
	}
}

func TestAgentGoalClear_RemovesFocus(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	if err := agent.SetGoalFocus(cfg, "agent-tt", "G-001"); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	rc := agentGoalClearTo(&out, &errOut, cfg)
	if rc != 0 {
		t.Fatalf("AgentGoalClear rc=%d stderr=%s", rc, errOut.String())
	}
	if got := agent.GetGoalFocus(cfg, "agent-tt"); got != "" {
		t.Errorf("focus not cleared: %q", got)
	}
}

func TestAgentGoalShow_RendersCurrentFocus(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")
	ensureGoalsDir(t, cfg)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := agent.SetGoalFocus(cfg, "agent-tt", "G-001"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rc := agentGoalShowTo(&out, s, cfg)
	if rc != 0 {
		t.Fatalf("AgentGoalShow rc=%d", rc)
	}
	if !strings.Contains(out.String(), "G-001") {
		t.Errorf("output missing G-001: %s", out.String())
	}
}

func TestAgentGoalShow_PrintsNoneWhenUnset(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	var out bytes.Buffer
	rc := agentGoalShowTo(&out, s, cfg)
	if rc != 0 {
		t.Fatalf("AgentGoalShow rc=%d", rc)
	}
	if !strings.Contains(out.String(), "(none)") {
		t.Errorf("output missing '(none)': %s", out.String())
	}
}
