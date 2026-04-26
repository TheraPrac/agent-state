package command

import (
	"os"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
)

// --- Start: claim lifecycle ---

func TestStartSetsClaim(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "test-session-001")
	defer os.Unsetenv("AS_SESSION_ID")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "test-session-001" {
		t.Errorf("ClaimedBy = %q, want %q", item.ClaimedBy, "test-session-001")
	}
	if item.ClaimedAt == "" {
		t.Error("ClaimedAt should be set")
	}

	// Session file should exist with the claim
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, err := mgr.Load("test-session-001")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("session file should exist")
	}
	if len(sess.ClaimedItems) != 1 || sess.ClaimedItems[0] != "T-001" {
		t.Errorf("session claimed items = %v", sess.ClaimedItems)
	}
}

func TestStartRejectsClaimedItem(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Create a live session that claims T-001
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	mgr.EnsureSession("other-session", "other-agent")
	mgr.AddClaim("other-session", "T-001")

	// Set claimed_by on the item
	claimedAt := time.Now().Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "other-session"
		it.ClaimedAt = claimedAt
		it.Doc.SetField("claimed_by", "other-session")
		it.Doc.SetField("claimed_at", claimedAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// Try to start with a different session
	os.Setenv("AS_SESSION_ID", "my-session")
	defer os.Unsetenv("AS_SESSION_ID")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 1 {
		t.Errorf("Start claimed item returned %d, want 1", code)
	}
}

func TestStartTakesOverStaleClaim(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Create a stale session
	mgr := session.NewManager(cfg.SessionsDir(), 1*time.Second)
	mgr.EnsureSession("stale-session", "old-agent")
	staleSession, _ := mgr.Load("stale-session")
	staleSession.LastActive = time.Now().Add(-3 * time.Hour)
	staleSession.ClaimedItems = []string{"T-001"}
	mgr.Save(staleSession)

	// Set claimed_by on the item
	staleClaimedAt := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "stale-session"
		it.ClaimedAt = staleClaimedAt
		it.Doc.SetField("claimed_by", "stale-session")
		it.Doc.SetField("claimed_at", staleClaimedAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// Start with a new session — should take over
	os.Setenv("AS_SESSION_ID", "new-session")
	defer os.Unsetenv("AS_SESSION_ID")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start over stale claim returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "new-session" {
		t.Errorf("ClaimedBy = %q, want %q", item.ClaimedBy, "new-session")
	}
}

func TestStartAllowsSameSession(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Create session and set claim
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	mgr.EnsureSession("my-session", "agent")
	mgr.AddClaim("my-session", "T-001")

	myClaimedAt := time.Now().Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "my-session"
		it.ClaimedAt = myClaimedAt
		it.Doc.SetField("claimed_by", "my-session")
		it.Doc.SetField("claimed_at", myClaimedAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// Same session should be able to start
	os.Setenv("AS_SESSION_ID", "my-session")
	defer os.Unsetenv("AS_SESSION_ID")

	code := Start(s, cfg, "T-001", StartOpts{})
	// T-001 is queued, so this should succeed
	if code != 0 {
		t.Errorf("Start by same session returned %d, want 0", code)
	}
}

func TestStartWithoutSessionID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Unsetenv("AS_SESSION_ID")

	code := Start(s, cfg, "T-001", StartOpts{})
	if code != 0 {
		t.Fatalf("Start without session ID returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "" {
		t.Errorf("ClaimedBy should be empty without session ID, got %q", item.ClaimedBy)
	}
}

// --- Close: clears claim ---

func TestCloseClearsClaim(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "test-session-close")
	defer os.Unsetenv("AS_SESSION_ID")

	// Start T-001 (sets claim)
	Start(s, cfg, "T-001", StartOpts{})

	// Close it
	code := Close(s, cfg, "T-001", "completed", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "" {
		t.Errorf("ClaimedBy should be cleared after close, got %q", item.ClaimedBy)
	}
	if item.ClaimedAt != "" {
		t.Errorf("ClaimedAt should be cleared after close, got %q", item.ClaimedAt)
	}

	// Session should no longer have the claim
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	sess, _ := mgr.Load("test-session-close")
	if sess != nil {
		for _, id := range sess.ClaimedItems {
			if id == "T-001" {
				t.Error("session should not have T-001 in claimed items after close")
			}
		}
	}
}

// --- Release: clears claim ---

func TestReleaseClearsClaim(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "test-session-release")
	defer os.Unsetenv("AS_SESSION_ID")

	Start(s, cfg, "T-001", StartOpts{})

	code := Release(s, cfg, "T-001")
	if code != 0 {
		t.Fatalf("Release returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "" {
		t.Errorf("ClaimedBy should be cleared, got %q", item.ClaimedBy)
	}
	if item.AssignedTo != "" {
		t.Errorf("AssignedTo should be cleared, got %q", item.AssignedTo)
	}
}

func TestReleaseNoClaimOrAssignment(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := Release(s, cfg, "T-001")
	if code != 1 {
		t.Errorf("Release on unassigned/unclaimed returned %d, want 1", code)
	}
}

func TestReleaseClaimOnly(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Set claim without agent assignment
	someClaimedAt := time.Now().Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "some-session"
		it.ClaimedAt = someClaimedAt
		it.Doc.SetField("claimed_by", "some-session")
		it.Doc.SetField("claimed_at", someClaimedAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	code := Release(s, cfg, "T-001")
	if code != 0 {
		t.Fatalf("Release claim-only returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "" {
		t.Errorf("ClaimedBy should be cleared, got %q", item.ClaimedBy)
	}
}

// --- SprintRecover ---

func TestSprintRecoverReleasesStale(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Create epic + sprint + add item
	EpicCreate(cfg, "Test Epic")
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint 1")
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Set claim on item from a stale session
	mgr := session.NewManager(cfg.SessionsDir(), 1*time.Second)
	mgr.EnsureSession("stale-sess", "agent")
	stale, _ := mgr.Load("stale-sess")
	stale.LastActive = time.Now().Add(-3 * time.Hour)
	stale.ClaimedItems = []string{"T-001"}
	mgr.Save(stale)

	staleAt := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "stale-sess"
		it.ClaimedAt = staleAt
		it.Doc.SetField("claimed_by", "stale-sess")
		it.Doc.SetField("claimed_at", staleAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	code := SprintRecover(s, cfg, sprintID)
	if code != 0 {
		t.Fatalf("SprintRecover returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if item.ClaimedBy != "" {
		t.Errorf("ClaimedBy should be cleared, got %q", item.ClaimedBy)
	}
}

func TestSprintRecoverSkipsFresh(t *testing.T) {
	s, cfg := setupTestEnv(t)

	EpicCreate(cfg, "Test Epic")
	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint 1")
	r, _ = registry.Load(cfg.EpicsPath())
	sprintID := r.Sprints[0].ID
	SprintAdd(s, cfg, sprintID, []string{"T-001"})

	// Set claim from a fresh session
	mgr := session.NewManager(cfg.SessionsDir(), 2*time.Hour)
	mgr.EnsureSession("fresh-sess", "agent")
	mgr.AddClaim("fresh-sess", "T-001")

	freshAt := time.Now().Format(time.RFC3339)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "fresh-sess"
		it.ClaimedAt = freshAt
		it.Doc.SetField("claimed_by", "fresh-sess")
		it.Doc.SetField("claimed_at", freshAt)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	code := SprintRecover(s, cfg, sprintID)
	if code != 0 {
		t.Fatalf("SprintRecover returned %d, want 0", code)
	}

	// Fresh claim should remain
	item, _ := s.Get("T-001")
	if item.ClaimedBy != "fresh-sess" {
		t.Errorf("fresh claim should remain, ClaimedBy = %q", item.ClaimedBy)
	}
}

func TestSprintRecoverNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := SprintRecover(s, cfg, "nonexistent-sprint")
	if code != 1 {
		t.Errorf("SprintRecover not found returned %d, want 1", code)
	}
}
