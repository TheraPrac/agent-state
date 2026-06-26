package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/store"
)

// setupSprintTestEnv creates a test environment with an epic and sprint ready.
func setupSprintTestEnv(t *testing.T) (*store.Store, *config.Config, string, string) {
	t.Helper()
	s, cfg := setupTestEnv(t)

	// Create an epic
	EpicCreate(nil, cfg, "Test Epic", EpicCreateOpts{})
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID

	// Create a sprint
	SprintCreate(cfg, epicID, "Sprint 1", SprintCreateOpts{})
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID

	return s, cfg, epicID, sprintID
}

// --- SprintAdd ---

func TestSprintAddSuccess(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	code := SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	if code != 0 {
		t.Fatalf("SprintAdd returned %d, want 0", code)
	}

	// Verify items added to sprint
	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	if len(sp.Items) != 2 {
		t.Fatalf("expected 2 items in sprint, got %d", len(sp.Items))
	}

	// Verify item's sprint field was updated
	item, _ := s.Get("T-001")
	if item.Sprint != sprintID {
		t.Errorf("T-001 sprint = %q, want %q", item.Sprint, sprintID)
	}
}

func TestSprintAddSetsEpicOnItem(t *testing.T) {
	s, cfg, epicID, sprintID := setupSprintTestEnv(t)

	code := SprintAdd(s, cfg, sprintID, []string{"T-001"})
	if code != 0 {
		t.Fatalf("SprintAdd returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.Epic != epicID {
		t.Errorf("T-001 epic = %q, want %q", item.Epic, epicID)
	}
}

func TestSprintAddDeduplicates(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	SprintAdd(s, cfg, sprintID, []string{"T-001"})
	code := SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	if code != 0 {
		t.Fatalf("SprintAdd dedup returned %d, want 0", code)
	}

	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	if len(sp.Items) != 2 {
		t.Errorf("expected 2 items after dedup, got %d", len(sp.Items))
	}
}

func TestSprintAddBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintAdd(s, cfg, "nonexistent", []string{"T-001"})
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintAddBadItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	code := SprintAdd(s, cfg, sprintID, []string{"T-999"})
	if code != 1 {
		t.Errorf("expected exit 1 for bad item, got %d", code)
	}
}

func TestSprintAddNoItems(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	code := SprintAdd(s, cfg, sprintID, []string{})
	if code != 2 {
		t.Errorf("expected exit 2 for no items, got %d", code)
	}
}

// --- SprintRm ---

func TestSprintRmSuccess(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add first
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})

	code := SprintRm(s, cfg, sprintID, "T-001")
	if code != 0 {
		t.Fatalf("SprintRm returned %d, want 0", code)
	}

	// Verify removed from sprint
	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	if len(sp.Items) != 1 {
		t.Fatalf("expected 1 item after remove, got %d", len(sp.Items))
	}

	// Verify item's sprint field was cleared
	item, _ := s.Get("T-001")
	if item.Sprint != "" {
		t.Errorf("T-001 sprint = %q, want empty", item.Sprint)
	}
}

func TestSprintRmBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintRm(s, cfg, "nonexistent", "T-001")
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintRmItemNotInSprint(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	code := SprintRm(s, cfg, sprintID, "T-001")
	if code != 1 {
		t.Errorf("expected exit 1 for item not in sprint, got %d", code)
	}
}

// --- SprintShow ---

func TestSprintShowSuccess(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})

	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow returned %d, want 0", code)
	}
}

func TestSprintShowEmpty(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow empty returned %d, want 0", code)
	}
}

func TestSprintShowBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintShow(s, cfg, "nonexistent")
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintShowWithMissingItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Manually add a nonexistent item to the sprint's items list in registry
	r, _ := registry.Load(cfg.EpicsPath())
	r.SprintAddItems(sprintID, []string{"T-999"})
	r.Save(cfg.EpicsPath())

	// Should still show without crashing
	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow with missing item returned %d, want 0", code)
	}
}

func TestSprintShowActiveItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-003"}) // T-003 is active

	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow with active item returned %d, want 0", code)
	}
}

func TestSprintShowCompletedItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-004"}) // T-004 is completed

	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow with completed item returned %d, want 0", code)
	}
}

// --- SprintPlan ---

func TestSprintPlanSuccess(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan returned %d, want 0", code)
	}
}

func TestSprintPlanEmpty(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan empty returned %d, want 0", code)
	}
}

func TestSprintPlanBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintPlan(s, cfg, "nonexistent")
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintPlanWithDeps(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	// T-002 depends on T-001 — both in sprint
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})

	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan with deps returned %d, want 0", code)
	}
}

