package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/store"
)

// TestReopenTerminalItem reopens the done T-004 fixture: status flips to active,
// the file moves from archive/ back to tasks/, and an Op="reopen" changelog
// entry is written.
func TestReopenTerminalItem(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Precondition: T-004 is terminal (done) and lives in archive/.
	pre, ok := s.Get("T-004")
	if !ok || pre.Status != "done" {
		t.Fatalf("precondition: T-004 status=%q, want done", pre.Status)
	}

	if rc := Reopen(s, cfg, "T-004", "regression found in follow-up"); rc != 0 {
		t.Fatalf("Reopen rc=%d, want 0", rc)
	}

	// Status is the type's active status.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	g, ok := s2.Get("T-004")
	if !ok {
		t.Fatal("T-004 not found after reopen")
	}
	if g.Status != "active" {
		t.Errorf("status = %q after reopen, want active", g.Status)
	}

	// File moved back to the active directory (tasks/), out of archive/.
	tasksPath := filepath.Join(cfg.ItemDir(), "tasks", "T-004-done.md")
	if _, err := os.Stat(tasksPath); err != nil {
		t.Errorf("expected reopened file in tasks/: %v", err)
	}
	archivePath := filepath.Join(cfg.ItemDir(), "archive", "T-004-done.md")
	if _, err := os.Stat(archivePath); err == nil {
		t.Errorf("file should no longer be in archive/ after reopen")
	}

	// Changelog records the reopen.
	logPath := filepath.Join(cfg.ChangelogDir(), "T-004.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading changelog: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"op":"reopen"`) {
		t.Errorf("changelog missing reopen op:\n%s", body)
	}
	if !strings.Contains(body, "regression found in follow-up") {
		t.Errorf("changelog missing reason:\n%s", body)
	}
}

// TestReopenAlreadyActive errors when the target is not terminal.
func TestReopenAlreadyActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-003 is active (non-terminal).
	if rc := Reopen(s, cfg, "T-003", "should fail"); rc == 0 {
		t.Error("Reopen on an already-active item must fail")
	}
}

// TestReopenRequiresReason errors when --reason is empty.
func TestReopenRequiresReason(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := Reopen(s, cfg, "T-004", ""); rc != 2 {
		t.Errorf("Reopen without --reason rc=%d, want 2", rc)
	}
	// Status must be unchanged (still done) after the refused call.
	g, _ := s.Get("T-004")
	if g.Status != "done" {
		t.Errorf("T-004 status = %q after refused reopen, want done", g.Status)
	}
}

// TestReopenNotFound errors for a missing id.
func TestReopenNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := Reopen(s, cfg, "T-999", "x"); rc != 1 {
		t.Errorf("Reopen on missing id rc=%d, want 1", rc)
	}
}

// TestReopenClearsCompletionMarkers closes an item (stamping completion
// markers) then reopens it, asserting the close-time stamps are cleared so the
// reopened item is not double-counted as completed.
func TestReopenClearsCompletionMarkers(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Close T-001 (queued→done) with a reason so `resolution` is also stamped.
	if rc := Close(s, cfg, "T-001", "done", CloseOpts{Force: true, Reason: "superseded"}); rc != 0 {
		t.Fatalf("Close rc=%d, want 0", rc)
	}
	// Precondition: completed_at and resolution are present post-close.
	g, _ := s.Get("T-001")
	if v, ok := g.Doc.GetNestedField("time_tracking.completed_at"); !ok || v == "" {
		t.Fatalf("precondition: time_tracking.completed_at not stamped by close")
	}
	if v, _ := g.Doc.GetField("resolution"); v == "" {
		t.Fatalf("precondition: resolution not stamped by close")
	}

	if rc := Reopen(s, cfg, "T-001", "regression"); rc != 0 {
		t.Fatalf("Reopen rc=%d, want 0", rc)
	}

	// Reload from disk and assert the markers were cleared.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	g2, _ := s2.Get("T-001")
	if v, ok := g2.Doc.GetNestedField("time_tracking.completed_at"); ok && v != "" {
		t.Errorf("time_tracking.completed_at = %q after reopen, want cleared", v)
	}
	if v, _ := g2.Doc.GetField("resolution"); v != "null" && v != "" {
		t.Errorf("resolution = %q after reopen, want blanked", v)
	}
	if g2.Status != "active" {
		t.Errorf("status = %q after reopen, want active", g2.Status)
	}
}

// TestReopenRejectsGoal proves the type guard: a terminal goal cannot be
// reopened (goals use their own lifecycle).
func TestReopenRejectsGoal(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "met", 40) // terminal goal in archive/
	s := reloadStoreGoal(t, cfg)

	if rc := Reopen(s, cfg, "G-001", "want it back"); rc == 0 {
		t.Error("Reopen of a goal must be rejected (task/issue only)")
	}
	// Goal must remain terminal.
	g, _ := s.Get("G-001")
	if g.Status != "met" {
		t.Errorf("G-001 status = %q after rejected reopen, want met", g.Status)
	}
}

// TestReopenWorksForIssue confirms reopen still works for an issue item (the
// task path is covered by TestReopenTerminalItem).
func TestReopenWorksForIssue(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// I-001 is queued; close it to done first.
	if rc := Close(s, cfg, "I-001", "done", CloseOpts{Force: true}); rc != 0 {
		t.Fatalf("Close I-001 rc=%d, want 0", rc)
	}
	if rc := Reopen(s, cfg, "I-001", "needs more work"); rc != 0 {
		t.Fatalf("Reopen I-001 rc=%d, want 0", rc)
	}
	s2, _ := store.New(cfg)
	g, _ := s2.Get("I-001")
	if g.Status != "active" {
		t.Errorf("I-001 status = %q after reopen, want active", g.Status)
	}
}
