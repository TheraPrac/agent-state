package command

import (
	"os"
	"strings"
	"testing"
)

// --- IsGoalReachable ---

func TestIsGoalReachable_NotAGoalMember(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if IsGoalReachable(s, cfg, "T-001") {
		t.Error("T-001 not in any goal must_do — should return false")
	}
}

func TestIsGoalReachable_InActiveGoalMustDo(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if !IsGoalReachable(s, cfg, "T-001") {
		t.Error("T-001 in active goal must_do — should return true")
	}
}

func TestIsGoalReachable_InInactiveGoalMustDoReturnsFalse(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	// Seed a draft goal (not active).
	seedGoalFile(t, cfg, "G-001", "draft", 40)
	s = reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if IsGoalReachable(s, cfg, "T-001") {
		t.Error("T-001 only in draft goal must_do — should return false")
	}
}

func TestIsGoalReachable_EmptyIDReturnsFalse(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)

	if IsGoalReachable(s, cfg, "") {
		t.Error("empty id — should return false")
	}
}

// --- IsQueuePending with goal-reachability ---

func TestIsQueuePending_GoalReachableShortCircuits(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Agent-add so the entry has Approved=false.
	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	if IsQueuePending(s, cfg, "T-001") {
		t.Error("goal-reachable item should NOT be reported as pending")
	}
}

func TestIsQueuePending_NonGoalReachableStillPending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	s, cfg := setupTestEnv(t)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	if !IsQueuePending(s, cfg, "T-001") {
		t.Error("non-goal-reachable agent-added item should still be pending")
	}
}

// --- PendingApprovalCount ---

func TestPendingApprovalCount_ExcludesGoalReachable(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Both are agent-added (Approved=false), but T-001 is goal-reachable.
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})

	count := PendingApprovalCount(s, cfg)
	if count != 1 {
		t.Errorf("PendingApprovalCount = %d, want 1 (only T-002 not goal-reachable)", count)
	}
}

// --- QueueAdd auto-approve ---

func TestQueueAdd_GoalReachableAutoApproved(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].Approved {
		t.Error("goal-reachable agent-added item should be auto-approved")
	}
}

// --- upsertQueueSprintEntry auto-approve ---

func TestSprintAdd_GoalReachableEntryAutoApproved(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	added, err := upsertQueueSprintEntry(cfg, s, nil, "T-001", "S-001")
	if err != nil {
		t.Fatalf("upsertQueueSprintEntry: %v", err)
	}
	if !added {
		t.Fatal("expected entry to be added")
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].Approved {
		t.Error("goal-reachable sprint entry should be auto-approved")
	}
}

// --- Start and StackPush allow goal-reachable pending ---

func TestStartAllowsGoalReachablePending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Agent-add → would be pending without goal-reachability.
	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	// Goal-reachable: Start should succeed without --force.
	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 0 {
		t.Errorf("Start rc=%d, want 0 for goal-reachable item", rc)
	}
}

func TestStackPushAllowsGoalReachablePending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Agent-add → would be pending without goal-reachability.
	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd rc=%d", rc)
	}

	// Goal-reachable: StackPush should succeed without --from-pending.
	if rc := StackPush(s, cfg, "T-001", StackPushOpts{}); rc != 0 {
		t.Errorf("StackPush rc=%d, want 0 for goal-reachable item", rc)
	}
}

// --- QueueAutoApprove ---

func TestQueueAutoApprove_FlipsAllGoalReachable(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedTaskInGoalEnv(t, cfg, "T-003", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	// T-001 and T-002 are goal-reachable; T-003 is not.
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001", "T-002"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Manually build pending queue entries (bypass IsGoalReachable in QueueAdd
	// by writing the queue file directly).
	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: false, AddedBy: "agent-a"},
		{ID: "T-002", Approved: false, AddedBy: "agent-a"},
		{ID: "T-003", Approved: false, AddedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	rc := QueueAutoApprove(s, cfg)
	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}

	entries := LoadQueue(cfg)
	m := make(map[string]bool)
	for _, e := range entries {
		m[e.ID] = e.Approved
	}
	if !m["T-001"] {
		t.Error("T-001 (goal-reachable) should be approved after auto-approve")
	}
	if !m["T-002"] {
		t.Error("T-002 (goal-reachable) should be approved after auto-approve")
	}
	if m["T-003"] {
		t.Error("T-003 (not goal-reachable) should remain pending")
	}
}

func TestQueueAutoApprove_LeavesNonGoalReachablePending(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: false, AddedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	rc := QueueAutoApprove(s, cfg)
	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 || entries[0].Approved {
		t.Error("non-goal-reachable item should remain pending")
	}
}

func TestQueueAutoApprove_EmptyQueueIsNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := QueueAutoApprove(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}
	if !strings.Contains(out, "Queue is empty") {
		t.Errorf("expected empty-queue message, got %q", out)
	}
}

// --- QueueAutoApprove orphan banner (T-413) ---

func TestQueueAutoApprove_PrintsOrphanBannerWhenOrphansExist(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	// T-001 is goal-reachable; T-002 is an orphan.
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: false, AddedBy: "agent-a"},
		{ID: "T-002", Approved: true, AddedBy: "user"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := QueueAutoApprove(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}
	if !strings.Contains(out, "orphan") {
		t.Errorf("expected orphan banner in output, got %q", out)
	}
	if !strings.Contains(out, "st goal review") {
		t.Errorf("expected 'st goal review' pointer in orphan banner, got %q", out)
	}
}

func TestQueueAutoApprove_NoOrphanBannerWhenZeroOrphans(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")

	_, s, cfg := newGoalEnv(t)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "b1", []string{"T-001"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	// Only one entry; it is goal-reachable so it will be flipped and no orphan remains.
	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: false, AddedBy: "agent-a"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := QueueAutoApprove(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}
	if strings.Contains(out, "orphan") {
		t.Errorf("no orphan banner expected when zero orphans, got %q", out)
	}
}

func TestQueueAutoApprove_NoPendingItemsIsNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// All entries already approved.
	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001", Approved: true, AddedBy: "user"},
	}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := QueueAutoApprove(s, cfg)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if rc != 0 {
		t.Fatalf("QueueAutoApprove rc=%d, want 0", rc)
	}
	if !strings.Contains(out, "nothing to auto-approve") {
		t.Errorf("expected no-op message, got %q", out)
	}
}