func TestSprintPlanCrossSprintDeps(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	// T-002 depends on T-001, but only add T-002 to sprint
	SprintAdd(s, cfg, sprintID, []string{"T-002"})

	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan cross-sprint returned %d, want 0", code)
	}
}

// --- computeParallelGroups ---

func TestComputeParallelGroupsNoDeps(t *testing.T) {
	s, _ := setupTestEnv(t)
	items := []string{"T-001", "T-003"}
	intraDeps := make(map[string][]string)

	groups := computeParallelGroups(items, intraDeps, s)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0]) != 2 {
		t.Errorf("expected 2 items in group 0, got %d", len(groups[0]))
	}
}

func TestComputeParallelGroupsChain(t *testing.T) {
	s, _ := setupTestEnv(t)
	items := []string{"T-001", "T-002"}
	intraDeps := map[string][]string{
		"T-002": {"T-001"},
	}

	groups := computeParallelGroups(items, intraDeps, s)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0][0] != "T-001" {
		t.Errorf("group 0: got %v, want [T-001]", groups[0])
	}
	if groups[1][0] != "T-002" {
		t.Errorf("group 1: got %v, want [T-002]", groups[1])
	}
}

func TestComputeParallelGroupsCycle(t *testing.T) {
	s, _ := setupTestEnv(t)
	// Create a cycle: A depends on B, B depends on A (within sprint)
	items := []string{"T-001", "T-003"}
	intraDeps := map[string][]string{
		"T-001": {"T-003"},
		"T-003": {"T-001"},
	}

	groups := computeParallelGroups(items, intraDeps, s)
	// Should handle the cycle gracefully (assign to fallback group)
	if len(groups) == 0 {
		t.Fatal("expected at least 1 group")
	}
	// All items should be assigned
	totalItems := 0
	for _, g := range groups {
		totalItems += len(g)
	}
	if totalItems != 2 {
		t.Errorf("expected 2 total items, got %d", totalItems)
	}
}

func TestComputeParallelGroupsThreeLayers(t *testing.T) {
	s, _ := setupTestEnv(t)
	// T-001 has no deps, T-002 depends on T-001, T-003 depends on T-002
	items := []string{"T-001", "T-002", "T-003"}
	intraDeps := map[string][]string{
		"T-002": {"T-001"},
		"T-003": {"T-002"},
	}

	groups := computeParallelGroups(items, intraDeps, s)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
}

func TestComputeParallelGroupsEmptyItems(t *testing.T) {
	s, _ := setupTestEnv(t)
	groups := computeParallelGroups([]string{}, make(map[string][]string), s)
	if len(groups) != 1 {
		t.Fatalf("expected 1 (empty) group, got %d", len(groups))
	}
}

// --- SprintCreate with sequence ---

func TestSprintCreateSetsSequence(t *testing.T) {
	_, cfg := setupTestEnv(t)
	EpicCreate(nil, cfg, "Parent", EpicCreateOpts{})
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID

	SprintCreate(cfg, epicID, "Sprint 1", SprintCreateOpts{})
	SprintCreate(cfg, epicID, "Sprint 2", SprintCreateOpts{})

	r, _ = registry.Load(cfg.EpicsPath())
	if len(r.Sprints) != 2 {
		t.Fatalf("expected 2 sprints, got %d", len(r.Sprints))
	}
	if r.Sprints[0].Sequence != 1 {
		t.Errorf("sprint 1 sequence = %d, want 1", r.Sprints[0].Sequence)
	}
	if r.Sprints[1].Sequence != 2 {
		t.Errorf("sprint 2 sequence = %d, want 2", r.Sprints[1].Sequence)
	}
}

// --- SprintPlan with completed item ---

func TestSprintPlanCompletedItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-004"}) // completed

	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan with completed item returned %d, want 0", code)
	}
}

func TestSprintPlanActiveItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-003"}) // active

	code := SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintPlan with active item returned %d, want 0", code)
	}
}

// --- E2E: add → show → plan → rm ---

func TestSprintE2EWorkflow(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add items
	code := SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	if code != 0 {
		t.Fatalf("SprintAdd returned %d", code)
	}

	// Show
	code = SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Fatalf("SprintShow returned %d", code)
	}

	// Plan
	code = SprintPlan(s, cfg, sprintID)
	if code != 0 {
		t.Fatalf("SprintPlan returned %d", code)
	}

	// Remove
	code = SprintRm(s, cfg, sprintID, "T-001")
	if code != 0 {
		t.Fatalf("SprintRm returned %d", code)
	}

	// Verify T-001 removed, T-002 still there
	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	if len(sp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(sp.Items))
	}
	if sp.Items[0] != "T-002" {
		t.Errorf("remaining item = %q, want T-002", sp.Items[0])
	}
}

