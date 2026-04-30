package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStackPushShow(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := StackPush(s, cfg, "T-001", StackPushOpts{Reason: "primary task"})
	if code != 0 {
		t.Fatalf("StackPush returned %d", code)
	}

	code = StackPush(s, cfg, "T-002", StackPushOpts{Reason: "blocks T-001"})
	if code != 0 {
		t.Fatalf("StackPush T-002 returned %d", code)
	}

	entries := LoadStack(cfg)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ID != "T-001" || entries[1].ID != "T-002" {
		t.Errorf("stack = %s, %s (want T-001 bottom, T-002 top)", entries[0].ID, entries[1].ID)
	}
	if entries[0].Reason != "primary task" {
		t.Errorf("reason = %q", entries[0].Reason)
	}

	code = StackShow(s, cfg)
	if code != 0 {
		t.Errorf("StackShow returned %d", code)
	}
}

func TestStackPushDuplicate(t *testing.T) {
	s, cfg := setupTestEnv(t)

	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	code := StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	if code != 1 {
		t.Errorf("duplicate push returned %d, want 1", code)
	}
}

func TestStackPushNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := StackPush(s, cfg, "T-999", StackPushOpts{Reason: ""})
	if code != 1 {
		t.Errorf("not found returned %d, want 1", code)
	}
}

func TestStackPop(t *testing.T) {
	s, cfg := setupTestEnv(t)

	StackPush(s, cfg, "T-001", StackPushOpts{Reason: "primary"})
	StackPush(s, cfg, "T-002", StackPushOpts{Reason: "interrupt"})

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := StackPop(s, cfg)
	w.Close()
	os.Stdout = old

	if code != 0 {
		t.Fatalf("StackPop returned %d", code)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "T-002") {
		t.Errorf("pop output = %q, want T-002 popped", output)
	}
	if !strings.Contains(output, "T-001") {
		t.Errorf("pop output = %q, want returning to T-001", output)
	}

	entries := LoadStack(cfg)
	if len(entries) != 1 || entries[0].ID != "T-001" {
		t.Errorf("stack after pop = %v", entries)
	}
}

func TestStackPopEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := StackPop(s, cfg)
	if code != 0 {
		t.Errorf("pop empty returned %d", code)
	}
}

func TestStackPopAutoSkipsResolved(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// T-004 is completed (in archive fixture)
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: "base"})
	// Manually add a resolved item to the stack
	entries := LoadStack(cfg)
	entries = append(entries, StackEntry{ID: "T-004", Reason: "was working on this"})
	entries = append(entries, StackEntry{ID: "T-002", Reason: "top"})
	SaveStack(cfg, entries)

	// Pop T-002 (top)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	StackPop(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Should pop T-002, skip T-004 (resolved), return to T-001
	if !strings.Contains(output, "T-001") {
		t.Errorf("output = %q, want returning to T-001 (skip resolved T-004)", output)
	}
}

func TestStackShowEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := StackShow(s, cfg)
	if code != 0 {
		t.Errorf("show empty returned %d", code)
	}
}

func TestStackPersistenceRoundtrip(t *testing.T) {
	_, cfg := setupTestEnv(t)

	entries := []StackEntry{
		{
			ID:       "T-001",
			Reason:   "primary task",
			PushedAt: "2026-03-27T10:00:00-06:00",
			PushedBy: "agent-a",
			Repos: map[string]StackRepoState{
				"api": {Branch: "feat/T-001-billing", LastCommit: "abc1234"},
			},
		},
		{
			ID:       "I-100",
			Reason:   "blocks T-001",
			PushedAt: "2026-03-27T11:00:00-06:00",
			PushedBy: "agent-a",
			Repos:    map[string]StackRepoState{},
		},
	}

	if err := SaveStack(cfg, entries); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}

	loaded := LoadStack(cfg)
	if len(loaded) != 2 {
		t.Fatalf("loaded = %d, want 2", len(loaded))
	}

	if loaded[0].ID != "T-001" || loaded[0].Reason != "primary task" {
		t.Errorf("entry 0 = %+v", loaded[0])
	}
	if loaded[0].PushedBy != "agent-a" {
		t.Errorf("pushed_by = %q", loaded[0].PushedBy)
	}
	if loaded[0].Repos["api"].Branch != "feat/T-001-billing" {
		t.Errorf("repo branch = %q", loaded[0].Repos["api"].Branch)
	}
	if loaded[0].Repos["api"].LastCommit != "abc1234" {
		t.Errorf("repo commit = %q", loaded[0].Repos["api"].LastCommit)
	}

	if loaded[1].ID != "I-100" || loaded[1].Reason != "blocks T-001" {
		t.Errorf("entry 1 = %+v", loaded[1])
	}
}

