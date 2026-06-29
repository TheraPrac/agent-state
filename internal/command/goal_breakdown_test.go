package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// TestGoalBreakdownNoActiveGoals: with no active goals, prints a sentinel
// message and exits 0.
func TestGoalBreakdownNoActiveGoals(t *testing.T) {
	_, s, cfg := newGoalEnv(t)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3})
	if code != 0 {
		t.Errorf("expected exit 0 with no goals, got %d", code)
	}
	if !strings.Contains(buf.String(), "No active goals") {
		t.Errorf("expected 'No active goals' in output, got: %q", buf.String())
	}
}

// TestGoalBreakdownTopN: with two active goals and several tasks, each goal
// section shows at most --top N items.
func TestGoalBreakdownTopN(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	_, s, cfg := newGoalEnv(t)

	// Seed two active goals.
	seedGoalFile(t, cfg, "G-001", "active", 80)
	seedGoalFile(t, cfg, "G-002", "active", 20)

	// Seed four queued tasks linked to G-001.
	for _, id := range []string{"T-001", "T-002", "T-003", "T-004"} {
		seedTaskInGoalEnv(t, cfg, id, "queued")
	}
	// One task linked to G-002.
	seedTaskInGoalEnv(t, cfg, "T-005", "queued")

	s = reloadStoreGoal(t, cfg)

	// Link tasks to goals.
	if rc := ItemGoalsAdd(s, cfg, "T-001", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd T-001 rc=%d", rc)
	}
	if rc := ItemGoalsAdd(s, cfg, "T-002", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd T-002 rc=%d", rc)
	}
	if rc := ItemGoalsAdd(s, cfg, "T-003", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd T-003 rc=%d", rc)
	}
	if rc := ItemGoalsAdd(s, cfg, "T-004", []string{"G-001"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd T-004 rc=%d", rc)
	}
	if rc := ItemGoalsAdd(s, cfg, "T-005", []string{"G-002"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd T-005 rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 2})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput: %s", code, buf.String())
	}

	out := buf.String()

	// Both goals appear in the output.
	if !strings.Contains(out, "G-001") {
		t.Error("expected G-001 in output")
	}
	if !strings.Contains(out, "G-002") {
		t.Error("expected G-002 in output")
	}

	// G-001 appears before G-002 (sorted by weight descending: 80 > 20).
	g1pos := strings.Index(out, "G-001")
	g2pos := strings.Index(out, "G-002")
	if g1pos >= g2pos {
		t.Errorf("G-001 (weight 80) should appear before G-002 (weight 20)")
	}

	// At most 2 items per goal. T-001 through T-004 are in G-001 but only 2 show.
	// Count how many T-00x IDs appear between G-001 and G-002 headers.
	g1Section := out[g1pos:g2pos]
	g1Items := 0
	for _, id := range []string{"T-001", "T-002", "T-003", "T-004"} {
		if strings.Contains(g1Section, id) {
			g1Items++
		}
	}
	if g1Items > 2 {
		t.Errorf("expected at most 2 items in G-001 section (--top 2), got %d\n%s", g1Items, g1Section)
	}
	if g1Items == 0 {
		t.Errorf("expected at least 1 item in G-001 section, got 0\n%s", g1Section)
	}

	// T-005 appears in G-002 section.
	if !strings.Contains(out[g2pos:], "T-005") {
		t.Errorf("expected T-005 in G-002 section\n%s", out[g2pos:])
	}
}

// TestGoalBreakdownJSON: --json flag emits valid JSON with expected shape.
func TestGoalBreakdownJSON(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	_, s, cfg := newGoalEnv(t)

	seedGoalFile(t, cfg, "G-010", "active", 50)
	seedTaskInGoalEnv(t, cfg, "T-010", "queued")
	s = reloadStoreGoal(t, cfg)
	if rc := ItemGoalsAdd(s, cfg, "T-010", []string{"G-010"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3, JSON: true})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\noutput: %s", code, buf.String())
	}

	var out []goalBreakdownGoalJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}

	if len(out) != 1 {
		t.Fatalf("expected 1 goal in JSON, got %d", len(out))
	}
	g := out[0]
	if g.GoalID != "G-010" {
		t.Errorf("expected goal_id 'G-010', got %q", g.GoalID)
	}
	if g.Weight != 50 {
		t.Errorf("expected weight 50, got %d", g.Weight)
	}
	if len(g.Items) != 1 {
		t.Errorf("expected 1 item under G-010, got %d", len(g.Items))
	}
	if len(g.Items) > 0 && g.Items[0].ID != "T-010" {
		t.Errorf("expected item T-010, got %q", g.Items[0].ID)
	}
}

