package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/model"
)

func TestHeuristicAdd(t *testing.T) {
	_, cfg := setupTestEnv(t)

	code := Heuristic_Add(cfg, "when X occurs, do Y", "api,auth")
	if code != 0 {
		t.Fatalf("Heuristic_Add returned %d, want 0", code)
	}

	entries, err := changelog.HeuristicList(cfg, cfg.AgentID(), nil)
	if err != nil {
		t.Fatalf("HeuristicList: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Reason != "when X occurs, do Y" {
		t.Errorf("reason: got %q", entries[0].Reason)
	}
	if entries[0].Kind != changelog.KindHeuristic {
		t.Errorf("kind: got %q, want KindHeuristic", entries[0].Kind)
	}
	if len(entries[0].RelevanceTags) != 2 {
		t.Errorf("expected 2 tags, got %v", entries[0].RelevanceTags)
	}
}

func TestHeuristicAddRequiresText(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := Heuristic_Add(cfg, "", "")
	if code == 0 {
		t.Error("empty text must return non-zero exit code")
	}
}

func TestHeuristicMigrateFromAgentMemory(t *testing.T) {
	_, cfg := setupTestEnv(t)

	// Create agent-memory dir under <root>/theraprac-workspace/agent-memory/
	// (mirrors the real directory layout: cfg.Root() is <root>/.as, so parent is <root>)
	agentMemoryDir := filepath.Join(filepath.Dir(cfg.Root()), "theraprac-workspace", "agent-memory")
	os.MkdirAll(agentMemoryDir, 0755)

	feedbackContent := `---
name: some-rule
description: Don't do this thing
metadata:
  type: feedback
---

When something happens, avoid doing it because it causes problems.
`
	os.WriteFile(filepath.Join(agentMemoryDir, "feedback_some_rule.md"), []byte(feedbackContent), 0644)

	code := Heuristic_Migrate(cfg)
	if code != 0 {
		t.Fatalf("Heuristic_Migrate returned %d, want 0", code)
	}

	entries, err := changelog.HeuristicList(cfg, cfg.AgentID(), nil)
	if err != nil {
		t.Fatalf("HeuristicList: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 migrated entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Reason, "avoid doing it") {
		t.Errorf("reason should contain file body: %q", entries[0].Reason)
	}
	if entries[0].Field != "feedback_some_rule.md" {
		t.Errorf("field should be file basename for idempotency: %q", entries[0].Field)
	}

	// Re-run must be idempotent — no duplicate entries.
	code2 := Heuristic_Migrate(cfg)
	if code2 != 0 {
		t.Fatalf("second Heuristic_Migrate returned %d", code2)
	}
	entries2, _ := changelog.HeuristicList(cfg, cfg.AgentID(), nil)
	if len(entries2) != 1 {
		t.Errorf("idempotent re-run should not add duplicate entries, got %d", len(entries2))
	}
}

func TestHeuristicRetire(t *testing.T) {
	_, cfg := setupTestEnv(t)

	// Seed two heuristics.
	Heuristic_Add(cfg, "rule one: do X", "")
	Heuristic_Add(cfg, "rule two: do Y", "")

	before, _ := changelog.HeuristicActiveList(cfg, cfg.AgentID(), nil)
	if len(before) != 2 {
		t.Fatalf("expected 2 active entries, got %d", len(before))
	}

	// Retire by 1-based index: removes "rule one" from the active list.
	code := Heuristic_Retire(cfg, "1", "rule one is obsolete")
	if code != 0 {
		t.Fatalf("Heuristic_Retire returned %d, want 0", code)
	}

	// Active list must shrink: only "rule two" remains.
	afterFirst, _ := changelog.HeuristicActiveList(cfg, cfg.AgentID(), nil)
	if len(afterFirst) != 1 {
		t.Fatalf("expected 1 active entry after retire, got %d", len(afterFirst))
	}
	if afterFirst[0].Reason != "rule two: do Y" {
		t.Errorf("expected rule two to remain active, got %q", afterFirst[0].Reason)
	}

	// Raw log grows: heuristic_retire tombstone was appended.
	all, _ := changelog.HeuristicList(cfg, cfg.AgentID(), nil)
	if len(all) != 3 {
		t.Fatalf("expected 3 raw entries (2 add + 1 retire), got %d", len(all))
	}
	if all[2].Op != "heuristic_retire" {
		t.Errorf("expected op heuristic_retire, got %q", all[2].Op)
	}
	if all[2].Field != before[0].Timestamp {
		t.Errorf("retire.Field %q != original timestamp %q", all[2].Field, before[0].Timestamp)
	}

	// Cannot retire the same entry twice: it no longer appears in the active list.
	if code := Heuristic_Retire(cfg, before[0].Timestamp, "re-retire"); code == 0 {
		t.Error("retiring an already-retired entry should return non-zero")
	}

	// Out-of-range index (only 1 active entry remains).
	if code := Heuristic_Retire(cfg, "2", "reason"); code == 0 {
		t.Error("out-of-range index should return non-zero")
	}

	// Empty reason.
	if code := Heuristic_Retire(cfg, "1", ""); code == 0 {
		t.Error("empty reason should return non-zero")
	}

	// Retire remaining entry by its full unique timestamp.
	if code := Heuristic_Retire(cfg, before[1].Timestamp, "retiring rule two by timestamp"); code != 0 {
		t.Errorf("timestamp retire returned %d, want 0", code)
	}
	final, _ := changelog.HeuristicActiveList(cfg, cfg.AgentID(), nil)
	if len(final) != 0 {
		t.Errorf("expected 0 active entries after retiring all, got %d", len(final))
	}
}

func TestResumeShowsHeuristics(t *testing.T) {
	cfg := testResumeCfg(t)

	// Seed one heuristic directly via HeuristicAppend.
	err := changelog.HeuristicAppend(cfg, changelog.Entry{
		Op:     "heuristic_add",
		Reason: "always run tests before pushing",
	})
	if err != nil {
		t.Fatalf("HeuristicAppend: %v", err)
	}

	item := &model.Item{ID: "I-804", Title: "heuristic test", Type: "issue", Status: "active"}
	out := renderResume(cfg, item, nil, "", "", "x", tapeAudit{verified: true, message: "ok"}, remoteState{}, nil, nil)

	if !strings.Contains(out, "## Heuristics") {
		t.Errorf("resume output should contain ## Heuristics section:\n%s", out)
	}
	if !strings.Contains(out, "always run tests before pushing") {
		t.Errorf("resume output should contain heuristic reason:\n%s", out)
	}
}