// --- Additional edge cases ---

func TestSprintAddAlreadySetSkip(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add T-001 the first time
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Reload store to get updated item
	s2, _ := store.New(cfg)

	// Add T-001 again — should skip the item update since sprint already set
	code := SprintAdd(s2, cfg, sprintID, []string{"T-001"})
	if code != 0 {
		t.Fatalf("SprintAdd skip returned %d, want 0", code)
	}
}

// I-1322: SprintAdd no longer touches the queue — operator uses st next for ordering.
func TestSprintAddDoesNotAutoQueue(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	code := SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	if code != 0 {
		t.Fatalf("SprintAdd returned %d", code)
	}

	if entries := LoadQueue(cfg); len(entries) != 0 {
		t.Fatalf("SprintAdd must not add queue entries, got %d", len(entries))
	}
}

// SprintAdd does not affect an operator-queued entry.
func TestSprintAddPreservesManualEntry(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "") // operator-equivalent: ensure QueueAdd marks Approved=true
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Operator manually queued T-001 first.
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "operator-curated"}); code != 0 {
		t.Fatalf("QueueAdd returned %d", code)
	}

	// Now sprint adds it.
	if code := SprintAdd(s, cfg, sprintID, []string{"T-001"}); code != 0 {
		t.Fatalf("SprintAdd returned %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 {
		t.Fatalf("expected 1 queue entry, got %d", len(entries))
	}
	if !entries[0].Approved {
		t.Error("operator-queued entry should stay approved after sprint add")
	}
	if entries[0].Source != "" && entries[0].Source != QueueSourceManual {
		t.Errorf("Source = %q, want empty or manual (operator-queued)", entries[0].Source)
	}
	if entries[0].Reason != "operator-curated" {
		t.Errorf("Reason should remain operator-curated, got %q", entries[0].Reason)
	}
}

// sprint rm cascade-removes sprint-sourced queue entries; manual entries stay.
func TestSprintRmCascadesSprintSourced(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add to sprint registry (I-1322: SprintAdd no longer auto-queues).
	if code := SprintAdd(s, cfg, sprintID, []string{"T-001"}); code != 0 {
		t.Fatalf("SprintAdd returned %d", code)
	}
	// Seed a sprint-sourced queue entry directly.
	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001", Source: QueueSourceSprint}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}
	if got := LoadQueue(cfg); len(got) != 1 {
		t.Fatalf("expected 1 queue entry, got %d", len(got))
	}

	if code := SprintRm(s, cfg, sprintID, "T-001"); code != 0 {
		t.Fatalf("SprintRm returned %d", code)
	}
	if got := LoadQueue(cfg); len(got) != 0 {
		t.Errorf("expected queue empty after cascade, got %d", len(got))
	}
}

func TestSprintRmLeavesManualEntry(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Operator manually queued T-001; SprintAdd no longer touches the queue.
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	if code := SprintRm(s, cfg, sprintID, "T-001"); code != 0 {
		t.Fatalf("SprintRm returned %d", code)
	}
	got := LoadQueue(cfg)
	if len(got) != 1 || got[0].ID != "T-001" {
		t.Errorf("manual-queued entry must survive sprint rm, got %v", got)
	}
}

// sprint show renders members in queue-position order.
func TestSprintShowOrdersByQueuePosition(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "") // operator mode so QueueAdd marks Approved=true
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	// Manually add items to the queue (I-1322: SprintAdd no longer auto-queues).
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	QueueAdd(s, cfg, "T-002", QueueOpts{})
	if code := QueueMove(s, cfg, "T-002", 1); code != 0 {
		t.Fatalf("QueueMove returned %d", code)
	}

	out := captureStdout(t, func() { SprintShow(s, cfg, sprintID) })
	idx1 := strings.Index(out, "T-002")
	idx2 := strings.Index(out, "T-001")
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("sprint show missing members:\n%s", out)
	}
	if idx1 > idx2 {
		t.Errorf("expected T-002 before T-001 in sprint show after queue move:\n%s", out)
	}
}

