package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Default (PLANNING) view = g.Ready(): T-001 (queued, unblocked,
// unassigned) is recommendable; it blocks the still-open T-002, so its
// rationale must NAME that leverage. T-002 (blocked by T-001), T-003
// (active+assigned) and T-004 (done) are correctly excluded.
func TestRecommend_TextPlanningView(t *testing.T) {
	s, cfg := setupTestEnv(t)

	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{}) })
	if rc != 0 {
		t.Fatalf("rc = %d\n%s", rc, out)
	}

	if !strings.Contains(out, "T-001") {
		t.Fatalf("T-001 must be recommendable\n%s", out)
	}
	if !strings.Contains(out, "unblocks 1 (T-002)") {
		t.Errorf("T-001 rationale must name the unblocked item\n%s", out)
	}
	if !strings.Contains(out, "priority p2") || !strings.Contains(out, "why:") {
		t.Errorf("rationale must be decomposed + labelled\n%s", out)
	}
	if strings.Contains(out, "T-002") && !strings.Contains(out, "unblocks 1 (T-002)") {
		t.Errorf("blocked T-002 must not itself be a candidate\n%s", out)
	}
	// Priority dominance: if the p1 issue is present it must precede the p2 task.
	if i, j := strings.Index(out, "I-001"), strings.Index(out, "T-001"); i >= 0 && i > j {
		t.Errorf("p1 I-001 must outrank p2 T-001\n%s", out)
	}
}

func TestRecommend_TopLimit(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Top: 1})
	})
	// Exactly one item row (each row prints one "why:" line).
	if n := strings.Count(out, "why:"); n != 1 {
		t.Fatalf("--top 1 must print exactly one item, got %d\n%s", n, out)
	}
}

func TestRecommend_JSONStableContract(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{JSON: true}) })
	if rc != 0 {
		t.Fatalf("json rc != 0\n%s", out)
	}

	var got []recommendJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) == 0 {
		t.Fatalf("expected ≥1 recommendation\n%s", out)
	}
	var t001 *recommendJSON
	for i := range got {
		if got[i].ID == "T-001" {
			t001 = &got[i]
		}
	}
	if t001 == nil {
		t.Fatalf("T-001 missing from JSON\n%s", out)
	}
	if t001.Priority != 2 {
		t.Errorf("T-001 priority = %d, want 2", t001.Priority)
	}
	if !strings.Contains(t001.Rationale, "unblocks 1 (T-002)") {
		t.Errorf("T-001 JSON rationale missing leverage: %q", t001.Rationale)
	}
	var hasUnblock bool
	for _, f := range t001.Factors {
		if f.Name == "unblock" {
			hasUnblock = true
		}
	}
	if !hasUnblock {
		t.Errorf("T-001 factors must include the unblock factor: %+v", t001.Factors)
	}
}

// DISPATCH view: empty queue ⇒ nothing; after a user-approved add the
// eligible item appears (mirrors selectNext's candidate set exactly).
func TestRecommend_QueueDispatchView(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "") // user-added ⇒ approved (I-490)

	empty := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Queue: true})
	})
	if !strings.Contains(empty, "No recommendable items") {
		t.Fatalf("empty queue must yield none, got:\n%s", empty)
	}

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	out := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Queue: true})
	})
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "why:") {
		t.Fatalf("approved+eligible T-001 must appear in dispatch view:\n%s", out)
	}
}

// --scope sprint with no registry ⇒ resilient empty set, not an error.
func TestRecommend_SprintScopeNoRegistry(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{Scope: "sprint"}) })
	if rc != 0 {
		t.Fatalf("must not error without a registry, rc!=0\n%s", out)
	}
	if !strings.Contains(out, "No recommendable items") {
		t.Fatalf("no active sprint ⇒ no candidates, got:\n%s", out)
	}
}

// Active goal weight is applied and appears in the rationale.
func TestRecommend_GoalWeightAppliedFromActiveGoals(t *testing.T) {
	s, cfg := setupTestEnvWithGoal(t, true)
	out := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{}) })
	if !strings.Contains(out, "goal-weight") {
		t.Fatalf("active goal weight must appear in rationale\n%s", out)
	}
}

// Inactive (non-active) goal contributes zero weight.
func TestRecommend_InactiveGoalContributesZero(t *testing.T) {
	s, cfg := setupTestEnvWithGoal(t, false)
	out := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{}) })
	if strings.Contains(out, "goal-weight") {
		t.Fatalf("inactive goal must not appear in rationale\n%s", out)
	}
}

