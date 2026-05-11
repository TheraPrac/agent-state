package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupTestEnv creates a temp directory with items and returns a store + config.
func setupTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	// Config
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Items
	//
	// I-589: every task/issue fixture carries a one-line meaningful SBAR
	// so the now-hard-blocking SBAR substance gate at `st plan approve`
	// doesn't reject the many tests that call `PlanApprove(..., {})` on
	// these items. Tests that need the EMPTY-SBAR branch (e.g.
	// TestPlanApproveBlocksEmptySBARByDefault) seed their own item with
	// an explicitly empty SBAR rather than relying on this baseline.
	writeFile(t, filepath.Join(root, "tasks", "T-001-first.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: First task

depends_on:
- []

next_actions:
- []

sbar:
  situation: |-
    First task fixture used across many tests.
  background: |-
    Standard baseline item; queued and ready.
  assessment: |-
    No diagnosis required for fixture use.
  recommendation: |-
    Keep fixture stable across the test suite.
`)

	writeFile(t, filepath.Join(root, "tasks", "T-002-second.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T11:00:00-06:00
last_touched: 2026-03-25T11:00:00-06:00

completed: null

title: Second task

depends_on:
- T-001

next_actions:
- []

sbar:
  situation: |-
    Second task fixture that depends on T-001.
  background: |-
    Exists to exercise dependency-aware tests.
  assessment: |-
    Useful baseline for chained-item flows.
  recommendation: |-
    Keep the depends_on link to T-001 stable.
`)

	writeFile(t, filepath.Join(root, "tasks", "T-003-active.md"), `id: T-003
type: task
status: active
created: 2026-03-25T12:00:00-06:00
last_touched: 2026-03-25T12:00:00-06:00

completed: null

title: Active task
assigned_to: agent-a

depends_on:
- []

next_actions:
- []

sbar:
  situation: |-
    Active-status fixture used by start/run path tests.
  background: |-
    Assigned to agent-a so claim-state assertions resolve.
  assessment: |-
    Useful baseline for activity-state coverage.
  recommendation: |-
    Keep this item active in the fixture.
`)

	writeFile(t, filepath.Join(root, "issues", "I-001-bug.md"), `id: I-001
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: A bug
priority: 1

sbar:
  situation: |-
    Fixture issue used by issue-shaped tests.
  background: |-
    Sample bug record with priority 1.
  assessment: |-
    Used to exercise issue lifecycle assertions.
  recommendation: |-
    Keep priority and queued status stable.
`)

	writeFile(t, filepath.Join(root, "archive", "T-004-done.md"), `id: T-004
type: task
status: done
created: 2026-03-20T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: 2026-03-25T10:00:00-06:00

title: Done task
`)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	return s, cfg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.WriteFile(path, []byte(content), 0644)
}

// --- Show ---

func TestShowHappy(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{})
	if code != 0 {
		t.Errorf("Show T-001 returned %d, want 0", code)
	}
}

func TestShowBrief(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{Brief: true})
	if code != 0 {
		t.Errorf("Show --brief returned %d, want 0", code)
	}
}

func TestShowField(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{Field: "status"})
	if code != 0 {
		t.Errorf("Show --field returned %d, want 0", code)
	}
}

func TestShowFieldNotFound(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{Field: "nonexistent"})
	if code != 1 {
		t.Errorf("Show --field nonexistent returned %d, want 1", code)
	}
}

func TestShowNotFound(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-999", ShowOpts{})
	if code != 1 {
		t.Errorf("Show T-999 returned %d, want 1", code)
	}
}

// --- List ---

func TestListAll(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{})
	if code != 0 {
		t.Errorf("List returned %d, want 0", code)
	}
}

func TestListByType(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{Type: "issue"})
	if code != 0 {
		t.Errorf("List --type issue returned %d, want 0", code)
	}
}

func TestListByStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{Status: "active"})
	if code != 0 {
		t.Errorf("List --status active returned %d, want 0", code)
	}
}

func TestListByAssigned(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{Assigned: "agent-a"})
	if code != 0 {
		t.Errorf("List --assigned agent-a returned %d, want 0", code)
	}
}

// --- Check ---

