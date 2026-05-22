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
	t.Setenv("ST_AGENT_WORKSPACES_DIR", t.TempDir()) // empty roster → fallback adds nothing (hermetic)
	// No registrations at all → reported, non-zero (not a silent
	// blank success).
	if code := Watch(cfg, WatchOpts{Once: true}); code != 1 {
		t.Errorf("no live agents exit %d, want 1", code)
	}
}

func TestWatch_OnceSnapshotCompressesLiveAgent(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	t.Setenv("ST_AGENT_WORKSPACES_DIR", t.TempDir()) // empty roster → only the registered agent appears (hermetic)
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
	// Exactly ONE compressed line for the agent, and it must show the
	// freshest activity with the tool_use+result COLLAPSED — not a
	// lone orphan tool_result (the bug a single-last-row design caused)
	// and not a firehose of every row.
	if !strings.Contains(out, "live ──") {
		t.Errorf("missing snapshot header:\n%s", out)
	}
	var agentLine string
	agentLines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[agent-w] ") {
			agentLines++
			agentLine = l
		}
	}
	if agentLines != 1 {
		t.Fatalf("want exactly 1 compressed line for agent-w, got %d:\n%s", agentLines, out)
	}
	if !strings.Contains(agentLine, "Bash: go test ./... → ok all passed") {
		t.Errorf("compressed line is not the collapsed Bash activity (W1 regression?):\n%s", agentLine)
	}
	if strings.Contains(agentLine, "orphan") {
		t.Errorf("freshest tool_result rendered as orphan — rows not rendered together:\n%s", agentLine)
	}
}

// TestWatchProgressChannel — I-775: a per-agent progress record written
// by the workspace agent-progress.sh Stop hook surfaces as a tagged row
// in a --once snapshot, tagged with the agent's own id (so CompressByAgent
// groups it inline under that agent) and prefixed with the "▸ " marker.
//
// The "re-poll-with-unchanged-updated emits nothing" property is enforced
// by the lastProgress watermark map inside poll(); not exercised here
// because --once polls once by design.
func TestWatchProgressChannel(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	t.Setenv("ST_AGENT_WORKSPACES_DIR", t.TempDir())
	sid := "sess-progress-1"
	writeFixtureSession(t, "/tmp/tp-fixture", sid)

	if err := os.MkdirAll(cfg.AgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := "agent_id: agent-p\nroot: agent-p\npid: " + strconv.Itoa(os.Getpid()) + "\nsession_id: " + sid + "\n"
	if err := os.WriteFile(filepath.Join(cfg.AgentsDir(), "agent-p.yaml"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed an agent-progress record under <root>/.as/agent-progress/ —
	// exactly the location the Stop hook writes to.
	progDir := filepath.Join(cfg.Root(), ".as", "agent-progress")
	if err := os.MkdirAll(progDir, 0o755); err != nil {
		t.Fatal(err)
	}
	progBody := "agent_id: agent-p\n" +
		"session_id: " + sid + "\n" +
		"updated: 2026-05-22T22:00:00Z\n" +
		"progress: \"wrote the agent-progress hook\"\n"
	if err := os.WriteFile(filepath.Join(progDir, "agent-p.yaml"), []byte(progBody), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := Watch(cfg, WatchOpts{Once: true}); code != 0 {
			t.Fatalf("watch --once exit %d, want 0", code)
		}
	})

	if !strings.Contains(out, "▸ wrote the agent-progress hook") {
		t.Errorf("progress row not surfaced in --once snapshot:\n%s", out)
	}
	if !strings.Contains(out, "[agent-p]") {
		t.Errorf("progress row not tagged with the agent's id (CompressByAgent grouping):\n%s", out)
	}
}
