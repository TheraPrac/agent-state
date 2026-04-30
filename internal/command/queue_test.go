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
	QueueNext(s, cfg, QueueNextOpts{})
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
	QueueNext(s, cfg, QueueNextOpts{})
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

func TestQueueMoveNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueMove(s, cfg, "T-999", 1)
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
	code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{})
	if code != 0 {
		t.Fatalf("QueueApprove returned %d", code)
	}

	entries = LoadQueue(cfg)
	if !entries[0].Approved {
		t.Error("should be approved after QueueApprove")
	}
}

func TestQueueApproveNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueApprove(s, cfg, "T-999", QueueApproveOpts{})
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
	s, cfg := setupTestEnv(t)

	// T-001 is queued + approved + unblocked but not in any sprint.
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	// T-003 is active and assigned to a sprint.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Sprint = "demo"
		it.Doc.SetField("sprint", "demo")
		it.Status = "queued"
		it.Doc.SetField("status", "queued")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}
	QueueAdd(s, cfg, "T-003", QueueOpts{})

	// No filter: returns first approved+unblocked → T-001.
	out := captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{}) })
	if !strings.Contains(out, "T-001") {
		t.Errorf("no-filter next = %q, want T-001", out)
	}

	// --sprint demo: skips T-001, returns T-003.
	out = captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{Sprint: "demo"}) })
	if !strings.Contains(out, "T-003") {
		t.Errorf("--sprint demo next = %q, want T-003", out)
	}

	// --sprint nonexistent: prints "no items".
	out = captureStdout(t, func() { QueueNext(s, cfg, QueueNextOpts{Sprint: "ghost"}) })
	if !strings.Contains(out, "No approved") {
		t.Errorf("--sprint ghost next = %q, want 'No approved' message", out)
	}
}

// I-488: queue approve --sprint flips every pending sprint member.
func TestQueueApproveSprintBulk(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	// Stamp T-001 + T-002 as members of "demo"; queue them as pending.
	for _, id := range []string{"T-001", "T-002"} {
		if err := s.Mutate(id, func(it *model.Item) error {
			it.Sprint = "demo"
			it.Doc.SetField("sprint", "demo")
			return nil
		}); err != nil {
			t.Fatalf("mutate %s: %v", id, err)
		}
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{}) // agent-added → pending
	QueueAdd(s, cfg, "T-002", QueueOpts{}) // agent-added → pending

	// T-003 is queued but NOT a sprint member.
	QueueAdd(s, cfg, "T-003", QueueOpts{})

	t.Setenv("AS_AGENT_ID", "")
	code := QueueApprove(s, cfg, "", QueueApproveOpts{Sprint: "demo"})
	if code != 0 {
		t.Fatalf("approve --sprint = %d", code)
	}

	entries := LoadQueue(cfg)
	by := map[string]QueueEntry{}
	for _, e := range entries {
		by[e.ID] = e
	}
	if !by["T-001"].Approved {
		t.Error("T-001 should be approved after --sprint demo")
	}
	if !by["T-002"].Approved {
		t.Error("T-002 should be approved after --sprint demo")
	}
	if by["T-003"].Approved {
		t.Error("T-003 (not in sprint) should still be pending")
	}
}

// I-488: queue approve with neither <id> nor --sprint errors.
func TestQueueApproveRequiresArgOrFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := QueueApprove(s, cfg, "", QueueApproveOpts{})
	if code != 2 {
		t.Errorf("empty approve returned %d, want 2", code)
	}
}

// I-488: queue approve with both <id> and --sprint errors.
func TestQueueApproveMutuallyExclusive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{Sprint: "demo"})
	if code != 2 {
		t.Errorf("conflicting args returned %d, want 2", code)
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
