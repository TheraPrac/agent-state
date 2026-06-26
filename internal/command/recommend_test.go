package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
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
func TestRecommend_QueueFlagNoOp(t *testing.T) {
	// T-461: --queue flag is a backward-compat no-op; both views use property-
	// based candidates. A pin adds a "queue pin" score boost but does not gate
	// visibility. Ready items appear regardless of queue state.
	s, cfg := setupTestEnv(t)

	// Without pins, --queue and default view return the same ready items.
	withFlag := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{Queue: true}) })
	without := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{}) })
	if withFlag != without {
		t.Fatalf("--queue flag must not change the candidate set:\nwith: %s\nwithout: %s", withFlag, without)
	}

	// After pinning T-001, the rationale shows the queue-pin score boost.
	// Pin stays within the priority band (no effective-priority change).
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	out := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{Queue: true}) })
	if !strings.Contains(out, "queue-pin") {
		t.Fatalf("pinned T-001 must show queue-pin in rationale:\n%s", out)
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

// setupTestEnvWithGoalTaggedItem seeds G-TEST (active goal) and T-010 — an
// item explicitly tagged `goals: [G-TEST]` — alongside the base T-001/T-002
// fixtures (which carry no goal tag). Used by I-896 goal-filter tests.
func setupTestEnvWithGoalTaggedItem(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	_, cfg := setupTestEnv(t)
	root := cfg.Root()

	// Goal file.
	os.MkdirAll(filepath.Join(root, "goals"), 0755)
	goalContent := "id: G-TEST\ntype: goal\nstatus: active\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: Test goal\nweight: 40\n"
	writeFile(t, filepath.Join(root, "goals", "G-TEST-test-goal.md"), goalContent)

	// Item tagged to G-TEST (queued, unblocked, unassigned).
	itemContent := `id: T-010
type: task
status: queued
created: 2026-03-25T09:00:00-06:00
last_touched: 2026-03-25T09:00:00-06:00
title: Goal-tagged task
goals:
- G-TEST
sbar:
  situation: |-
    I-896 fixture item tagged to G-TEST.
  background: |-
    Used to verify --goal filtering in st next.
  assessment: |-
    Should appear only when G-TEST is the filter.
  recommendation: |-
    Keep fixture stable.
`
	writeFile(t, filepath.Join(root, "tasks", "T-010-goal-tagged-task.md"), itemContent)

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New after goal seed: %v", err)
	}
	return s2, cfg
}

