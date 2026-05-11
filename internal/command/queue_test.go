package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
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

// I-489: queue move flips Source to "manual" so a future sprint-add
// chain walk skips this entry (and so sprint rm doesn't cascade-remove
// it). This converts an explicit operator placement into a "pin."
func TestQueueMoveFlipsSourceToManual(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	r, _ := registry.Load(cfg.EpicsPath())
	e := r.AddEpic("e")
	prio := 1
	r.Epics[indexOfEpicTest(r, e.ID)].Priority = &prio
	sp, _ := r.AddSprint(e.ID, "sp1")
	r.Save(cfg.EpicsPath())

	if code := SprintAdd(s, cfg, sp.ID, []string{"T-001"}); code != 0 {
		t.Fatalf("sprint add: %d", code)
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

func TestQueueApprove(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-a")

	QueueAdd(s, cfg, "T-001", QueueOpts{})

	entries := LoadQueue(cfg)
	if entries[0].Approved {
		t.Fatal("should start unapproved")
	}

	t.Setenv("AS_AGENT_ID", "")
	// I-491 plan gate isn't under test here — bypass to keep the
	// focus on the basic approve flow.
	code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{BypassPlan: true})
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
	// I-491 plan gate isn't under test here — bulk-bypass.
	code := QueueApprove(s, cfg, "", QueueApproveOpts{Sprint: "demo", BypassPlan: true})
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

// I-491: queue approve refuses items with no approved plan, and the
// error message points at `st prep` / `st plan approve`.
func TestQueueApproveBlocksUnplannedItem(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{}) // pending, no plan

	t.Setenv("AS_AGENT_ID", "")
	code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{})
	if code != 1 {
		t.Errorf("expected exit 1 for unplanned item, got %d", code)
	}

	entries := LoadQueue(cfg)
	if entries[0].Approved {
		t.Error("entry should remain pending when plan gate refuses")
	}
}

// I-491: with PlanApproved on the item, queue approve succeeds.
func TestQueueApproveSucceedsWithPlan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	t.Setenv("AS_AGENT_ID", "")
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("plan approve: %d", code)
	}

	if code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{}); code != 0 {
		t.Errorf("queue approve after plan approve should succeed; got %d", code)
	}
}

// I-491: --bypass-plan succeeds + writes a changelog entry.
func TestQueueApproveBypassPlanWritesChangelog(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	t.Setenv("AS_AGENT_ID", "")
	if code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{BypassPlan: true}); code != 0 {
		t.Fatalf("--bypass-plan should succeed; got %d", code)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "approve_bypass_plan" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected approve_bypass_plan changelog entry")
	}
}

// I-491: --sprint refuses bulk-approve when any sprint member lacks a
// plan; the error names the offenders so the operator can fix.
func TestQueueApproveSprintRefusesWhenAnyPlanless(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	for _, id := range []string{"T-001", "T-002"} {
		if err := s.Mutate(id, func(it *model.Item) error {
			it.Sprint = "demo"
			it.Doc.SetField("sprint", "demo")
			return nil
		}); err != nil {
			t.Fatalf("mutate %s: %v", id, err)
		}
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	t.Setenv("AS_AGENT_ID", "")
	// Approve T-001's plan but leave T-002 unplanned. Bulk-approve
	// should refuse the whole sprint, not partial-commit.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("plan approve T-001: %d", code)
	}

	if code := QueueApprove(s, cfg, "", QueueApproveOpts{Sprint: "demo"}); code != 1 {
		t.Errorf("bulk approve should refuse when any item lacks a plan; got %d", code)
	}

	entries := LoadQueue(cfg)
	for _, e := range entries {
		if e.Approved {
			t.Errorf("entry %s should remain pending after refused bulk approve", e.ID)
		}
	}
}

// I-491: --sprint with --bypass-plan approves all members and audits
// each plan-less item individually.
func TestQueueApproveSprintBypassAuditsEachItem(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	for _, id := range []string{"T-001", "T-002"} {
		if err := s.Mutate(id, func(it *model.Item) error {
			it.Sprint = "demo"
			it.Doc.SetField("sprint", "demo")
			return nil
		}); err != nil {
			t.Fatalf("mutate %s: %v", id, err)
		}
	}
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	t.Setenv("AS_AGENT_ID", "")
	if code := QueueApprove(s, cfg, "", QueueApproveOpts{Sprint: "demo", BypassPlan: true}); code != 0 {
		t.Fatalf("bulk approve --bypass-plan should succeed; got %d", code)
	}

	for _, id := range []string{"T-001", "T-002"} {
		entries, err := changelog.Read(cfg, id)
		if err != nil {
			t.Fatalf("read changelog %s: %v", id, err)
		}
		found := false
		for _, e := range entries {
			if e.Op == "approve_bypass_plan" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s should have an approve_bypass_plan entry from bulk bypass", id)
		}
	}
}

