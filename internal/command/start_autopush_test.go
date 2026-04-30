package command

import (
	"testing"
)

// TestStart_AutoPushesOntoStack covers the I-412 default: `st start <id>`
// pushes <id> onto the agent's work stack so the Stop hook attributes per-turn
// metrics by default. Without this, every Stop payload orphaned (I-411).
func TestStart_AutoPushesOntoStack(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-test")
	t.Setenv("AS_SESSION_ID", "test-session-autopush")
	defer t.Setenv("AS_SESSION_ID", "")

	s, cfg := setupTestEnv(t)

	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 0 {
		t.Fatalf("Start = %d, want 0", rc)
	}

	entries := LoadStack(cfg)
	if len(entries) != 1 {
		t.Fatalf("stack depth = %d, want 1", len(entries))
	}
	if entries[0].ID != "T-001" {
		t.Errorf("stack top = %q, want T-001", entries[0].ID)
	}
}

// TestStart_NoPushFlag covers the explicit opt-out for "set up the worktree
// but I'm not actively driving this yet" cases.
func TestStart_NoPushFlag(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-test")
	t.Setenv("AS_SESSION_ID", "test-session-nopush")
	defer t.Setenv("AS_SESSION_ID", "")

	s, cfg := setupTestEnv(t)

	if rc := Start(s, cfg, "T-001", StartOpts{NoPush: true}); rc != 0 {
		t.Fatalf("Start = %d, want 0", rc)
	}

	entries := LoadStack(cfg)
	if len(entries) != 0 {
		t.Errorf("stack depth = %d, want 0 (--no-push)", len(entries))
	}
}

// TestStart_AutoPushIdempotent covers the case where the item is already on
// the stack (e.g. operator pushed it manually before running start). Start
// should not error or push a duplicate.
func TestStart_AutoPushIdempotent(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-test")
	t.Setenv("AS_SESSION_ID", "test-session-idempotent")
	defer t.Setenv("AS_SESSION_ID", "")

	s, cfg := setupTestEnv(t)

	// Pre-push manually
	if rc := StackPush(s, cfg, "T-001", StackPushOpts{Reason: "manual"}); rc != 0 {
		t.Fatalf("pre-push StackPush = %d", rc)
	}

	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 0 {
		t.Fatalf("Start = %d, want 0", rc)
	}

	entries := LoadStack(cfg)
	if len(entries) != 1 {
		t.Errorf("stack depth = %d, want 1 (no duplicate push)", len(entries))
	}
}