// --brief renders a single one-line output.
func TestRecommend_BriefFormat(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{Brief: true, Top: 1}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("--brief must produce exactly one line, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, " — ") {
		t.Fatalf("--brief line must contain ' — ' separator\n%s", out)
	}
}

// No goals corpus (no goal items in store) ⇒ resilient, exits 0.
func TestRecommend_NoGoalsCorpusResilient(t *testing.T) {
	s, cfg := setupTestEnv(t) // baseline env has no goals
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{}) })
	if rc != 0 {
		t.Fatalf("must not error without goals corpus, rc=%d\n%s", rc, out)
	}
	if strings.Contains(out, "goal-weight") {
		t.Fatalf("no goals ⇒ no goal-weight in rationale\n%s", out)
	}
}

// TestRecommend_GoalFocusFiltersCandidates verifies that an agent with a
// focus_goal set only sees items linked to that goal.
func TestRecommend_GoalFocusFiltersCandidates(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	// Seed an active goal and link T-001 to it.
	if err := os.MkdirAll(filepath.Join(cfg.Root(), "goals"), 0755); err != nil {
		t.Fatal(err)
	}
	seedGoalFile(t, cfg, "G-FOCUS", "active", 40)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// Link T-001 to G-FOCUS via ItemGoalsAdd so it.Goals is populated.
	if rc := ItemGoalsAdd(s2, cfg, "T-001", []string{"G-FOCUS"}); rc != 0 {
		t.Fatalf("ItemGoalsAdd rc=%d", rc)
	}
	s3, _ := store.New(cfg)

	// Set agent focus to G-FOCUS.
	if err := setGoalFocusForTest(cfg, "agent-tt", "G-FOCUS"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		Recommend(s3, cfg, RecommendOpts{})
	})
	// T-001 is in G-FOCUS — must appear.
	if !strings.Contains(out, "T-001") {
		t.Errorf("T-001 (in focus goal) must appear: %s", out)
	}
}

// TestRecommend_NoGoalFocusUnchanged verifies that without a focus the full
// candidate set is returned (baseline regression).
func TestRecommend_NoGoalFocusUnchanged(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")
	// No focus set — expect normal output.
	out := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{})
	})
	if !strings.Contains(out, "T-001") {
		t.Errorf("without focus T-001 must appear in global ranking: %s", out)
	}
}

// TestRecommend_GoalFocusAutoClearsTerminalGoal verifies that a focus on a
// terminal (met) goal is auto-cleared and the global candidate set is used.
func TestRecommend_GoalFocusAutoClearsTerminalGoal(t *testing.T) {
	_, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-tt")

	// Seed a goal that is met (terminal).
	if err := os.MkdirAll(filepath.Join(cfg.Root(), "goals"), 0755); err != nil {
		t.Fatal(err)
	}
	// Note: seedGoalFile puts met goals in archive/ — the store still loads them.
	seedGoalFile(t, cfg, "G-MET", "met", 40)
	s2, _ := store.New(cfg)

	if err := setGoalFocusForTest(cfg, "agent-tt", "G-MET"); err != nil {
		t.Fatal(err)
	}

	// Recommend must not error and must return global results (auto-cleared stale focus).
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s2, cfg, RecommendOpts{}) })
	if rc != 0 {
		t.Fatalf("Recommend with terminal focus rc=%d\n%s", rc, out)
	}
	// T-001 must surface (global ranking restored).
	if !strings.Contains(out, "T-001") {
		t.Errorf("after auto-clear of terminal focus, T-001 must appear: %s", out)
	}
}

func setGoalFocusForTest(cfg *config.Config, agentID, goalID string) error {
	return agent.SetGoalFocus(cfg, agentID, goalID)
}

// setupTestEnvWithGoal extends the base env with a goal item whose status is
// either "active" (active=true) or "done" (active=false), linked to T-001.
func setupTestEnvWithGoal(t *testing.T, active bool) (*store.Store, *config.Config) {
	t.Helper()
	_, cfg := setupTestEnv(t)

	status := "done"
	if active {
		status = "active"
	}
	weight := 40
	goalContent := "id: G-TEST\ntype: goal\nstatus: " + status + "\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: Test goal\nweight: " + strconv.Itoa(weight) + "\ngoals:\n- T-001\n"

	root := cfg.Root()
	os.MkdirAll(filepath.Join(root, "goals"), 0755)
	os.WriteFile(filepath.Join(root, "goals", "G-TEST-test-goal.md"), []byte(goalContent), 0644)

	// Reload store to pick up the new goal file.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New after goal seed: %v", err)
	}
	return s2, cfg
}
