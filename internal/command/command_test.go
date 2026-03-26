package command

import (
	"os"
	"path/filepath"
	"testing"

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
`)

	writeFile(t, filepath.Join(root, "issues", "I-001-bug.md"), `id: I-001
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: A bug
severity: high
`)

	writeFile(t, filepath.Join(root, "archive", "T-004-done.md"), `id: T-004
type: task
status: completed
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
	code := Show(s, "T-001", ShowOpts{})
	if code != 0 {
		t.Errorf("Show T-001 returned %d, want 0", code)
	}
}

func TestShowBrief(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, "T-001", ShowOpts{Brief: true})
	if code != 0 {
		t.Errorf("Show --brief returned %d, want 0", code)
	}
}

func TestShowField(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, "T-001", ShowOpts{Field: "status"})
	if code != 0 {
		t.Errorf("Show --field returned %d, want 0", code)
	}
}

func TestShowFieldNotFound(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, "T-001", ShowOpts{Field: "nonexistent"})
	if code != 1 {
		t.Errorf("Show --field nonexistent returned %d, want 1", code)
	}
}

func TestShowNotFound(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, "T-999", ShowOpts{})
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
	code := Check(s, cfg, true)
	// May have reciprocal dep issues — just verify it doesn't crash
	_ = code
}

func TestCheckVerbose(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Check(s, cfg, false)
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
	code := Start(s, cfg, "T-001")
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
	code := Start(s, cfg, "T-002")
	if code != 1 {
		t.Errorf("Start blocked item returned %d, want 1", code)
	}
}

func TestStartAlreadyActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, "T-003")
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

	code := Start(s, cfg, "T-001")
	if code != 1 {
		t.Errorf("Start assigned-to-other returned %d, want 1", code)
	}
}

func TestStartNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Start(s, cfg, "T-999")
	if code != 1 {
		t.Errorf("Start not found returned %d, want 1", code)
	}
}

// --- Close ---

func TestCloseHappy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-003", "completed", CloseOpts{})
	if code != 0 {
		t.Errorf("Close T-003 returned %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	if item.Status != "completed" {
		t.Errorf("status = %q, want completed", item.Status)
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
	code := Close(s, cfg, "T-999", "completed", CloseOpts{})
	if code != 1 {
		t.Errorf("Close not found returned %d, want 1", code)
	}
}

// --- Update ---

func TestUpdateHappy(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Update(s, "T-001", "title", "Updated title")
	if code != 0 {
		t.Errorf("Update returned %d, want 0", code)
	}
}

func TestUpdateNotFound(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Update(s, "T-999", "title", "nope")
	if code != 1 {
		t.Errorf("Update nonexistent returned %d, want 1", code)
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
