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
