package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TestStart_FreshDiskReadCatchesPeerAssignment verifies that Start() re-reads
// the item from disk after GitPull so that a peer's assigned_to written after
// the in-memory store was loaded is caught by the ownership gate (I-1435).
func TestStart_FreshDiskReadCatchesPeerAssignment(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"),
		[]byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatalf("config.yaml: %v", err)
	}

	itemPath := filepath.Join(root, "tasks", "T-001-peer.md")
	unassigned := `id: T-001
type: task
status: queued
created: 2026-05-01T10:00:00-06:00
last_touched: 2026-05-01T10:00:00-06:00

completed: null

title: Peer filter test task

sbar:
  situation: Test task for peer-filter test.
  background: Test.
  assessment: Test.
  recommendation: Test.
`
	if err := os.WriteFile(itemPath, []byte(unassigned), 0644); err != nil {
		t.Fatalf("write item: %v", err)
	}

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Load store while the item has no assigned_to (simulates stale in-memory state).
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Simulate a peer pushing assigned_to AFTER our store was loaded.
	peerAssigned := `id: T-001
type: task
status: queued
created: 2026-05-01T10:00:00-06:00
last_touched: 2026-05-01T10:00:01-06:00
assigned_to: agent-peer

completed: null

title: Peer filter test task

sbar:
  situation: Test task for peer-filter test.
  background: Test.
  assessment: Test.
  recommendation: Test.
`
	if err := os.WriteFile(itemPath, []byte(peerAssigned), 0644); err != nil {
		t.Fatalf("write peer-assigned item: %v", err)
	}

	var rc int
	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			rc = Start(s, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if rc != 1 {
		t.Errorf("Start() = %d, want 1 (peer owns item via fresh disk read)", rc)
	}
	if !strings.Contains(stderr, "agent-peer") {
		t.Errorf("stderr must name the peer agent; got:\n%s", stderr)
	}
}
