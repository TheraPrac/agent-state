package command

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpdateFieldExitsNonZeroOnGateRefusal verifies that Update returns non-zero
// when the I-807 gate fires, AND that the disk mutation is preserved (mutation
// correctness is independent of git-sync outcome).
func TestUpdateFieldExitsNonZeroOnGateRefusal(t *testing.T) {
	workspace, s, cfg := setupGateWorkspace(t)

	// Dirty the tracked non-state file to arm the gate.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho gate-armed\n"), 0755); err != nil {
		t.Fatal(err)
	}

	code := Update(s, cfg, "T-001", "title", "Gate test updated title", UpdateModeValue)

	if code == 0 {
		t.Errorf("Update must return non-zero when the I-807 gate fires; got 0")
	}

	// The disk mutation must be preserved despite the gate refusal.
	// Check via Doc.GetField since item.Title is set at parse time, not from
	// Doc mutations.
	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after Update")
	}
	if item.Doc == nil {
		t.Fatal("T-001 has no Doc after Update")
	}
	got, _ := item.Doc.GetField("title")
	if got != "Gate test updated title" {
		t.Errorf("disk mutation must be preserved on gate refusal; title = %q, want %q",
			got, "Gate test updated title")
	}
}
