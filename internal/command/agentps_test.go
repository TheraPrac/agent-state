package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/agentps"
	"github.com/theraprac/agent-state/internal/model"
)

func TestAgentPS_RendersFleetWithLiveAndActiveItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())

	// Roster dir with one agent.
	wsDir := filepath.Join(t.TempDir(), ".as", "agent-workspaces")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const workspace = "/tmp/ws/theraprac-agent-tt"
	if err := os.WriteFile(filepath.Join(wsDir, "agent-tt.yaml"),
		[]byte("agent_id: agent-tt\npath: "+workspace+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ST_AGENT_WORKSPACES_DIR", wsDir)

	// Live registration (this process's pid ⇒ IsPIDLive true) + a fresh
	// WORKSPACE session JSONL (ground truth → LAST-UPDATE/SESSION/LIVE).
	sid := "sess-aps-1"
	writeFixtureSession(t, workspace, sid) // resolved via the roster workspace path
	if err := os.MkdirAll(cfg.AgentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentsDir(), "agent-tt.yaml"),
		[]byte("agent_id: agent-tt\nroot: agent-tt\npid: "+strconv.Itoa(os.Getpid())+
			"\nstarted: 2026-05-19T10:00:00Z\nsession_id: "+sid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An active agent-state item assigned to agent-tt.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "active"
		it.AssignedTo = "agent-tt"
		if it.Delivery == nil {
			it.Delivery = map[string]interface{}{}
		}
		it.Delivery["stage"] = "coding"
		return nil
	}); err != nil {
		t.Fatalf("Mutate T-001: %v", err)
	}

	out := captureStdout(t, func() {
		if code := AgentPS(s, cfg, AgentPSOpts{}); code != 0 {
			t.Fatalf("AgentPS exit %d, want 0", code)
		}
	})
	if !strings.Contains(out, "AGENT") || !strings.Contains(out, "LAST-UPDATE") {
		t.Errorf("missing header:\n%s", out)
	}
	row := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "agent-tt") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("no agent-tt row:\n%s", out)
	}
	for _, want := range []string{"theraprac-agent-tt", "live", "T-001 (coding)", "sess-aps"} {
		if !strings.Contains(row, want) {
			t.Errorf("agent-tt row missing %q:\n%s", want, row)
		}
	}
	// LAST-UPDATE must be populated from the fixture session's JSONL
	// mtime (transcript.NewestSessionForProjectDir on the roster
	// workspace path), not "—".
	if !strings.Contains(row, "ago") {
		t.Errorf("agent-tt LAST-UPDATE not populated from session mtime:\n%s", row)
	}

	// --json emits the joined rows pre-render.
	jout := captureStdout(t, func() {
		if code := AgentPS(s, cfg, AgentPSOpts{JSON: true}); code != 0 {
			t.Fatalf("--json exit %d", code)
		}
	})
	var rows []agentps.Row
	if err := json.Unmarshal([]byte(jout), &rows); err != nil {
		t.Fatalf("--json not valid []Row: %v\n%s", err, jout)
	}
	if len(rows) != 1 || rows[0].AgentID != "agent-tt" || rows[0].Item == nil || rows[0].Item.ID != "T-001" {
		t.Errorf("--json rows wrong: %+v", rows)
	}
}

func TestLessItemID(t *testing.T) {
	// Numeric suffix, not lexicographic: T-9 < T-10.
	if !lessItemID("T-9", "T-10") {
		t.Error("T-9 should sort before T-10 (numeric)")
	}
	if lessItemID("T-10", "T-9") {
		t.Error("T-10 must NOT sort before T-9")
	}
	if !lessItemID("I-5", "T-5") { // different prefix → prefix order
		t.Error("I-5 should sort before T-5")
	}
	// Non-conforming ids still get a strict, total, transitive order
	// (sort.Slice contract) — no panic, deterministic regardless of
	// input permutation.
	if lessItemID("weird", "T-1") == lessItemID("T-1", "weird") {
		t.Error("compare must be a strict order (asymmetric)")
	}
	for _, perm := range [][]string{
		{"T-2", "T-10", "T-1abc", "I-3", "weird"},
		{"weird", "T-1abc", "I-3", "T-10", "T-2"},
		{"I-3", "T-10", "weird", "T-2", "T-1abc"},
	} {
		cp := append([]string(nil), perm...)
		sort.Slice(cp, func(i, j int) bool { return lessItemID(cp[i], cp[j]) })
		if got := strings.Join(cp, ","); got != "I-3,T-2,T-10,T-1abc,weird" {
			t.Errorf("non-total order: perm %v sorted to %q", perm, got)
		}
	}
}

func TestAgentPS_WorkspaceFilterHonouredInJSON(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	wsDir := filepath.Join(t.TempDir(), ".as", "agent-workspaces")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"agent-a", "agent-b"} {
		if err := os.WriteFile(filepath.Join(wsDir, a+".yaml"),
			[]byte("agent_id: "+a+"\npath: /tmp/ws/theraprac-"+a+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ST_AGENT_WORKSPACES_DIR", wsDir)

	// --json + --workspace must filter (the bug: filter only applied in
	// the render path, silently no-op under --json).
	out := captureStdout(t, func() {
		if code := AgentPS(s, cfg, AgentPSOpts{Workspace: "agent-b", JSON: true}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	var rows []agentps.Row
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].AgentID != "agent-b" {
		t.Errorf("--json did not honour --workspace: %+v", rows)
	}
}

func TestAgentPS_MissingRosterIsReported(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Point at a non-existent roster dir explicitly → reported, non-zero
	// (absence surfaced, never a silent blank table).
	t.Setenv("ST_AGENT_WORKSPACES_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if code := AgentPS(s, cfg, AgentPSOpts{}); code != 1 {
		t.Errorf("missing roster exit %d, want 1", code)
	}
}