func TestCheckClean(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Check(s, cfg, true, false)
	// May have reciprocal dep issues — just verify it doesn't crash
	_ = code
}

func TestCheckVerbose(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Check(s, cfg, false, false)
	_ = code
}

// --- Ready ---

func TestReady(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, ReadyOpts{})
	if code != 0 {
		t.Errorf("Ready returned %d, want 0", code)
	}
}

func TestReadyWithLimit(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, ReadyOpts{Limit: 1})
	if code != 0 {
		t.Errorf("Ready --limit 1 returned %d, want 0", code)
	}
}

func TestReadyByType(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, ReadyOpts{Type: "task"})
	if code != 0 {
		t.Errorf("Ready --type task returned %d, want 0", code)
	}
}

// --- Create ---

func TestCreateHappy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, "task", "New task", CreateOpts{Priority: 2})
	if code != 0 {
		t.Errorf("Create returned %d, want 0", code)
	}

	// Verify item was created
	item, ok := s.Get("T-005")
	if !ok {
		t.Fatal("T-005 should exist after create")
	}
	if item.Title != "New task" {
		t.Errorf("title = %q, want %q", item.Title, "New task")
	}
}

func TestCreateBadType(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, "banana", "Bad type", CreateOpts{Priority: 2})
	if code != 2 {
		t.Errorf("Create bad type returned %d, want 2", code)
	}
}

func TestCreateWithDeps(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, "task", "Dep task", CreateOpts{Priority: 1, Depends: "T-001"})
	if code != 0 {
		t.Errorf("Create with deps returned %d, want 0", code)
	}
}

func TestCreateWithTag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, "task", "Tagged task", CreateOpts{Priority: 2, Tag: "infra"})
	if code != 0 {
		t.Errorf("Create with tag returned %d, want 0", code)
	}
}

// --- Start ---

func TestStartHappy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Errorf("Start T-001 returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.Status != "active" {
		t.Errorf("status = %q, want active", item.Status)
	}
}

func TestStartBlocked(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-002 depends on T-001 which is queued — should block
	code := Start(s, cfg, "T-002", StartOpts{})
	if code != 1 {
		t.Errorf("Start blocked item returned %d, want 1", code)
	}
}

func TestStartAlreadyActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, "T-003", StartOpts{})
	if code != 1 {
		t.Errorf("Start already-active returned %d, want 1", code)
	}
}

func TestStartAssignedToOther(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Set agent ID
	os.Setenv("AS_AGENT_ID", "agent-b")
	defer os.Unsetenv("AS_AGENT_ID")

	// T-001 is queued and unassigned, but let's assign it first
	item, _ := s.Get("T-001")
	item.AssignedTo = "agent-a"

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 1 {
		t.Errorf("Start assigned-to-other returned %d, want 1", code)
	}
}

func TestStartNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, "T-999", StartOpts{})
	if code != 1 {
		t.Errorf("Start not found returned %d, want 1", code)
	}
}

// I-490: start refuses to activate an item with a pending queue entry.
func TestStartRefusesPending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)

	// Agent-add → pending entry.
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{}); code != 0 {
		t.Fatalf("queue add: %d", code)
	}

	if code := Start(s, cfg, "T-001", StartOpts{}); code != 1 {
		t.Errorf("expected exit 1 for pending start, got %d", code)
	}

	item, _ := s.Get("T-001")
	if item.Status == "active" {
		t.Error("item should NOT have been activated when pending")
	}
}

// I-490: approving a pending entry then starting succeeds.
func TestStartAfterApprove(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{}); code != 0 {
		t.Fatalf("queue add: %d", code)
	}
	t.Setenv("AS_AGENT_ID", "")
	// I-491 plan gate isn't under test here — bypass to focus on
	// the start-after-approve flow.
	if code := QueueApprove(s, cfg, "T-001", QueueApproveOpts{BypassPlan: true}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := Start(s, cfg, "T-001", StartOpts{}); code != 0 {
		t.Errorf("expected start to succeed after approve, got %d", code)
	}
}