// TestGoalBreakdownEmptyGoalSection: a goal with no workable items shows
// "(no workable items)" in text mode rather than crashing or being omitted.
func TestGoalBreakdownEmptyGoalSection(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-020", "active", 30)
	// No tasks seeded — goal has no workable items.
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "G-020") {
		t.Error("expected G-020 to appear even with no items")
	}
	if !strings.Contains(out, "no workable items") {
		t.Errorf("expected 'no workable items' in output, got: %q", out)
	}
}

// TestGoalBreakdownJSONEmptyItems: in JSON mode, a goal with no workable items
// must emit items:[] rather than running the scoring pipeline on an empty slice
// (I-1657 — goalBreakdownJSON was missing the len(cands)==0 guard).
func TestGoalBreakdownJSONEmptyItems(t *testing.T) {
	_, s, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-030", "active", 40)
	// No tasks — goal has no workable items.
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3, JSON: true})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	var out []goalBreakdownGoalJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 goal in JSON, got %d", len(out))
	}
	if out[0].GoalID != "G-030" {
		t.Errorf("expected goal_id 'G-030', got %q", out[0].GoalID)
	}
	if out[0].Items == nil {
		t.Error("expected Items to be a non-nil empty slice, got nil")
	}
	if len(out[0].Items) != 0 {
		t.Errorf("expected 0 items in empty-goal JSON, got %d", len(out[0].Items))
	}
}

// seedTaskWithAssignee writes a task file with an assigned_to field, so the
// item appears in peerAssignedReady for a different agent.
func seedTaskWithAssignee(t *testing.T, cfg *config.Config, id, assignee string) {
	t.Helper()
	content := fmt.Sprintf(`id: %s
type: task
status: queued
assigned_to: %s
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: Task %s

sbar:
  situation: |-
    Fixture task.
  background: |-
    Test.
  assessment: |-
    Test.
  recommendation: |-
    Test.
`, id, assignee, id)
	slug := strings.ToLower(strings.ReplaceAll(id, "-", "")) + "-task"
	path := filepath.Join(cfg.ItemDir(), "tasks", id+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("seedTaskWithAssignee: %v", err)
	}
}

// TestGoalBreakdownEmptyWithPeerNote: when a goal has no workable items AND a
// peer-assigned item exists, the text path surfaces the peer note (I-1657).
func TestGoalBreakdownEmptyWithPeerNote(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	_, s, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-040", "active", 50)
	// Seed a task assigned to a peer — excluded from candidates but visible in peer note.
	seedTaskWithAssignee(t, cfg, "T-040", "agent-b")
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "no workable items") {
		t.Errorf("expected 'no workable items', got: %q", out)
	}
	if !strings.Contains(out, "T-040") {
		t.Errorf("expected peer item T-040 in peer note, got: %q", out)
	}
	if !strings.Contains(out, "agent-b") {
		t.Errorf("expected agent-b in peer note, got: %q", out)
	}
}

// TestGoalBreakdownJSONEmptyWithPeerNote: JSON mode surfaces peer_note field
// when goal has no workable items but peer-assigned items exist (I-1657).
func TestGoalBreakdownJSONEmptyWithPeerNote(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	_, s, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-050", "active", 60)
	seedTaskWithAssignee(t, cfg, "T-050", "agent-b")
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	code := goalBreakdownTo(&buf, s, cfg, GoalBreakdownOpts{Top: 3, JSON: true})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	var out []goalBreakdownGoalJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 goal, got %d", len(out))
	}
	if len(out[0].Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(out[0].Items))
	}
	if !strings.Contains(out[0].PeerNote, "T-050") {
		t.Errorf("expected T-050 in peer_note, got: %q", out[0].PeerNote)
	}
	if !strings.Contains(out[0].PeerNote, "agent-b") {
		t.Errorf("expected agent-b in peer_note, got: %q", out[0].PeerNote)
	}
}
