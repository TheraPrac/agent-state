package command

import (
	"strings"
	"testing"
)

// I-382: Status renders YOUR STACK with the per-agent stack
// (top-of-stack first, marked "← current"; parents below).
func TestStatusDashboardShowsYourStack(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	// Push two items so the section has top-of-stack + parent.
	if code := StackPush(s, cfg, "T-002", StackPushOpts{Reason: "parent"}); code != 0 {
		t.Fatalf("first push: %d", code)
	}
	if code := StackPush(s, cfg, "T-001", StackPushOpts{Reason: "current"}); code != 0 {
		t.Fatalf("second push: %d", code)
	}

	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{})
	})

	if !strings.Contains(out, "YOUR STACK") {
		t.Errorf("expected YOUR STACK header, got:\n%s", out)
	}
	if !strings.Contains(out, "agent-a") {
		t.Errorf("expected agent-a label in header, got:\n%s", out)
	}
	if !strings.Contains(out, "depth 2") {
		t.Errorf("expected depth 2 in header, got:\n%s", out)
	}
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "T-002") {
		t.Errorf("expected both ids in stack section, got:\n%s", out)
	}
	if !strings.Contains(out, "← current") {
		t.Errorf("expected ← current marker on top of stack, got:\n%s", out)
	}
}

// I-382: when the agent has no identity AND no entries, the
// section is omitted entirely (central-workspace path — would
// just be noise).
func TestStatusDashboardOmitsStackOnCentralWorkspace(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{})
	})
	if strings.Contains(out, "YOUR STACK") {
		t.Errorf("section should be omitted when no agent id and no entries, got:\n%s", out)
	}
}

// I-382: a stack entry whose underlying item file no longer
// exists (deleted between push and status) renders as
// "(not found)" instead of crashing.
func TestStatusDashboardStackItemNotFound(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	// Push an item, then write the stack file directly with a bogus
	// id so the item lookup fails. Easier than mutating the store
	// to delete an item mid-test.
	if code := StackPush(s, cfg, "T-001", StackPushOpts{}); code != 0 {
		t.Fatalf("push: %d", code)
	}
	// Re-load via store; injected id "I-DELETED" won't be in the
	// store so Get returns false → "(not found)" path.
	if err := SaveStack(cfg, []StackEntry{
		{ID: "I-DELETED", PushedAt: "2026-05-01T00:00:00Z", PushedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}

	out := captureStdout(t, func() {
		Status(s, cfg, "", StatusOpts{})
	})
	if !strings.Contains(out, "(not found)") {
		t.Errorf("expected (not found) for missing item, got:\n%s", out)
	}
	if !strings.Contains(out, "I-DELETED") {
		t.Errorf("expected the orphaned id to render, got:\n%s", out)
	}
}
