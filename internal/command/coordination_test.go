package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/store"
)

func setupCoordEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "T-001-x.md"), []byte(`id: T-001
type: task
status: active
created: 2026-04-26T10:00:00-06:00
last_touched: 2026-04-26T10:00:00-06:00

completed: null

title: Self item
assigned_to: agent-self
claimed_by: sess-self

depends_on:
- []
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "T-002-y.md"), []byte(`id: T-002
type: task
status: active
created: 2026-04-26T10:00:00-06:00
last_touched: 2026-04-26T10:00:00-06:00

completed: null

title: Peer item
assigned_to: agent-peer
claimed_by: sess-peer

depends_on:
- []
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg
}

// TestPromptCoordinationBlock asserts the structure of the generated
// block: three sections, with active agents AND a fresh mail message
// reflected back in the rendered text.
func TestPromptCoordinationBlock(t *testing.T) {
	s, cfg := setupCoordEnv(t)

	// Two live agents — self + peer. PIDs use this test process so
	// IsPIDLive returns true for both.
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-self", SessionID: "sess-self", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-peer", SessionID: "sess-peer", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	// One recent mail for self.
	if _, err := mail.Send(cfg, mail.Message{
		From: "agent-peer-1", To: "agent-self-1", Kind: mail.KindWarning,
		Body: "auth middleware will conflict", Item: "T-300",
	}); err != nil {
		t.Fatal(err)
	}

	block := buildCoordinationBlock(s, cfg, "agent-self-1", "T-001")

	for _, want := range []string{
		"## Active Agents",
		"agent-self-1 (you)",
		"agent-peer-1",
		"## Recent Mail",
		"[warning]",
		"auth middleware will conflict",
		"## Coordination Rules",
		"st mail send <agent-id>",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q\n--- block ---\n%s", want, block)
		}
	}
}

// TestPromptMailConsumed: every mail rendered into the block is moved
// from pending to archive — one-time delivery. A subsequent block
// build sees no fresh mail.
func TestPromptMailConsumed(t *testing.T) {
	s, cfg := setupCoordEnv(t)
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-self", SessionID: "sess-self", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := mail.Send(cfg, mail.Message{
			From: "agent-peer-1", To: "agent-self-1", Kind: mail.KindWarning,
			Body: "msg" + string(rune('0'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	block1 := buildCoordinationBlock(s, cfg, "agent-self-1", "T-001")
	for i := 0; i < 3; i++ {
		want := "msg" + string(rune('0'+i))
		if !strings.Contains(block1, want) {
			t.Errorf("first block missing %q:\n%s", want, block1)
		}
	}

	// Pending mailbox should be empty after the consume.
	pending, err := mail.List(cfg, "agent-self-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected pending empty after consume, got %d", len(pending))
	}

	// Second block has no fresh mail to surface.
	block2 := buildCoordinationBlock(s, cfg, "agent-self-1", "T-001")
	if strings.Contains(block2, "msg0") || strings.Contains(block2, "msg1") || strings.Contains(block2, "msg2") {
		t.Errorf("second block re-surfaced consumed mail:\n%s", block2)
	}
	if !strings.Contains(block2, "Recent Mail") || !strings.Contains(block2, "(none)") {
		t.Errorf("second block should mark Recent Mail (none):\n%s", block2)
	}
}

// TestPromptActiveAgentsLiveOnly: a registration whose PID is dead
// must NOT appear in the block. Live registrations do.
func TestPromptActiveAgentsLiveOnly(t *testing.T) {
	s, cfg := setupCoordEnv(t)
	// Live registration.
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-self", SessionID: "sess-self", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}

	// Dead registration: hand-write a file with a PID we know is gone.
	deadPID := 999998
	for agent.IsPIDLive(deadPID) {
		deadPID++
	}
	deadPath := filepath.Join(cfg.AgentsDir(), "agent-zombie-1.yaml")
	if err := os.WriteFile(deadPath, []byte(`agent_id: agent-zombie-1
root: agent-zombie
pid: 999998
started: 2026-01-01T00:00:00Z
session_id: sess-zombie
`), 0644); err != nil {
		t.Fatal(err)
	}

	block := buildCoordinationBlock(s, cfg, "agent-self-1", "T-001")

	if !strings.Contains(block, "agent-self-1") {
		t.Errorf("live agent missing from block:\n%s", block)
	}
	if strings.Contains(block, "agent-zombie") {
		t.Errorf("dead-PID registration leaked into block:\n%s", block)
	}
}

// Older mail (before the window) should NOT be surfaced AND NOT be
// consumed — left pending for a future widened window or manual review.
func TestPromptMailWindowSkipsOldMail(t *testing.T) {
	s, cfg := setupCoordEnv(t)
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-self", SessionID: "sess-self", PID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	// Hand-craft an old message — file write avoids needing to mock time.
	old := mail.Message{
		From: "agent-peer-1", To: "agent-self-1", Kind: mail.KindWarning,
		Body: "ancient", At: time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	}
	if _, err := mail.Send(cfg, old); err != nil {
		t.Fatal(err)
	}
	block := buildCoordinationBlock(s, cfg, "agent-self-1", "T-001")
	if strings.Contains(block, "ancient") {
		t.Errorf("old mail should not be surfaced:\n%s", block)
	}
	pending, _ := mail.List(cfg, "agent-self-1")
	if len(pending) != 1 {
		t.Errorf("old mail should remain pending (not consumed), got %d", len(pending))
	}
}
