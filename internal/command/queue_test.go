package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

func TestQueueAddShow(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "Phase 1"})
	if code != 0 {
		t.Fatalf("QueueAdd returned %d", code)
	}

	code = QueueAdd(s, cfg, "T-002", QueueOpts{Reason: "Phase 2"})
	if code != 0 {
		t.Fatalf("QueueAdd T-002 returned %d", code)
	}

	// Verify persistence
	entries := LoadQueue(cfg)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ID != "T-001" || entries[1].ID != "T-002" {
		t.Errorf("order = %s, %s", entries[0].ID, entries[1].ID)
	}
	if entries[0].Reason != "Phase 1" {
		t.Errorf("reason = %q", entries[0].Reason)
	}
	if !entries[0].Approved {
		t.Error("user-added items should be approved by default")
	}

	// Show should not error
	code = QueueShow(s, cfg)
	if code != 0 {
		t.Errorf("QueueShow returned %d", code)
	}
}

func TestQueueAddDuplicate(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	code := QueueAdd(s, cfg, "T-001", QueueOpts{})
	if code != 1 {
		t.Errorf("duplicate add returned %d, want 1", code)
	}
}

func TestQueueAddNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueAdd(s, cfg, "T-999", QueueOpts{})
	if code != 1 {
		t.Errorf("not found returned %d, want 1", code)
	}
}

func TestQueueAgentAdded(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "found during work"})
	entries := LoadQueue(cfg)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].Approved {
		t.Error("agent-added items should not be auto-approved")
	}
	if entries[0].AddedBy != "agent-a" {
		t.Errorf("added_by = %q", entries[0].AddedBy)
	}
}

func TestQueueNext(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	QueueNext(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "T-001") {
		t.Errorf("next = %q, want T-001", output)
	}
}

func TestQueueNextSkipsUnapproved(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	QueueAdd(s, cfg, "T-002", QueueOpts{}) // unapproved (agent), blocked
	t.Setenv("AS_AGENT_ID", "")
	QueueAdd(s, cfg, "T-001", QueueOpts{}) // approved (user), unblocked

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	QueueNext(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "T-001") {
		t.Errorf("next = %q, want T-001 (skip unapproved T-002)", output)
	}
}

func TestQueueRm(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	code := QueueRm(cfg, "T-001")
	if code != 0 {
		t.Fatalf("QueueRm returned %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 || entries[0].ID != "T-002" {
		t.Errorf("entries after rm = %v", entries)
	}
}

func TestQueueRmNotFound(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := QueueRm(cfg, "T-999")
	if code != 1 {
		t.Errorf("rm not found returned %d, want 1", code)
	}
}

func TestQueueMove(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})
	QueueAdd(s, cfg, "T-003", QueueOpts{})

	// Move T-003 to position 1
	code := QueueMove(cfg, "T-003", 1)
	if code != 0 {
		t.Fatalf("QueueMove returned %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].ID != "T-003" {
		t.Errorf("position 1 = %s, want T-003", entries[0].ID)
	}
	if entries[1].ID != "T-001" {
		t.Errorf("position 2 = %s, want T-001", entries[1].ID)
	}
}

func TestQueueMoveNotFound(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := QueueMove(cfg, "T-999", 1)
	if code != 1 {
		t.Errorf("move not found returned %d, want 1", code)
	}
}

func TestQueueApprove(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	QueueAdd(s, cfg, "T-001", QueueOpts{})

	entries := LoadQueue(cfg)
	if entries[0].Approved {
		t.Fatal("should start unapproved")
	}

	t.Setenv("AS_AGENT_ID", "")
	code := QueueApprove(cfg, "T-001")
	if code != 0 {
		t.Fatalf("QueueApprove returned %d", code)
	}

	entries = LoadQueue(cfg)
	if !entries[0].Approved {
		t.Error("should be approved after QueueApprove")
	}
}

func TestQueueApproveNotFound(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := QueueApprove(cfg, "T-999")
	if code != 1 {
		t.Errorf("approve not found returned %d, want 1", code)
	}
}

func TestQueueShowEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueShow(s, cfg)
	if code != 0 {
		t.Errorf("QueueShow empty returned %d", code)
	}
}

func TestQueueNextEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueNext(s, cfg)
	if code != 0 {
		t.Errorf("QueueNext empty returned %d", code)
	}
}

func TestNextAction(t *testing.T) {
	s, cfg := setupPRTestEnv(t) // has active T-003 + testing config

	action := NextAction(s, cfg, "T-003")
	if action == "" {
		t.Error("expected a next action for active item")
	}
	// Active item with no test evidence → should suggest running tests
	if !strings.Contains(action, "test") {
		t.Errorf("action = %q, expected test-related suggestion", action)
	}
}

func TestNextActionNotActive(t *testing.T) {
	s, cfg := setupTestEnv(t)

	action := NextAction(s, cfg, "T-001") // queued
	if !strings.Contains(action, "start") {
		t.Errorf("action = %q, expected start suggestion", action)
	}
}

func TestQueuePruneDropsTerminalItems(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})
	QueueAdd(s, cfg, "T-003", QueueOpts{})

	// Mark T-001 as completed (terminal); T-002 + T-003 stay queued/active.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "completed"
		it.Doc.SetField("status", "completed")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	code := QueuePrune(s, cfg)
	if code != 0 {
		t.Fatalf("QueuePrune returned %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 2 {
		t.Fatalf("entries after prune = %d, want 2 (T-002, T-003)", len(entries))
	}
	for _, e := range entries {
		if e.ID == "T-001" {
			t.Error("terminal T-001 should have been pruned")
		}
	}
}

func TestQueuePruneEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueuePrune(s, cfg)
	if code != 0 {
		t.Errorf("prune on empty queue returned %d", code)
	}
}

func TestQueuePruneNoTerminalItems(t *testing.T) {
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	code := QueuePrune(s, cfg)
	if code != 0 {
		t.Fatalf("prune returned %d", code)
	}
	entries := LoadQueue(cfg)
	if len(entries) != 2 {
		t.Errorf("prune with no terminal items removed entries: %v", entries)
	}
}

func TestQueuePruneKeepsMissingItems(t *testing.T) {
	// If a queue entry references an item that no longer exists in the
	// store, prune leaves it alone — that's a data-integrity signal the
	// operator should see in `st queue show` (rendered as "not found"),
	// not a silent drop.
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-999", QueueOpts{}) // ghost ID

	// QueueAdd rejects unknown IDs. Bypass by writing queue file directly.
	entries := []QueueEntry{
		{ID: "T-001", Approved: true},
		{ID: "T-999", Approved: true},
	}
	if err := SaveQueue(cfg, entries); err != nil {
		t.Fatalf("save: %v", err)
	}

	code := QueuePrune(s, cfg)
	if code != 0 {
		t.Fatalf("prune returned %d", code)
	}
	got := LoadQueue(cfg)
	foundGhost := false
	for _, e := range got {
		if e.ID == "T-999" {
			foundGhost = true
		}
	}
	if !foundGhost {
		t.Error("prune should have kept the ghost entry T-999 (store miss ≠ terminal)")
	}
}
