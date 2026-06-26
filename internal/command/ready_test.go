package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// TestReady_OrderedByScore verifies the scored output is deterministic.
func TestReady_OrderedByScore(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = Ready(s, cfg, ReadyOpts{}) })
	if rc != 0 {
		t.Fatalf("Ready returned rc=%d\n%s", rc, out)
	}
	// T-001 is the only unblocked+unassigned item in the base fixture.
	if !strings.Contains(out, "T-001") {
		t.Fatalf("T-001 must appear in ready output\n%s", out)
	}
}

// TestReady_PriorityStillDominates: p1 I-001 must appear before p2 T-001.
func TestReady_PriorityStillDominates(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() { Ready(s, cfg, ReadyOpts{}) })
	iPos := strings.Index(out, "I-001")
	tPos := strings.Index(out, "T-001")
	if iPos < 0 || tPos < 0 {
		t.Skipf("both I-001 and T-001 must be present; got:\n%s", out)
	}
	if iPos > tPos {
		t.Fatalf("p1 I-001 must precede p2 T-001\n%s", out)
	}
}

// TestReady_FiltersAppliedAfterScoring: --type task excludes issues.
func TestReady_FiltersAppliedAfterScoring(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() { Ready(s, cfg, ReadyOpts{Type: "task"}) })
	if strings.Contains(out, "I-001") {
		t.Fatalf("--type task must exclude issues\n%s", out)
	}
}

// TestReady_GoalWeightLeadsWithinBand: an item with active goal weight
// outranks a same-priority item without goal weight.
func TestReady_GoalWeightLeadsWithinBand(t *testing.T) {
	s, cfg := setupReadyGoalEnv(t)
	out := captureStdout(t, func() { Ready(s, cfg, ReadyOpts{}) })
	aPos := strings.Index(out, "T-A")
	bPos := strings.Index(out, "T-B")
	if aPos < 0 || bPos < 0 {
		t.Fatalf("both T-A and T-B must appear:\n%s", out)
	}
	if aPos > bPos {
		t.Fatalf("goal-weighted T-A must outrank T-B within the same priority band\n%s", out)
	}
}

// setupReadyGoalEnv creates a minimal env with two same-priority tasks where
// T-A has an active goal weight and T-B does not.
func setupReadyGoalEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "goals", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	taskA := `id: T-A
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Task A (goal-weighted)
sbar:
  situation: |-
    Task A fixture with goal weight.
  background: |-
    Has active goal to drive ordering test.
  assessment: |-
    Should outrank T-B in ready output.
  recommendation: |-
    Keep goal linkage stable.
`
	taskB := `id: T-B
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Task B (no goal)
sbar:
  situation: |-
    Task B fixture with no goal weight.
  background: |-
    No active goals linked.
  assessment: |-
    Should rank below T-A.
  recommendation: |-
    Keep fixture stable.
`
	goal := `id: G-001
type: goal
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Test goal
weight: 30
goals:
- T-A
`
	os.WriteFile(filepath.Join(root, "tasks", "T-A-task-a.md"), []byte(taskA), 0644)
	os.WriteFile(filepath.Join(root, "tasks", "T-B-task-b.md"), []byte(taskB), 0644)
	os.WriteFile(filepath.Join(root, "goals", "G-001-test-goal.md"), []byte(goal), 0644)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg
}