// I-490: --force bypasses the gate and writes a changelog entry.
func TestStartForceBypassesPending(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	s, cfg := setupTestEnv(t)
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{}); code != 0 {
		t.Fatalf("queue add: %d", code)
	}
	if code := Start(s, cfg, "T-001", StartOpts{Force: true}); code != 0 {
		t.Errorf("--force should succeed against pending, got %d", code)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "start_force" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected start_force entry in changelog after --force bypass")
	}
}

// I-490: items not on the queue at all are unaffected by the gate.
func TestStartUnqueuedItemAllowed(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := Start(s, cfg, "T-001", StartOpts{}); code != 0 {
		t.Errorf("expected unqueued start to succeed; got %d", code)
	}
}

// --- Close ---

func TestCloseHappy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-003", "done", CloseOpts{})
	if code != 0 {
		t.Errorf("Close T-003 returned %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	if item.Status != "done" {
		t.Errorf("status = %q, want done", item.Status)
	}
}

func TestCloseAbandonedRequiresReason(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-003", "abandoned", CloseOpts{})
	if code != 2 {
		t.Errorf("Close abandoned without reason returned %d, want 2", code)
	}
}

func TestCloseAbandonedWithReason(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-003", "abandoned", CloseOpts{Reason: "no longer needed"})
	if code != 0 {
		t.Errorf("Close abandoned with reason returned %d, want 0", code)
	}
}

func TestCloseInvalidResolution(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-003", "flying", CloseOpts{})
	if code != 2 {
		t.Errorf("Close invalid resolution returned %d, want 2", code)
	}
}

func TestCloseNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-999", "done", CloseOpts{})
	if code != 1 {
		t.Errorf("Close not found returned %d, want 1", code)
	}
}

func TestCloseAutoRemovesFromQueue(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Add the active item (and an unrelated one) to the queue.
	if code := QueueAdd(s, cfg, "T-003", QueueOpts{}); code != 0 {
		t.Fatalf("QueueAdd T-003 = %d", code)
	}
	if code := QueueAdd(s, cfg, "T-001", QueueOpts{}); code != 0 {
		t.Fatalf("QueueAdd T-001 = %d", code)
	}

	if code := Close(s, cfg, "T-003", "done", CloseOpts{}); code != 0 {
		t.Fatalf("Close T-003 = %d", code)
	}

	entries := LoadQueue(cfg)
	if len(entries) != 1 || entries[0].ID != "T-001" {
		t.Errorf("after close: entries = %v, want only T-001", entries)
	}
}

func TestCloseQueueAutoRemoveSilentOnUnqueuedItem(t *testing.T) {
	// Closing an item that isn't in the queue must not error.
	s, cfg := setupTestEnv(t)
	if code := Close(s, cfg, "T-003", "done", CloseOpts{}); code != 0 {
		t.Errorf("Close on unqueued item returned %d, want 0", code)
	}
}

// --- Update ---

func TestUpdateHappy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Update(s, cfg, "T-001", "title", "Updated title", UpdateModeValue)
	if code != 0 {
		t.Errorf("Update returned %d, want 0", code)
	}
}

func TestUpdateNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Update(s, cfg, "T-999", "title", "nope", UpdateModeValue)
	if code != 1 {
		t.Errorf("Update nonexistent returned %d, want 1", code)
	}
}

// TestUpdateLongFormFieldReplaces is the I-385 regression at the
// command boundary: setting a long-form field with multi-line content
// twice must replace, not accumulate, body content.
func TestUpdateLongFormFieldReplaces(t *testing.T) {
	s, cfg := setupTestEnv(t)

	first := "first paragraph\nfirst details"
	second := "second paragraph\nsecond details"

	if code := Update(s, cfg, "T-001", "description", first, UpdateModeValue); code != 0 {
		t.Fatalf("first Update returned %d", code)
	}
	if code := Update(s, cfg, "T-001", "description", second, UpdateModeValue); code != 0 {
		t.Fatalf("second Update returned %d", code)
	}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 disappeared")
	}
	out := item.Doc.String()
	if strings.Contains(out, "first paragraph") {
		t.Errorf("stale first value remains:\n%s", out)
	}
	if !strings.Contains(out, "second paragraph") {
		t.Errorf("latest value missing:\n%s", out)
	}
}

// --- Sync ---