func TestStackPerAgent(t *testing.T) {
	_, cfg := setupTestEnv(t)

	// With agent ID — uses stacks/<agent>.yaml
	t.Setenv("AS_AGENT_ID", "agent-b")
	path := cfg.StackPath()
	if !strings.Contains(path, "stacks") || !strings.Contains(path, "agent-b") {
		t.Errorf("agent path = %q", path)
	}

	// Without agent ID — uses default stack.yaml
	t.Setenv("AS_AGENT_ID", "")
	path = cfg.StackPath()
	if !strings.HasSuffix(path, "stack.yaml") || strings.Contains(path, "stacks") {
		t.Errorf("default path = %q", path)
	}
}

func TestLoadStackLegacyFallback(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	legacyPath := filepath.Join(cfg.Root(), ".as", "stack.yaml")
	writeFile(t, legacyPath, `stack:
  - id: T-001
    reason: legacy stack entry
    pushed_at: 2026-03-27T10:00:00-06:00
    pushed_by: agent-a
`)

	entries := LoadStack(cfg)
	if len(entries) != 1 {
		t.Fatalf("legacy entries = %d, want 1", len(entries))
	}
	if entries[0].ID != "T-001" || entries[0].Reason != "legacy stack entry" {
		t.Fatalf("legacy entry = %+v", entries[0])
	}

	if code := StackPush(s, cfg, "T-002", StackPushOpts{Reason: "new per-agent entry"}); code != 0 {
		t.Fatalf("StackPush returned %d", code)
	}

	perAgentPath := filepath.Join(cfg.Root(), ".as", "stacks", "agent-a.yaml")
	if _, err := os.Stat(perAgentPath); err != nil {
		t.Fatalf("expected per-agent stack at %s: %v", perAgentPath, err)
	}

	entries = LoadStack(cfg)
	if len(entries) != 2 {
		t.Fatalf("per-agent entries = %d, want 2", len(entries))
	}
	if entries[0].ID != "T-001" || entries[1].ID != "T-002" {
		t.Errorf("per-agent stack = %+v", entries)
	}
}

// I-490: stack push refuses pending items unless --from-pending.
func TestStackPushBlocksPending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	// Pending entry (agent-added → Approved=false).
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{}); code != 0 {
		t.Fatalf("queue add: %d", code)
	}

	if code := StackPush(s, cfg, "T-001", StackPushOpts{}); code != 1 {
		t.Errorf("expected push of pending item to refuse with exit 1, got %d", code)
	}

	if code := StackPush(s, cfg, "T-001", StackPushOpts{FromPending: true}); code != 0 {
		t.Errorf("--from-pending should bypass; got %d", code)
	}
	entries := LoadStack(cfg)
	if len(entries) != 1 || entries[0].ID != "T-001" {
		t.Errorf("stack after --from-pending push = %+v", entries)
	}
}

// I-490: stack push of an item NOT on the queue is unaffected by the gate.
func TestStackPushUnqueuedItemAllowed(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := StackPush(s, cfg, "T-001", StackPushOpts{}); code != 0 {
		t.Errorf("push of unqueued item should succeed; got %d", code)
	}
}
