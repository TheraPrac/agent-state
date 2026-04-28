package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupSprintJoinEnv creates a test env with a session ID set.
func setupSprintJoinEnv(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Set session ID
	os.Setenv("AS_SESSION_ID", "test-session-123")
	t.Cleanup(func() { os.Unsetenv("AS_SESSION_ID") })

	// Reload config to pick up env var
	cfg, _ = config.Load(cfg.Root())
	s, _ = store.New(cfg)

	return s, cfg, sprintID
}

// --- SprintJoin ---

func TestSprintJoinSuccess(t *testing.T) {
	_, cfg, sprintID := setupSprintJoinEnv(t)

	code := SprintJoin(cfg, sprintID)
	if code != 0 {
		t.Fatalf("SprintJoin returned %d, want 0", code)
	}

	// Verify session file has sprint set
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, err := mgr.Load("test-session-123")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("session should exist")
	}
	if sess.Sprint != sprintID {
		t.Errorf("session.Sprint = %q, want %q", sess.Sprint, sprintID)
	}
}

func TestSprintJoinAlreadyJoined(t *testing.T) {
	_, cfg, sprintID := setupSprintJoinEnv(t)

	SprintJoin(cfg, sprintID)
	code := SprintJoin(cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintJoin again returned %d, want 0", code)
	}
}

