package command

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
)

// seedTaskInGoalEnv writes a minimal task fixture into the tasks dir of a goal env.
func seedTaskInGoalEnv(t *testing.T, cfg *config.Config, id, status string) {
	t.Helper()
	dir := "tasks"
	if status == "done" || status == "completed" || status == "abandoned" || status == "resolved" {
		dir = "archive"
	}
	content := fmt.Sprintf(`id: %s
type: task
status: %s
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
`, id, status, id)
	slug := strings.ToLower(strings.ReplaceAll(id, "-", "")) + "-task"
	path := filepath.Join(cfg.ItemDir(), dir, id+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("seedTaskInGoalEnv: %v", err)
	}
}

func TestGoalMustDoAddCreatesBucket(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001", "T-002"})
	if rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d, want 0", rc)
	}

	goal, ok := s.Get("G-001")
	if !ok {
		t.Fatal("G-001 not found after add")
	}
	if len(goal.MustDo["clinical"]) != 2 {
		t.Errorf("MustDo[clinical] = %v, want 2 items", goal.MustDo["clinical"])
	}
	if goal.MustDo["clinical"][0] != "T-001" || goal.MustDo["clinical"][1] != "T-002" {
		t.Errorf("MustDo[clinical] = %v, want [T-001 T-002]", goal.MustDo["clinical"])
	}
}

func TestGoalMustDoAddFlatList(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-010", "queued")
	s := reloadStoreGoal(t, cfg)

	// No bucket → flat list (empty-string key).
	rc := GoalMustDoAdd(s, cfg, "G-001", "", []string{"T-010"})
	if rc != 0 {
		t.Fatalf("GoalMustDoAdd (flat) rc=%d, want 0", rc)
	}

	goal, ok := s.Get("G-001")
	if !ok {
		t.Fatal("G-001 not found after flat add")
	}
	flat := goal.MustDo[""]
	if len(flat) != 1 || flat[0] != "T-010" {
		t.Errorf("MustDo[\"\"] = %v, want [T-010]", flat)
	}
}

func TestGoalMustDoAddRejectsUnknownItem(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s := reloadStoreGoal(t, cfg)

	rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-999"})
	if rc == 0 {
		t.Error("GoalMustDoAdd with unknown item should fail")
	}
}

func TestGoalMustDoAddRejectsDuplicate(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001"}); rc != 0 {
		t.Fatalf("first add rc=%d, want 0", rc)
	}

	// Reload so the second add reads the mutated state.
	s = reloadStoreGoal(t, cfg)
	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001"}); rc == 0 {
		t.Error("duplicate GoalMustDoAdd should fail")
	}
}

func TestGoalMustDoAddRejectsNonGoal(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	// Seed a task as the "goalID" — should be rejected.
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	rc := GoalMustDoAdd(s, cfg, "T-001", "clinical", []string{"T-002"})
	if rc == 0 {
		t.Error("GoalMustDoAdd on non-goal should fail")
	}
}

func TestGoalMustDoAddWritesChangelog(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "billing", []string{"T-001", "T-002"}); rc != 0 {
		t.Fatalf("GoalMustDoAdd rc=%d, want 0", rc)
	}

	entries, err := changelog.Read(cfg, "G-001")
	if err != nil {
		t.Fatalf("changelog.Read: %v", err)
	}
	addEntries := 0
	for _, e := range entries {
		if e.Op == "must_do_add" {
			addEntries++
		}
	}
	if addEntries != 2 {
		t.Errorf("changelog has %d must_do_add entries, want 2", addEntries)
	}
}

func TestGoalMustDoRemovePrunesEmptyBucket(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	s := reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001"}); rc != 0 {
		t.Fatalf("add rc=%d, want 0", rc)
	}
	s = reloadStoreGoal(t, cfg)

	if rc := GoalMustDoRemove(s, cfg, "G-001", []string{"T-001"}); rc != 0 {
		t.Fatalf("remove rc=%d, want 0", rc)
	}

	goal, _ := s.Get("G-001")
	if _, exists := goal.MustDo["clinical"]; exists {
		t.Error("empty bucket 'clinical' should be pruned after remove")
	}
	if len(goal.MustDo) != 0 {
		t.Errorf("MustDo should be empty after removing last item, got %v", goal.MustDo)
	}
}

func TestGoalMustDoListShowsPerItemStatus(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "completed")
	s := reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "clinical", []string{"T-001", "T-002"}); rc != 0 {
		t.Fatalf("add rc=%d, want 0", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	rc := goalMustDoListTo(&buf, s, cfg, "G-001")
	if rc != 0 {
		t.Fatalf("goalMustDoListTo rc=%d, want 0", rc)
	}

	out := buf.String()
	if !strings.Contains(out, "clinical:") {
		t.Errorf("list output missing 'clinical:' bucket header:\n%s", out)
	}
	if !strings.Contains(out, "T-001") {
		t.Errorf("list output missing T-001:\n%s", out)
	}
	if !strings.Contains(out, "T-002") {
		t.Errorf("list output missing T-002:\n%s", out)
	}
	if !strings.Contains(out, "queued") {
		t.Errorf("list output missing 'queued' status for T-001:\n%s", out)
	}
	// T-002 is completed (terminal) — should show done marker and "1/2 done".
	if !strings.Contains(out, "1/2 done") {
		t.Errorf("list output missing '1/2 done' footer:\n%s", out)
	}
}

func TestGoalShowRendersMustDoBuckets(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedTaskInGoalEnv(t, cfg, "T-001", "queued")
	seedTaskInGoalEnv(t, cfg, "T-002", "queued")
	s := reloadStoreGoal(t, cfg)

	if rc := GoalMustDoAdd(s, cfg, "G-001", "billing", []string{"T-001", "T-002"}); rc != 0 {
		t.Fatalf("add rc=%d, want 0", rc)
	}
	s = reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	showTo(&buf, s, cfg, "G-001", ShowOpts{})
	out := buf.String()

	if !strings.Contains(out, "must_do:") {
		t.Errorf("show output missing 'must_do:' line:\n%s", out)
	}
	if !strings.Contains(out, "billing:") {
		t.Errorf("show output missing bucket label 'billing:':\n%s", out)
	}
}