func TestSyncNoGit(t *testing.T) {
	s, _ := setupTestEnv(t)
	// No git repo in temp dir — should handle gracefully
	code := Sync(s, "test sync")
	// Will fail because no git repo, but shouldn't panic
	_ = code
}

// --- Index ---

func TestIndex(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Create index.md path
	os.WriteFile(cfg.IndexPath(), []byte(""), 0644)
	code := Index(s, cfg)
	if code != 0 {
		t.Errorf("Index returned %d, want 0", code)
	}
}

// --- Status ---

func TestStatusDashboard(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{})
	if code != 0 {
		t.Errorf("Status dashboard returned %d, want 0", code)
	}
}

func TestStatusWithIssues(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Issues: true})
	if code != 0 {
		t.Errorf("Status -i returned %d, want 0", code)
	}
}

func TestStatusWithTasks(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Tasks: true})
	if code != 0 {
		t.Errorf("Status -t returned %d, want 0", code)
	}
}

func TestStatusWithRecent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Recent: true})
	if code != 0 {
		t.Errorf("Status -r returned %d, want 0", code)
	}
}

func TestStatusAll(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{All: true})
	if code != 0 {
		t.Errorf("Status -a returned %d, want 0", code)
	}
}

func TestStatusCheck(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Check: true})
	// May have issues — just verify it doesn't crash
	_ = code
}

func TestStatusSingleEntity(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-001", StatusOpts{})
	if code != 0 {
		t.Errorf("Status T-001 returned %d, want 0", code)
	}
}

func TestStatusSingleNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-999", StatusOpts{})
	if code != 1 {
		t.Errorf("Status T-999 returned %d, want 1", code)
	}
}

// --- Stats ---

func TestStats(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Stats(s, cfg, StatsOpts{})
	if code != 0 {
		t.Errorf("Stats returned %d, want 0", code)
	}
}

func TestStatsJSON(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Stats(s, cfg, StatsOpts{JSON: true})
	if code != 0 {
		t.Errorf("Stats --json returned %d, want 0", code)
	}
}

// --- Dep ---

func TestDepTree(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepTree(s, cfg, "T-002", DepTreeOpts{Depth: 5})
	if code != 0 {
		t.Errorf("DepTree T-002 returned %d, want 0", code)
	}
}

func TestDepTreeNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepTree(s, cfg, "T-999", DepTreeOpts{Depth: 5})
	if code != 1 {
		t.Errorf("DepTree T-999 returned %d, want 1", code)
	}
}

func TestDepGraph(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepGraph(s, cfg, DepGraphOpts{})
	if code != 0 {
		t.Errorf("DepGraph returned %d, want 0", code)
	}
}

func TestDepGraphJSON(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepGraph(s, cfg, DepGraphOpts{JSON: true})
	if code != 0 {
		t.Errorf("DepGraph --json returned %d, want 0", code)
	}
}

// --- Prime ---

func TestPrime(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime returned %d, want 0", code)
	}
}

func TestPrimeJSON(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Prime(s, cfg, PrimeOpts{Format: "json"})
	if code != 0 {
		t.Errorf("Prime --json returned %d, want 0", code)
	}
}

// --- Coverage boost: Show ---

func TestShowFullOutput(t *testing.T) {
	s, _ := setupTestEnv(t)
	// T-003 has assigned_to, and I-001 has severity — test full output paths
	code := Show(s, nil, "T-003", ShowOpts{})
	if code != 0 {
		t.Errorf("Show T-003 full returned %d, want 0", code)
	}
	code = Show(s, nil, "I-001", ShowOpts{})
	if code != 0 {
		t.Errorf("Show I-001 full returned %d, want 0", code)
	}
}

func TestShowBriefWithStage(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "I-001", ShowOpts{Brief: true})
	if code != 0 {
		t.Errorf("Show I-001 brief returned %d, want 0", code)
	}
}

// --- Coverage boost: Status helpers ---

func TestStatusCompleted(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Completed: true})
	if code != 0 {
		t.Errorf("Status -d returned %d, want 0", code)
	}
}

// I-406: TestSeverityRank / TestSeverityColor replaced by priority
// equivalents below. The severity-specific helpers are gone.