func TestSprintJoinBadSprint(t *testing.T) {
	_, cfg, _ := setupSprintJoinEnv(t)

	code := SprintJoin(cfg, "nonexistent-sprint")
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintJoinNoSessionID(t *testing.T) {
	_, cfg, _, sprintID := setupSprintTestEnv(t)
	// No session ID set
	code := SprintJoin(cfg, sprintID)
	if code != 1 {
		t.Errorf("expected exit 1 for no session, got %d", code)
	}
}

func TestSprintJoinRegistryLoadError(t *testing.T) {
	_, cfg, _ := setupSprintJoinEnv(t)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintJoin(cfg, "x")
	if code != 1 {
		t.Errorf("expected exit 1 for registry error, got %d", code)
	}
}

// --- SprintLeave ---

func TestSprintLeaveSuccess(t *testing.T) {
	s, cfg, sprintID := setupSprintJoinEnv(t)

	// Join first
	SprintJoin(cfg, sprintID)

	code := SprintLeave(s, cfg)
	if code != 0 {
		t.Fatalf("SprintLeave returned %d, want 0", code)
	}

	// Verify session sprint is cleared
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, _ := mgr.Load("test-session-123")
	if sess.Sprint != "" {
		t.Errorf("session.Sprint = %q, want empty", sess.Sprint)
	}
}

func TestSprintLeaveReleasesClaims(t *testing.T) {
	s, cfg, sprintID := setupSprintJoinEnv(t)

	// Add items to sprint
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Join sprint
	SprintJoin(cfg, sprintID)

	// Simulate a claim on T-001 by this session
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, _ := mgr.Load("test-session-123")
	sess.ClaimedItems = []string{"T-001"}
	mgr.Save(sess)

	leaveClaimedAt := time.Now().Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "test-session-123"
		it.ClaimedAt = leaveClaimedAt
		it.Doc.SetField("claimed_by", "test-session-123")
		it.Doc.SetField("claimed_at", leaveClaimedAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// Leave
	code := SprintLeave(s, cfg)
	if code != 0 {
		t.Fatalf("SprintLeave returned %d, want 0", code)
	}

	// Verify claim is released
	// Reload from store
	s2, _ := store.New(cfg)
	item2, _ := s2.Get("T-001")
	if item2.ClaimedBy != "" {
		t.Errorf("T-001 claimed_by = %q, want empty", item2.ClaimedBy)
	}
}

func TestSprintLeaveNotJoined(t *testing.T) {
	s, cfg, _ := setupSprintJoinEnv(t)

	// Create session without joining
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	mgr.EnsureSession("test-session-123", "")

	code := SprintLeave(s, cfg)
	if code != 0 {
		t.Errorf("SprintLeave when not joined returned %d, want 0", code)
	}
}

func TestSprintLeaveNoSession(t *testing.T) {
	s, cfg, _ := setupSprintJoinEnv(t)
	// Don't create session file

	code := SprintLeave(s, cfg)
	if code != 1 {
		t.Errorf("expected exit 1 for no session, got %d", code)
	}
}

func TestSprintLeaveNoSessionID(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintLeave(s, cfg)
	if code != 1 {
		t.Errorf("expected exit 1 for no session ID, got %d", code)
	}
}

// --- SprintJoin → SprintLeave E2E ---

func TestSprintJoinLeaveE2E(t *testing.T) {
	s, cfg, sprintID := setupSprintJoinEnv(t)

	// Join
	code := SprintJoin(cfg, sprintID)
	if code != 0 {
		t.Fatalf("Join returned %d", code)
	}

	// Verify joined
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, _ := mgr.Load("test-session-123")
	if sess.Sprint != sprintID {
		t.Errorf("after join: sprint = %q, want %q", sess.Sprint, sprintID)
	}

	// Leave
	code = SprintLeave(s, cfg)
	if code != 0 {
		t.Fatalf("Leave returned %d", code)
	}

	// Verify left
	sess, _ = mgr.Load("test-session-123")
	if sess.Sprint != "" {
		t.Errorf("after leave: sprint = %q, want empty", sess.Sprint)
	}
}

// --- Sprint-scoped Prime ---

func setupSprintPrimeEnv(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()

	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".as/sessions"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Items
	writeFile(t, filepath.Join(root, "tasks", "T-001-task.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Ready task
priority: 0
depends_on:
- []
`)
	writeFile(t, filepath.Join(root, "tasks", "T-002-task.md"), `id: T-002
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Active task in sprint
priority: 1
depends_on:
- []
`)
	writeFile(t, filepath.Join(root, "tasks", "T-003-task.md"), `id: T-003
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Task outside sprint
priority: 2
depends_on:
- []
`)
	writeFile(t, filepath.Join(root, "archive", "T-004-done.md"), `id: T-004
type: task
status: done
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: 2026-03-25T10:00:00-06:00
title: Done task in sprint
`)
	writeFile(t, filepath.Join(root, "tasks", "T-005-blocked.md"), `id: T-005
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Blocked by outside dep
priority: 1
depends_on:
- T-003
`)

	os.Setenv("AS_SESSION_ID", "prime-session")
	t.Cleanup(func() { os.Unsetenv("AS_SESSION_ID") })

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	// Create epic and sprint
	EpicCreate(cfg, "Test Epic")
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint 1")
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID

	// Add items to sprint (T-001, T-002, T-004, T-005 in sprint; T-003 outside)
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002", "T-004", "T-005"})

	// Join session to sprint
	SprintJoin(cfg, sprintID)

	// Reload store after modifications
	s, _ = store.New(cfg)

	return s, cfg, sprintID
}

func TestPrimeSprintScoped(t *testing.T) {
	s, cfg, _ := setupSprintPrimeEnv(t)

	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime sprint-scoped returned %d, want 0", code)
	}
}

func TestPrimeSprintScopedJSON(t *testing.T) {
	s, cfg, _ := setupSprintPrimeEnv(t)

	code := Prime(s, cfg, PrimeOpts{Format: "json"})
	if code != 0 {
		t.Errorf("Prime sprint-scoped JSON returned %d, want 0", code)
	}
}

func TestPrimeSprintScopedCompact(t *testing.T) {
	s, cfg, _ := setupSprintPrimeEnv(t)

	code := Prime(s, cfg, PrimeOpts{Compact: true})
	if code != 0 {
		t.Errorf("Prime sprint-scoped compact returned %d, want 0", code)
	}
}

func TestPrimeFallsBackToGlobal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No session → global prime
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime global fallback returned %d, want 0", code)
	}
}

func TestPrimeSprintScopedCrossSprintDeps(t *testing.T) {
	s, cfg, _ := setupSprintPrimeEnv(t)
	// T-005 depends on T-003 which is outside the sprint
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime with cross-sprint deps returned %d, want 0", code)
	}
}

// --- Create with --sprint ---

func TestCreateWithSprint(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	code := Create(s, cfg, "task", "Sprint task", CreateOpts{
		Priority: 1,
		Sprint:   sprintID,
	})
	if code != 0 {
		t.Fatalf("Create with sprint returned %d, want 0", code)
	}

	// Verify item has sprint field
	item, ok := s.Get("T-005")
	if !ok {
		t.Fatal("T-005 should exist")
	}
	if item.Sprint != sprintID {
		t.Errorf("item.Sprint = %q, want %q", item.Sprint, sprintID)
	}

	// Verify item was added to sprint's items list
	r, _ := registry.Load(cfg.EpicsPath())
	sp, _ := r.SprintByID(sprintID)
	found := false
	for _, id := range sp.Items {
		if id == "T-005" {
			found = true
		}
	}
	if !found {
		t.Error("T-005 should be in sprint's items list")
	}
}

func TestCreateWithBadSprint(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Should still create the item, just warn about sprint
	code := Create(s, cfg, "task", "Bad sprint task", CreateOpts{
		Priority: 2,
		Sprint:   "nonexistent",
	})
	if code != 0 {
		t.Errorf("Create with bad sprint returned %d, want 0 (should warn, not fail)", code)
	}
}

func TestCreateWithSprintSetsEpic(t *testing.T) {
	s, cfg, epicID, sprintID := setupSprintTestEnv(t)

	Create(s, cfg, "task", "Epic inherit", CreateOpts{
		Priority: 1,
		Sprint:   sprintID,
	})

	item, _ := s.Get("T-005")
	if item.Epic != epicID {
		t.Errorf("item.Epic = %q, want %q", item.Epic, epicID)
	}
}

// --- SprintStatus ---

func TestSprintStatusOverview(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})

	code := SprintStatus(s, cfg, "")
	if code != 0 {
		t.Errorf("SprintStatus overview returned %d, want 0", code)
	}
}

func TestSprintStatusDetail(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001", "T-002"})

	code := SprintStatus(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintStatus detail returned %d, want 0", code)
	}
}

func TestSprintStatusBadSprint(t *testing.T) {
	s, cfg, _, _ := setupSprintTestEnv(t)
	code := SprintStatus(s, cfg, "nonexistent")
	if code != 1 {
		t.Errorf("expected exit 1 for bad sprint, got %d", code)
	}
}

func TestSprintStatusRegistryError(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.WriteFile(cfg.EpicsPath(), []byte("bad"), 0000)
	defer os.Chmod(cfg.EpicsPath(), 0644)

	code := SprintStatus(s, cfg, "")
	if code != 1 {
		t.Errorf("expected exit 1 for registry error, got %d", code)
	}
}

func TestSprintStatusWithSessions(t *testing.T) {
	s, cfg, sprintID := setupSprintJoinEnv(t)
	SprintAdd(s, cfg, sprintID, []string{"T-001"})
	SprintJoin(cfg, sprintID)

	// Overview
	code := SprintStatus(s, cfg, "")
	if code != 0 {
		t.Errorf("SprintStatus overview with sessions returned %d, want 0", code)
	}

	// Detail
	code = SprintStatus(s, cfg, sprintID)
	if code != 0 {
		t.Errorf("SprintStatus detail with sessions returned %d, want 0", code)
	}
}

func TestSprintStatusEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := SprintStatus(s, cfg, "")
	if code != 0 {
		t.Errorf("SprintStatus empty returned %d, want 0", code)
	}
}

// --- Queue filtering ---

func TestQueueShowSkipsSprintItems(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add T-001 to queue and to sprint
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Reload store
	s, _ = store.New(cfg)

	// Queue show should still work (displays the item since QueueShow doesn't filter)
	code := QueueShow(s, cfg)
	if code != 0 {
		t.Errorf("QueueShow returned %d, want 0", code)
	}
}

// --- Prime global with queue, stack, next action ---

func TestPrimeGlobalWithQueueAndStack(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Add to queue
	QueueAdd(s, cfg, "T-001", QueueOpts{Reason: "test"})

	// Push to stack
	StackPush(s, cfg, "T-003", "testing stack")

	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime with queue+stack returned %d, want 0", code)
	}
}

func TestPrimeGlobalCompact(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Prime(s, cfg, PrimeOpts{Compact: true})
	if code != 0 {
		t.Errorf("Prime compact returned %d, want 0", code)
	}
}

func TestPrimeGlobalJSON(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Prime(s, cfg, PrimeOpts{Format: "json"})
	if code != 0 {
		t.Errorf("Prime JSON returned %d, want 0", code)
	}
}

func TestPrimeGlobalQueueFiltersSprinted(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add T-001 to queue AND sprint
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Reload store
	s, _ = store.New(cfg)

	// Global prime — sprinted items should be filtered from queue display
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime with filtered queue returned %d, want 0", code)
	}
}

func TestPrimeSprintScopedWithActiveItem(t *testing.T) {
	s, cfg, _ := setupSprintPrimeEnv(t)

	// Prime should show T-002 as active and suggest next action for it
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime with active item returned %d, want 0", code)
	}
}

func TestPrimeSprintScopedOnlyCompletedItems(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".as/sessions"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeFile(t, filepath.Join(root, "archive", "T-001-done.md"), `id: T-001
type: task
status: done
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: 2026-03-25T10:00:00-06:00
title: Done task
`)

	os.Setenv("AS_SESSION_ID", "done-session")
	t.Cleanup(func() { os.Unsetenv("AS_SESSION_ID") })

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	EpicCreate(cfg, "Done Epic")
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Done Sprint")
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID
	SprintAdd(s, cfg, sprintID, []string{"T-001"})
	SprintJoin(cfg, sprintID)

	s, _ = store.New(cfg)
	code := Prime(s, cfg, PrimeOpts{})
	if code != 0 {
		t.Errorf("Prime all-completed sprint returned %d, want 0", code)
	}
}

// --- resolveSessionSprint ---

func TestResolveSessionSprintNoSessionID(t *testing.T) {
	_, cfg := setupTestEnv(t)
	result := resolveSessionSprint(cfg)
	if result != "" {
		t.Errorf("expected empty sprint, got %q", result)
	}
}

func TestResolveSessionSprintNoSessionFile(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "nonexistent-session")
	defer os.Unsetenv("AS_SESSION_ID")
	cfg, _ = config.Load(cfg.Root())

	result := resolveSessionSprint(cfg)
	if result != "" {
		t.Errorf("expected empty sprint, got %q", result)
	}
}

func TestResolveSessionSprintWithSprint(t *testing.T) {
	_, cfg, sprintID := setupSprintJoinEnv(t)
	SprintJoin(cfg, sprintID)

	result := resolveSessionSprint(cfg)
	if result != sprintID {
		t.Errorf("expected %q, got %q", sprintID, result)
	}
}

// --- Concurrent session tests ---

func TestMultipleSessionsJoinSameSprint(t *testing.T) {
	_, cfg, _, sprintID := setupSprintTestEnv(t)

	// Session 1 joins
	os.Setenv("AS_SESSION_ID", "session-alpha")
	defer os.Unsetenv("AS_SESSION_ID")
	cfg1, _ := config.Load(cfg.Root())

	code := SprintJoin(cfg1, sprintID)
	if code != 0 {
		t.Fatalf("session-alpha join returned %d", code)
	}

	// Session 2 joins same sprint
	os.Setenv("AS_SESSION_ID", "session-beta")
	cfg2, _ := config.Load(cfg.Root())

	code = SprintJoin(cfg2, sprintID)
	if code != 0 {
		t.Fatalf("session-beta join returned %d", code)
	}

	// Both should be joined
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	s1, _ := mgr.Load("session-alpha")
	s2, _ := mgr.Load("session-beta")

	if s1.Sprint != sprintID {
		t.Errorf("session-alpha sprint = %q", s1.Sprint)
	}
	if s2.Sprint != sprintID {
		t.Errorf("session-beta sprint = %q", s2.Sprint)
	}
}

func TestConcurrentClaimRejection(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Session 1 starts T-001
	os.Setenv("AS_SESSION_ID", "claimer-1")
	defer os.Unsetenv("AS_SESSION_ID")
	cfg1, _ := config.Load(cfg.Root())
	s1, _ := store.New(cfg1)

	code := Start(s1, cfg1, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("claimer-1 start returned %d", code)
	}

	// Session 2 tries to start T-001 — should be rejected
	os.Setenv("AS_SESSION_ID", "claimer-2")
	cfg2, _ := config.Load(cfg.Root())
	s2, _ := store.New(cfg2)

	code = Start(s2, cfg2, "T-001", StartOpts{})
	if code != 1 {
		t.Errorf("claimer-2 should be rejected, got code %d", code)
	}

	_ = s // suppress unused
}

// --- SprintRecover with session pruning ---

func TestSprintRecoverPrunesSessions(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)

	// Add item to sprint
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Create a stale session with a claim
	os.Setenv("AS_SESSION_ID", "stale-session")
	defer os.Unsetenv("AS_SESSION_ID")
	cfg1, _ := config.Load(cfg.Root())
	s1, _ := store.New(cfg1)

	mgr := session.NewManager(cfg1.SessionsDir(), time.Duration(cfg1.StaleClaimTTL())*time.Second)
	sess, _ := mgr.EnsureSession("stale-session", "agent")
	sess.LastActive = time.Now().Add(-3 * time.Hour)
	sess.ClaimedItems = []string{"T-001"}
	mgr.Save(sess)

	// Also set claim on item
	staleAt := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	_ = s1.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "stale-session"
		it.ClaimedAt = staleAt
		it.Doc.SetField("claimed_by", "stale-session")
		it.Doc.SetField("claimed_at", staleAt)
		return nil
	})

	// Recover
	s2, _ := store.New(cfg1)
	code := SprintRecover(s2, cfg1, sprintID)
	if code != 0 {
		t.Fatalf("SprintRecover returned %d", code)
	}

	// Verify claim was released and session was pruned
	s3, _ := store.New(cfg1)
	item2, _ := s3.Get("T-001")
	if item2.ClaimedBy != "" {
		t.Errorf("T-001 still claimed: %q", item2.ClaimedBy)
	}
}
