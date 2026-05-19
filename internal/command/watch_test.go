package command

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWatch_NoLiveAgentsIsReported(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	// No registrations at all → reported, non-zero (not a silent
	// blank success).
	if code := Watch(cfg, WatchOpts{Once: true}); code != 1 {
		t.Errorf("no live agents exit %d, want 1", code)
	}
}

func TestWatch_OnceSnapshotCompressesLiveAgent(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	sid := "sess-watch-1"
	writeFixtureSession(t, "/tmp/tp-fixture", sid)

	if err := os.MkdirAll(cfg.AgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A registration with THIS process's pid is "live" (IsPIDLive
	// true), pointing at the fixture session.
	reg := "agent_id: agent-w\nroot: agent-w\npid: " + strconv.Itoa(os.Getpid()) + "\nsession_id: " + sid + "\n"
	if err := os.WriteFile(filepath.Join(cfg.AgentsDir(), "agent-w.yaml"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := Watch(cfg, WatchOpts{Once: true}); code != 0 {
			t.Fatalf("watch --once exit %d, want 0", code)
		}
	})
	// Exactly one compressed line for the agent, showing its freshest
	// activity (the Bash tool collapse from the fixture), not a
	// firehose of every row.
	if !strings.Contains(out, "[agent-w] ") {
		t.Errorf("no compressed line for the live agent:\n%s", out)
	}
	if !strings.Contains(out, "live ──") {
		t.Errorf("missing snapshot header:\n%s", out)
	}
	agentLines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[agent-w] ") {
			agentLines++
		}
	}
	if agentLines != 1 {
		t.Errorf("want exactly 1 compressed line for agent-w, got %d:\n%s", agentLines, out)
	}
}