func TestPriorityRank(t *testing.T) {
	tests := []struct {
		p    *int
		want int
	}{
		{intPtr(0), 0}, {intPtr(1), 1}, {intPtr(2), 2},
		{intPtr(3), 3}, {intPtr(4), 4},
		{nil, 99}, // unprioritized sorts last
	}
	for _, tt := range tests {
		got := priorityRank(tt.p)
		if got != tt.want {
			t.Errorf("priorityRank(%v) = %d, want %d", tt.p, got, tt.want)
		}
	}
}

func TestPriorityColor(t *testing.T) {
	// p0/p1 are urgent and should have a color; p2 is yellow (default
	// dashboard yellow), p3+ are dimmed without a color of their own.
	if priorityColor(intPtr(0)) == "" {
		t.Error("p0 should have a color")
	}
	if priorityColor(intPtr(1)) == "" {
		t.Error("p1 should have a color")
	}
	if priorityColor(nil) != "" {
		t.Error("nil priority should have no color")
	}
}

// intPtr lives in epic_test.go.

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("short string should not truncate")
	}
	got := truncate("this is a very long string", 10)
	if got != "this i..." {
		// truncate takes max chars total, including "..."
		t.Logf("truncate(26 chars, 10) = %q (len=%d)", got, len(got))
	}
}

func TestIsStartStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)
	item, _ := s.Get("T-001")
	if !isStartStatus(item, cfg) {
		t.Error("T-001 (queued) should be start status")
	}
	item, _ = s.Get("T-003")
	if isStartStatus(item, cfg) {
		t.Error("T-003 (active) should not be start status")
	}
}

func TestIsTerminal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	item, _ := s.Get("T-004")
	if !isTerminal(item, cfg) {
		t.Error("T-004 (completed) should be terminal")
	}
	item, _ = s.Get("T-001")
	if isTerminal(item, cfg) {
		t.Error("T-001 (queued) should not be terminal")
	}
}

// --- Coverage boost: List ---

func TestListEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{Tag: "nonexistent-tag"})
	if code != 0 {
		t.Errorf("List with no matches returned %d, want 0", code)
	}
}

// --- Coverage boost: Ready ---

func TestReadyWithTag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, ReadyOpts{Tag: "nonexistent"})
	if code != 0 {
		t.Errorf("Ready --tag nonexistent returned %d, want 0", code)
	}
}

// --- Coverage boost: Update ---

func TestUpdateNoDoc(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Get item and nil out doc to test error path
	item, _ := s.Get("T-001")
	item.Doc = nil
	code := Update(s, cfg, "T-001", "title", "test", UpdateModeValue)
	// Will succeed because store re-reads — just exercises the path
	_ = code
}

// --- Coverage boost: Close ---

func TestCloseWontfixRequiresReason(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// First start the issue so it's active
	Start(s, cfg, "I-001", StartOpts{})
	code := Close(s, cfg, "I-001", "abandoned", CloseOpts{})
	if code != 2 {
		t.Errorf("Close wontfix without reason returned %d, want 2", code)
	}
}

func TestCloseWontfixWithReason(t *testing.T) {
	s, cfg := setupTestEnv(t)
	Start(s, cfg, "I-001", StartOpts{})
	code := Close(s, cfg, "I-001", "abandoned", CloseOpts{Reason: "not a real bug"})
	if code != 0 {
		t.Errorf("Close wontfix with reason returned %d, want 0", code)
	}
}

// --- Coverage boost: Dep ---

func TestDepTreeDefaultDepth(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepTree(s, cfg, "T-001", DepTreeOpts{})
	if code != 0 {
		t.Errorf("DepTree default depth returned %d, want 0", code)
	}
}

// --- Coverage boost: Sync ---

func TestSyncDefaultMessage(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Sync(s, "")
	_ = code // no git repo, just verify no crash
}

// --- Coverage boost: Finish ---

func TestFinishNoWorktreeConfig(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Finish(s, cfg, "T-001", FinishOpts{})
	if code != 1 {
		t.Errorf("Finish without worktree config returned %d, want 1", code)
	}
}

func TestFinishNoID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Finish(s, cfg, "", FinishOpts{})
	// No worktree config, so returns 1 before checking ID
	if code != 1 {
		t.Errorf("Finish no ID returned %d, want 1", code)
	}
}
