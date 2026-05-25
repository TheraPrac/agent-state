package command

import (
	"bytes"
	"strings"
	"testing"
)

// --- GoalOrphans ---

func TestGoalOrphans_EmptyQueueReturnsEmpty(t *testing.T) {
	_, s, cfg := newGoalEnv(t)

	if got := GoalOrphans(s, cfg); len(got) != 0 {
		t.Errorf("GoalOrphans on empty queue = %v, want empty", got)
	}
}

func TestGoalOrphans_QueuedItemNotInAnyGoalIsOrphan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	orphans := GoalOrphans(s, cfg)
	if len(orphans) != 1 || orphans[0] != "T-001" {
		t.Errorf("GoalOrphans = %v, want [T-001]", orphans)
	}
}

func TestGoalOrphans_QueuedItemInActiveGoalNotOrphan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	if got := GoalOrphans(s, cfg); len(got) != 0 {
		t.Errorf("GoalOrphans = %v, want empty (item is goal-reachable)", got)
	}
}

func TestGoalOrphans_QueuedItemInDraftGoalIsOrphan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "draft", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001", Approved: false, AddedBy: "agent-a"}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	orphans := GoalOrphans(s, cfg)
	if len(orphans) != 1 || orphans[0] != "T-001" {
		t.Errorf("GoalOrphans = %v, want [T-001] (draft goal, not active)", orphans)
	}
}

func TestGoalOrphans_ApprovedOrphanStillCountsAsOrphan(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s = reloadStoreGoal(t, cfg)

	// Already-approved entry (AddedBy=user) still an orphan if not goal-reachable.
	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001", Approved: true, AddedBy: "user"}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	orphans := GoalOrphans(s, cfg)
	if len(orphans) != 1 || orphans[0] != "T-001" {
		t.Errorf("GoalOrphans = %v, want [T-001] (approved but not goal-reachable)", orphans)
	}
}

// --- GoalReview --count ---

func TestGoalReview_CountFlagPrintsOrphanCountOnly(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: false, AddedBy: "agent-a"},
		{ID: "T-002", Approved: false, AddedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	var out bytes.Buffer
	rc := GoalReview(s, cfg, GoalReviewOpts{Count: true, Out: &out})
	if rc != 0 {
		t.Fatalf("GoalReview --count rc=%d", rc)
	}
	got := strings.TrimSpace(out.String())
	if got != "2" {
		t.Errorf("--count output = %q, want \"2\"", got)
	}
}

func TestGoalReview_CountFlagZeroOrphansExitsZero(t *testing.T) {
	_, s, cfg := newGoalEnv(t)

	var out bytes.Buffer
	rc := GoalReview(s, cfg, GoalReviewOpts{Count: true, Out: &out})
	if rc != 0 {
		t.Fatalf("GoalReview --count on empty rc=%d", rc)
	}
	got := strings.TrimSpace(out.String())
	if got != "0" {
		t.Errorf("--count output = %q, want \"0\"", got)
	}
}

// --- GoalReview --list ---

func TestGoalReview_ListFlagPrintsOrphansOneIDPerLine(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: true, AddedBy: "user"},
		{ID: "T-002", Approved: true, AddedBy: "user"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	var out bytes.Buffer
	rc := GoalReview(s, cfg, GoalReviewOpts{List: true, Out: &out})
	if rc != 0 {
		t.Fatalf("GoalReview --list rc=%d", rc)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || lines[0] != "T-001" || lines[1] != "T-002" {
		t.Errorf("--list output lines = %v, want [T-001 T-002]", lines)
	}
}

// --- GoalReview health table ---

func TestGoalReview_ShowsActiveGoalHealthTable(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var out bytes.Buffer
	rc := GoalReview(s, cfg, GoalReviewOpts{
		List:  false,
		Count: false,
		Out:   &out,
		In:    strings.NewReader("q\n"),
	})
	if rc != 0 {
		t.Fatalf("GoalReview rc=%d", rc)
	}
	body := out.String()
	if !strings.Contains(body, "G-001") {
		t.Errorf("health table missing G-001; output:\n%s", body)
	}
	if !strings.Contains(body, "Active goals:") {
		t.Errorf("health table header missing; output:\n%s", body)
	}
}

func TestGoalReview_FlagsFullyDoneGoalAsMarkMetCandidate(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "done")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var out bytes.Buffer
	GoalReview(s, cfg, GoalReviewOpts{Out: &out, In: strings.NewReader("q\n")})
	body := out.String()
	if !strings.Contains(body, "candidate for st goal mark-met") {
		t.Errorf("expected mark-met annotation for 100%% goal; output:\n%s", body)
	}
}

// --- GoalReview interactive prompt ---

func TestGoalReview_InteractiveSkipLeavesOrphanAlone(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001", Approved: true, AddedBy: "user"}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	var out bytes.Buffer
	GoalReview(s, cfg, GoalReviewOpts{Out: &out, In: strings.NewReader("s\n")})

	// T-001 must not have been added to any goal.
	item, _ := s.Get("T-001")
	if item != nil && len(item.Goals) > 0 {
		t.Error("skip should leave T-001 with no goals")
	}
}

func TestGoalReview_InteractiveAddOrphanToGoal(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	// Make T-002 goal-reachable so only T-001 is an orphan.
	if rc := ItemGoalsAdd(s, cfg, "T-002", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: true, AddedBy: "user"},
		{ID: "T-002", Approved: false, AddedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	var out bytes.Buffer
	// Menu entry 1 = G-001. Input "1" picks it.
	rc := GoalReview(s, cfg, GoalReviewOpts{Out: &out, In: strings.NewReader("1\n")})
	if rc != 0 {
		t.Fatalf("GoalReview rc=%d; output:\n%s", rc, out.String())
	}

	s = reloadStoreGoal(t, cfg)
	item, _ := s.Get("T-001")
	if item == nil || len(item.Goals) == 0 || item.Goals[0] != "G-001" {
		t.Errorf("T-001 should have Goals=[G-001] after interactive add, got %v", item.Goals)
	}
}

func TestGoalReview_InteractiveQuitStopsLoop(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedTaskInGoalEnv(t, cfg, "T-003", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: true, AddedBy: "user"},
		{ID: "T-002", Approved: true, AddedBy: "user"},
		{ID: "T-003", Approved: true, AddedBy: "user"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	var out bytes.Buffer
	// "q" on first orphan — T-002 and T-003 should not be processed.
	GoalReview(s, cfg, GoalReviewOpts{Out: &out, In: strings.NewReader("q\n")})

	body := out.String()
	if !strings.Contains(body, "Stopped.") {
		t.Errorf("quit should print Stopped.; output:\n%s", body)
	}
	// T-002 and T-003 were not shown (quit on T-001).
	if strings.Contains(body, "T-002") || strings.Contains(body, "T-003") {
		t.Errorf("quit should stop after first orphan; output:\n%s", body)
	}
}

// --- GoalReview edge cases ---

func TestGoalReview_NoOrphansAndNoActiveGoalsExitsCleanly(t *testing.T) {
	_, s, cfg := newGoalEnv(t)

	var out bytes.Buffer
	rc := GoalReview(s, cfg, GoalReviewOpts{Out: &out, In: strings.NewReader("")})
	if rc != 0 {
		t.Fatalf("GoalReview rc=%d", rc)
	}
	body := out.String()
	if !strings.Contains(body, "No active goals") {
		t.Errorf("expected no-active-goals message; output:\n%s", body)
	}
}