// I-489: computeSprintQueuePosition formula honors the chain.
func TestComputeSprintQueuePosition(t *testing.T) {
	s, cfg := setupTestEnv(t)
	_ = s

	r, _ := registry.Load(cfg.EpicsPath())
	a := r.AddEpic("alpha")
	b := r.AddEpic("billing")
	prio1 := 1
	prio2 := 2
	r.Epics[indexOfEpicTest(r, a.ID)].Priority = &prio1
	r.Epics[indexOfEpicTest(r, b.ID)].Priority = &prio2
	sa, _ := r.AddSprint(a.ID, "alpha-1")
	sb, _ := r.AddSprint(b.ID, "billing-1")
	r.SprintAddItems(sa.ID, []string{"T-001"})
	r.SprintAddItems(sb.ID, []string{"T-002"})
	r.Save(cfg.EpicsPath())

	posA := computeSprintQueuePosition(r, sa.ID, "T-001")
	posB := computeSprintQueuePosition(r, sb.ID, "T-002")
	if posA >= posB {
		t.Errorf("alpha (p1) item should sort before billing (p2): posA=%d posB=%d", posA, posB)
	}

	r.SprintAddItems(sa.ID, []string{"T-003"})
	pos1 := computeSprintQueuePosition(r, sa.ID, "T-001")
	pos3 := computeSprintQueuePosition(r, sa.ID, "T-003")
	if pos1 >= pos3 {
		t.Errorf("first-in-sprint should sort before second: pos1=%d pos3=%d", pos1, pos3)
	}

	c := r.AddEpic("unprio")
	sc, _ := r.AddSprint(c.ID, "unprio-1")
	r.SprintAddItems(sc.ID, []string{"T-004"})
	posC := computeSprintQueuePosition(r, sc.ID, "T-004")
	if posC <= posB {
		t.Errorf("unprioritized epic should sort after numbered: posC=%d posB=%d", posC, posB)
	}
}

// I-489: sprint add lands a high-priority epic's item ahead of a
// previously-queued lower-priority epic's item.
func TestSprintAddRespectsEpicPriority(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	r, _ := registry.Load(cfg.EpicsPath())
	hi := r.AddEpic("hi")
	lo := r.AddEpic("lo")
	prioHi := 1
	prioLo := 2
	r.Epics[indexOfEpicTest(r, hi.ID)].Priority = &prioHi
	r.Epics[indexOfEpicTest(r, lo.ID)].Priority = &prioLo
	sHi, _ := r.AddSprint(hi.ID, "hi-1")
	sLo, _ := r.AddSprint(lo.ID, "lo-1")
	r.Save(cfg.EpicsPath())

	if code := SprintAdd(s, cfg, sLo.ID, []string{"T-002"}); code != 0 {
		t.Fatalf("sprint add lo: %d", code)
	}
	if code := SprintAdd(s, cfg, sHi.ID, []string{"T-001"}); code != 0 {
		t.Fatalf("sprint add hi: %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 2 {
		t.Fatalf("expected 2 queue entries, got %d", len(entries))
	}
	if entries[0].ID != "T-001" {
		t.Errorf("queue order broken: entries[0]=%s, want T-001 (high-priority epic)", entries[0].ID)
	}
}

// I-489: findChainInsertIndex skips manual-Source entries (operator
// pinned), so a sprint-add doesn't displace operator-curated work.
func TestFindChainInsertIndexSkipsManual(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	r, _ := registry.Load(cfg.EpicsPath())
	e := r.AddEpic("e")
	prio := 1
	r.Epics[indexOfEpicTest(r, e.ID)].Priority = &prio
	sp, _ := r.AddSprint(e.ID, "sp1")
	r.Save(cfg.EpicsPath())

	if code := QueueAdd(s, cfg, "T-003", QueueOpts{Reason: "ops"}); code != 0 {
		t.Fatalf("queue add T-003: %d", code)
	}
	if code := SprintAdd(s, cfg, sp.ID, []string{"T-001"}); code != 0 {
		t.Fatalf("sprint add: %d", code)
	}

	entries := LoadQueue(cfg)
	manualIdx := -1
	for i, e := range entries {
		if e.ID == "T-003" {
			manualIdx = i
			break
		}
	}
	if manualIdx < 0 {
		t.Fatal("T-003 (manual) missing after sprint add")
	}
	if entries[manualIdx].Source == QueueSourceSprint {
		t.Error("T-003 source got rewritten — operator origin lost")
	}
}

// indexOfEpicTest finds an epic by ID for tests that need to mutate
// Priority directly without exposing helpers from registry.
func indexOfEpicTest(r *registry.Registry, id string) int {
	for i, e := range r.Epics {
		if e.ID == id {
			return i
		}
	}
	return -1
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