// sprint next derives from item properties (T-461: approval gate removed).
// SprintAdd sets Sprint on items; they are immediately visible once unblocked.
// T-002 depends on T-001 → T-001 leads.
func TestSprintNextHonorsApprovalAndDeps(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// No sprint members yet → no candidates match this sprint.
	out := captureStdout(t, func() { SprintNext(s, cfg, sprintID) })
	if !strings.Contains(out, "No unblocked items") {
		t.Errorf("expected 'No unblocked items' before SprintAdd, got %q", out)
	}

	// SprintAdd sets Sprint field on items; T-001 is unblocked, T-002 is blocked.
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})
	out = captureStdout(t, func() { SprintNext(s, cfg, sprintID) })
	if !strings.Contains(out, "T-001") {
		t.Errorf("expected T-001 (T-002 blocked by T-001), got %q", out)
	}
}

func TestSprintNextBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintNext(s, cfg, "ghost-sprint")
	if code != 1 {
		t.Errorf("SprintNext on missing sprint returned %d, want 1", code)
	}
}

func TestSprintAddRegistryLoadError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Make epics.yaml unreadable
	os.Chmod(cfg.EpicsPath(), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)

	code := SprintAdd(s, cfg, "x", []string{"T-001"})
	if code != 1 {
		t.Errorf("expected exit 1 for registry load error, got %d", code)
	}
}

func TestSprintRmRegistryLoadError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintRm(s, cfg, "x", "T-001")
	if code != 1 {
		t.Errorf("expected exit 1 for registry load error, got %d", code)
	}
}

func TestSprintShowRegistryLoadError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintShow(s, cfg, "x")
	if code != 1 {
		t.Errorf("expected exit 1 for registry load error, got %d", code)
	}
}

func TestSprintPlanRegistryLoadError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintPlan(s, cfg, "x")
	if code != 1 {
		t.Errorf("expected exit 1 for registry load error, got %d", code)
	}
}

func TestSprintRmWithNonexistentItem(t *testing.T) {
	// Test SprintRm when item doesn't exist in store but is in sprint
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Manually add a nonexistent item to the sprint
	r, _ := registry.Load(cfg.EpicsPath())
	r.SprintAddItems(sprintID, []string{"T-999"})
	r.Save(cfg.EpicsPath())

	// Remove the nonexistent item — should succeed even though item not in store
	code := SprintRm(s, cfg, sprintID, "T-999")
	if code != 0 {
		t.Errorf("SprintRm nonexistent item returned %d, want 0", code)
	}
}

func TestSprintAddSaveError(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Make the epics.yaml read-only to force save error
	os.Chmod(cfg.EpicsPath(), 0444)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintAdd(s, cfg, sprintID, []string{"T-001"})
	if code != 1 {
		t.Errorf("expected exit 1 for save error, got %d", code)
	}
}

func TestSprintRmSaveError(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Make the epics.yaml read-only to force save error
	os.Chmod(cfg.EpicsPath(), 0444)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintRm(s, cfg, sprintID, "T-001")
	if code != 1 {
		t.Errorf("expected exit 1 for save error, got %d", code)
	}
}

func TestSprintShowWithBlockedItem(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	// T-002 is blocked by T-001
	SprintAdd(s, cfg, sprintID, []string{"T-002"})

	code := SprintShow(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow blocked item returned %d, want 0", code)
	}
}

func TestSprintShowWithLongTitle(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeFile(t, filepath.Join(root, "tasks", "T-001-long.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: This is a very long title that should be truncated to fit the display
priority: 0
depends_on:
- []
`)

	cfg, _ := config.Load(root)
	st, _ := store.New(cfg)

	EpicCreate(nil, cfg, "Epic", EpicCreateOpts{})
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint", SprintCreateOpts{})
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID

	SprintAdd(st, cfg, sprintID, []string{"T-001"})
	code := SprintShow(st, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintShow long title returned %d, want 0", code)
	}
}

func TestSprintAddWithExistingEpicOnItem(t *testing.T) {
	// Test that if item already has an epic, we don't overwrite it
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeFile(t, filepath.Join(root, "tasks", "T-001-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Task with epic
epic: existing-epic
depends_on:
- []
`)

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	// Create epic and sprint
	EpicCreate(nil, cfg, "Different Epic", EpicCreateOpts{})
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint 1", SprintCreateOpts{})
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID

	code := SprintAdd(s, cfg, sprintID, []string{"T-001"})
	if code != 0 {
		t.Fatalf("SprintAdd returned %d", code)
	}

	// Epic should NOT be overwritten since it was already set
	item, _ := s.Get("T-001")
	if item.Epic != "existing-epic" {
		t.Errorf("epic should remain 'existing-epic', got %q", item.Epic)
	}
}