// I-896: st next --goal restricts candidates to items tagged with that goal.
func TestRecommend_GoalFlagFiltersToGoalItems(t *testing.T) {
	s, cfg := setupTestEnvWithGoalTaggedItem(t)

	var rc int
	out := captureStdout(t, func() {
		rc = Recommend(s, cfg, RecommendOpts{Top: 1, Brief: true, Goal: "G-TEST"})
	})
	if rc != 0 {
		t.Fatalf("Recommend with --goal G-TEST rc=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "T-010") {
		t.Errorf("expected T-010 (tagged to G-TEST) in output; got:\n%s", out)
	}
	// T-001 is untagged — must not appear when goal filter is active.
	if strings.Contains(out, "T-001") {
		t.Errorf("--goal G-TEST must not return untagged T-001; got:\n%s", out)
	}
}

// I-896: when --goal names a non-existent goal, Recommend returns the
// "no recommendable items" message rather than silently returning
// cross-goal candidates. Tests the filter-returns-nil path added in I-896.
func TestRecommend_GoalFlagNonExistentGoalReturnsNoItems(t *testing.T) {
	s, cfg := setupTestEnvWithGoalTaggedItem(t)

	var rc int
	out := captureStdout(t, func() {
		rc = Recommend(s, cfg, RecommendOpts{Top: 5, Brief: true, Goal: "G-NONEXISTENT"})
	})
	if rc != 0 {
		t.Fatalf("Recommend with unknown --goal rc=%d\n%s", rc, out)
	}
	// Must not silently return cross-goal items (Top:5 to ensure all candidates
	// would appear if the filter were skipped).
	if strings.Contains(out, "T-001") || strings.Contains(out, "T-010") {
		t.Errorf("--goal G-NONEXISTENT must not return cross-goal items; got:\n%s", out)
	}
	// Must emit the explicit "no recommendable items" message so the operator
	// knows there are zero eligible items, not a full unfiltered list.
	if !strings.Contains(out, "No recommendable") {
		t.Errorf("--goal G-NONEXISTENT must print 'No recommendable' message; got:\n%s", out)
	}
}

// I-896: when --goal names a terminal (done) goal, Recommend returns empty
// rather than the full unfiltered candidate set.
func TestRecommend_GoalFlagTerminalGoalReturnsNoItems(t *testing.T) {
	_, cfg := setupTestEnvWithGoalTaggedItem(t)
	root := cfg.Root()

	// Seed a done goal.
	doneContent := "id: G-DONE\ntype: goal\nstatus: done\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: Done goal\nweight: 40\n"
	writeFile(t, filepath.Join(root, "goals", "G-DONE-done-goal.md"), doneContent)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var rc int
	out := captureStdout(t, func() {
		rc = Recommend(s2, cfg, RecommendOpts{Top: 5, Brief: true, Goal: "G-DONE"})
	})
	if rc != 0 {
		t.Fatalf("Recommend with terminal --goal rc=%d\n%s", rc, out)
	}
	if strings.Contains(out, "T-001") || strings.Contains(out, "T-010") {
		t.Errorf("--goal G-DONE (terminal) must not return cross-goal items; got:\n%s", out)
	}
	if !strings.Contains(out, "No recommendable") {
		t.Errorf("--goal G-DONE must print 'No recommendable' message; got:\n%s", out)
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

// TestRecommend_PriorityInheritanceSurfacesBlocker verifies that a low-priority
// blocker of a high-priority item surfaces before a higher-labeled unblocked item.
// Scenario: T-BLK (p3) blocks T-HI (p0); T-MID (p2) is unblocked. T-BLK must
// surface first because its effective priority inherits p0 from T-HI.
func TestRecommend_PriorityInheritanceSurfacesBlocker(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// T-BLK: p3 blocker of T-HI
	writeFile(t, filepath.Join(root, "tasks", "T-BLK-blocker.md"), `id: T-BLK
type: task
status: queued
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-01T00:00:00Z
title: Low-priority blocker
priority: 3
sbar:
  situation: Blocker fixture for priority inheritance test.
  background: Blocks T-HI (p0). Effective priority should inherit p0.
  assessment: Without inheritance, this stays p3 and is buried.
  recommendation: Build TransitiveMinPriority.
`)

	// T-HI: p0, blocked by T-BLK
	writeFile(t, filepath.Join(root, "tasks", "T-HI-high.md"), `id: T-HI
type: task
status: queued
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-01T00:00:00Z
title: High-priority blocked task
priority: 0
depends_on:
- T-BLK
sbar:
  situation: High-priority task blocked by T-BLK.
  background: T-BLK must be done first.
  assessment: Blocked until T-BLK resolves.
  recommendation: Fix blocker.
`)

	// T-MID: p2, unblocked
	writeFile(t, filepath.Join(root, "tasks", "T-MID-mid.md"), `id: T-MID
type: task
status: queued
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-01T00:00:00Z
title: Mid-priority unblocked task
priority: 2
sbar:
  situation: Mid-priority unblocked item.
  background: Candidate for scheduling.
  assessment: Outranks T-BLK by label alone.
  recommendation: Should lose to T-BLK after inheritance.
`)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	out := captureStdout(t, func() { Recommend(s, cfg, RecommendOpts{}) })

	blkIdx := strings.Index(out, "T-BLK")
	midIdx := strings.Index(out, "T-MID")
	if blkIdx < 0 {
		t.Fatalf("T-BLK not in output:\n%s", out)
	}
	if midIdx < 0 {
		t.Fatalf("T-MID not in output:\n%s", out)
	}
	if blkIdx > midIdx {
		t.Errorf("T-BLK (p3, effective p0) must precede T-MID (p2); got:\n%s", out)
	}
	// T-HI is blocked — must not appear as a candidate
	if strings.Contains(out, "T-HI") && !strings.Contains(out, "T-HI") {
		t.Errorf("T-HI (blocked) must not appear as a candidate:\n%s", out)
	}
	// Rationale must show effective priority
	if !strings.Contains(out, "effective") {
		t.Errorf("T-BLK rationale must show effective priority:\n%s", out)
	}
}

// TestRecommend_SurfacesPeerAssignedItems verifies that items assigned to a
// peer agent are surfaced as a footnote rather than silently dropped (I-1435).
func TestRecommend_SurfacesPeerAssignedItems(t *testing.T) {
	s, cfg := setupTestEnv(t)
	root := cfg.Root()

	writeFile(t, filepath.Join(root, "tasks", "T-005-peer-owned.md"), `id: T-005
type: task
status: queued
created: 2026-01-01T10:00:00-06:00
last_touched: 2026-01-01T10:00:00-06:00
assigned_to: agent-peer
priority: 0

completed: null

title: Peer-owned task

sbar:
  situation: Peer-owned task fixture for I-1435 test.
  background: Tests that peer-assigned items appear in the footnote.
  assessment: Should not appear as a normal candidate.
  recommendation: Test only.
`)

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var rc int
	out := captureStdout(t, func() { rc = Recommend(s2, cfg, RecommendOpts{}) })
	if rc != 0 {
		t.Fatalf("Recommend returned %d\n%s", rc, out)
	}
	if !strings.Contains(out, "not shown") {
		t.Errorf("output must contain peer-assigned footnote; got:\n%s", out)
	}
	if !strings.Contains(out, "agent-peer") {
		t.Errorf("footnote must name the peer agent; got:\n%s", out)
	}
	if !strings.Contains(out, "T-005") {
		t.Errorf("footnote must name the peer-assigned item; got:\n%s", out)
	}
	_ = s
}
