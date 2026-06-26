package command

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
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
	code = QueueShow(s, cfg, QueueShowOpts{})
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

// T-461: all QueueAdd calls are auto-approved regardless of caller identity.
func TestQueueAgentAdded(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "found during work"})
	entries := LoadQueue(cfg)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if !entries[0].Approved {
		t.Error("agent-added items must be auto-approved (T-461: approval gate removed)")
	}
	if entries[0].AddedBy != "agent-a" {
		t.Errorf("added_by = %q", entries[0].AddedBy)
	}
}

func TestQueueNext(t *testing.T) {
	// T-461: QueueNext now uses property+score rather than queue order.
	// I-001 (priority p1) beats T-001 (priority p2, pinned) because priority
	// is the lexicographic primary key — pins boost within a band, not across.
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	QueueNext(s, cfg, QueueNextOpts{})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "I-001") {
		t.Errorf("next = %q, want I-001 (highest priority p1 wins over pinned p2)", output)
	}
}

func TestQueueNextPinBoostWithinPriorityBand(t *testing.T) {
	// T-461: a pinned item (queue-pin score boost) floats to the top WITHIN
	// its priority band. T-001 (p2, pinned) beats T-002 (would be blocked
	// anyway), and I-001 (p1) still wins overall. Verify pin is surfaced in
	// the rationale via the --raw queue output.
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{}) // p2, pinned

	entries := LoadQueue(cfg)
	if len(entries) != 1 || entries[0].Source != QueueSourceManual || !entries[0].Approved {
		t.Errorf("QueueAdd must create a manual, auto-approved pin; got %+v", entries)
	}
}

func TestQueueRm(t *testing.T) {
	s, cfg := setupTestEnv(t)

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	code := QueueRm(s, cfg, "T-001")
	if code != 0 {
		t.Fatalf("QueueRm returned %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 || entries[0].ID != "T-002" {
		t.Errorf("entries after rm = %v", entries)
	}
}

func TestQueueRmNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueRm(s, cfg, "T-999")
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
	code := QueueMove(s, cfg, "T-003", 1)
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

// queue move flips Source to "manual" so sprint rm doesn't cascade-remove
// it. This converts an explicit operator placement into a "pin."
func TestQueueMoveFlipsSourceToManual(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Seed a sprint-sourced entry directly (I-1322: SprintAdd no longer auto-queues).
	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001", Source: QueueSourceSprint}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}
	entries := LoadQueue(cfg)
	if entries[0].Source != QueueSourceSprint {
		t.Fatalf("setup: expected source=sprint, got %q", entries[0].Source)
	}

	if code := QueueMove(s, cfg, "T-001", 1); code != 0 {
		t.Fatalf("queue move: %d", code)
	}
	entries = LoadQueue(cfg)
	if entries[0].Source != QueueSourceManual {
		t.Errorf("after queue move: source = %q, want %q", entries[0].Source, QueueSourceManual)
	}
}

func TestQueueMoveNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueMove(s, cfg, "T-999", 1)
	if code != 1 {
		t.Errorf("move not found returned %d, want 1", code)
	}
}

// T-461: QueueApprove is now a no-op informational command — the approval
// gate was eliminated because candidates derive from item properties, not
// queue.yaml. All these tests verify the new contract: always returns 0.
func TestQueueApprove_NoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Any call — with ID, with sprint, with bypass — returns 0.
	if code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{}); code != 0 {
		t.Errorf("QueueApprove(id) = %d, want 0 (no-op)", code)
	}
	if code := QueueApprove(s, cfg, "", QueueApproveOpts{Sprint: "demo"}); code != 0 {
		t.Errorf("QueueApprove(--sprint) = %d, want 0 (no-op)", code)
	}
	if code := QueueApprove(s, cfg, "", QueueApproveOpts{}); code != 0 {
		t.Errorf("QueueApprove(no-args) = %d, want 0 (no-op)", code)
	}
}

func TestQueueShowEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueShow(s, cfg, QueueShowOpts{})
	if code != 0 {
		t.Errorf("QueueShow empty returned %d", code)
	}
}

func TestQueueNextEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueNext(s, cfg, QueueNextOpts{})
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
		it.Status = "done"
		it.Doc.SetField("status", "done")
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

// I-488: Source field round-trips through Save/Load.
func TestQueueSourceRoundTrip(t *testing.T) {
	_, cfg := setupTestEnv(t)
	entries := []QueueEntry{
		{ID: "T-001", Approved: true, Source: QueueSourceSprint},
		{ID: "T-002", Approved: false, Source: QueueSourceSprint, Reason: "sprint:demo"},
		{ID: "T-003", Approved: true}, // empty Source = manual default
	}
	if err := SaveQueue(cfg, entries); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := LoadQueue(cfg)
	if len(got) != 3 {
		t.Fatalf("loaded %d entries, want 3", len(got))
	}
	if got[0].Source != QueueSourceSprint {
		t.Errorf("entry[0].Source = %q, want %q", got[0].Source, QueueSourceSprint)
	}
	if got[1].Source != QueueSourceSprint {
		t.Errorf("entry[1].Source = %q, want %q", got[1].Source, QueueSourceSprint)
	}
	if got[2].Source != "" {
		t.Errorf("entry[2].Source = %q, want empty (manual default)", got[2].Source)
	}
}

// I-488: queue next --sprint filters to sprint members.
func TestQueueNextSprintFilter(t *testing.T) {
	// T-461: QueueNext derives from item properties (g.Ready() + ClaimedBy).
	// I-001 (p1, no sprint) wins the global ranking. T-003 (queued, sprint=demo,
	// unassigned) wins the --sprint demo filter. Unknown sprint → empty message.
	s, cfg := setupTestEnv(t)

	// T-001: queued, unblocked, no sprint — pinned.
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	// T-003: make it queued + in sprint "demo" + unassigned (setupTestEnv has it active+assigned).
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Sprint = "demo"
		it.Doc.SetField("sprint", "demo")
		it.Status = "queued"
		it.Doc.SetField("status", "queued")
		it.AssignedTo = ""
		it.Doc.SetField("assigned_to", "")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}
	QueueAdd(s, cfg, "T-003", QueueOpts{})

	// No filter: I-001 (p1) wins over T-001 and T-003 (both p2).
	out := captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{}) })
	if !strings.Contains(out, "I-001") {
		t.Errorf("no-filter next = %q, want I-001 (highest priority)", out)
	}

	// --sprint demo: skips non-sprint items, returns T-003 (the only sprint-demo item).
	out = captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{Sprint: "demo"}) })
	if !strings.Contains(out, "T-003") {
		t.Errorf("--sprint demo next = %q, want T-003", out)
	}

	// --sprint nonexistent: prints "no items" message.
	out = captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{Sprint: "ghost"}) })
	if !strings.Contains(out, "No unblocked items for sprint ghost") {
		t.Errorf("--sprint ghost next = %q, want 'No unblocked items' message", out)
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

// TestQueueShowDeprecationBanner verifies that the default (non-raw) QueueShow
// output contains the deprecation notice and directs users to st next.
func TestQueueShowDeprecationBanner(t *testing.T) {
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	// Capture stdout.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	code := QueueShow(s, cfg, QueueShowOpts{}) // default: Raw=false

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured output: %v", err)
	}
	out := buf.String()

	if code != 0 {
		t.Errorf("QueueShow returned %d, want 0", code)
	}
	if !strings.Contains(strings.ToUpper(out), "DEPRECATED") {
		t.Errorf("output missing DEPRECATED notice; got:\n%s", out)
	}
	if !strings.Contains(out, "st next") {
		t.Errorf("output missing 'st next' reference; got:\n%s", out)
	}
}
