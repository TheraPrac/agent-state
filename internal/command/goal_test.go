package command

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// newGoalEnv creates a temp root with goals/ and archive/ directories, loads
// config from it, and returns a fresh store. It uses config.Load so that
// cfg.root (and thus cfg.ItemDir()) is set correctly.
func newGoalEnv(t *testing.T) (root string, s *store.Store, cfg *config.Config) {
	t.Helper()
	root = t.TempDir()
	for _, dir := range []string{"tasks", "issues", "goals", "archive", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	var err error
	cfg, err = config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return root, s, cfg
}

// reloadStoreGoal re-opens the store from disk (to pick up files written directly).
func reloadStoreGoal(t *testing.T, cfg *config.Config) *store.Store {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// seedGoalFile writes a goal fixture directly into the correct directory.
func seedGoalFile(t *testing.T, cfg *config.Config, id, status string, weight int) {
	t.Helper()
	dir := "goals"
	if status == "met" || status == "dropped" || status == "archived" {
		dir = "archive"
	}
	content := fmt.Sprintf(`id: %s
type: goal
status: %s
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Goal %s

weight: %d
success_criterion:

sbar:
  situation: |-
    Test goal.
  background: |-
    Test.
  assessment: |-
    Test.
  recommendation: |-
    Test.
`, id, status, id, weight)

	slug := strings.ToLower(strings.ReplaceAll(id, "-", "")) + "-goal"
	path := filepath.Join(cfg.ItemDir(), dir, id+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("seedGoalFile: %v", err)
	}
}

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

func TestGoalCreateValidatesWeight(t *testing.T) {
	cases := []struct {
		weight int
		wantRC int
	}{
		{0, 2},
		{-1, 2},
		{101, 2},
		{1, 0},
		{100, 0},
		{50, 0},
	}
	for _, tc := range cases {
		_, s, cfg := newGoalEnv(t)
		rc := GoalCreate(s, cfg, "Test Goal", tc.weight)
		if rc != tc.wantRC {
			t.Errorf("GoalCreate(weight=%d) rc=%d, want %d", tc.weight, rc, tc.wantRC)
		}
	}
}

func TestGoalActivateEnforcesWeightSum(t *testing.T) {
	_, _, cfg := newGoalEnv(t)

	// Two active goals sum to 90; draft goal G-003 at 20 would push to 110.
	seedGoalFile(t, cfg, "G-001", "active", 60)
	seedGoalFile(t, cfg, "G-002", "active", 30)
	seedGoalFile(t, cfg, "G-003", "draft", 20)
	// Draft goal at 10 — sum would be 100 (ok).
	seedGoalFile(t, cfg, "G-004", "draft", 10)

	s := reloadStoreGoal(t, cfg)

	if rc := GoalActivate(s, cfg, "G-003"); rc == 0 {
		t.Error("GoalActivate(G-003, weight=20) with active sum=90 should fail (110>100)")
	}
	if rc := GoalActivate(s, cfg, "G-004"); rc != 0 {
		t.Errorf("GoalActivate(G-004, weight=10) with active sum=90 should succeed (100≤100), got rc=%d", rc)
	}
}

func TestGoalMarkMetTransitions(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedGoalFile(t, cfg, "G-002", "draft", 20)
	s := reloadStoreGoal(t, cfg)

	// active → met: ok.
	if rc := GoalMarkMet(s, cfg, "G-001"); rc != 0 {
		t.Errorf("GoalMarkMet(active) rc=%d, want 0", rc)
	}
	g, _ := s.Get("G-001")
	if g.Status != "met" {
		t.Errorf("G-001 status = %q after mark-met, want met", g.Status)
	}

	// draft → met: must fail.
	if rc := GoalMarkMet(s, cfg, "G-002"); rc == 0 {
		t.Error("GoalMarkMet(draft) should fail")
	}
}

func TestGoalDropRequiresClosedVocabReason(t *testing.T) {
	// "aged" must be rejected.
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s := reloadStoreGoal(t, cfg)
	if rc := GoalDrop(s, cfg, "G-001", "aged"); rc == 0 {
		t.Error("GoalDrop(reason=aged) should fail")
	}

	// Each valid reason must succeed.
	for _, reason := range model.ValidDropReasons {
		_, _, cfg2 := newGoalEnv(t)
		seedGoalFile(t, cfg2, "G-001", "active", 40)
		s2 := reloadStoreGoal(t, cfg2)
		if rc := GoalDrop(s2, cfg2, "G-001", reason); rc != 0 {
			t.Errorf("GoalDrop(reason=%q) rc=%d, want 0", reason, rc)
		}
		g, _ := s2.Get("G-001")
		if g.Status != "dropped" {
			t.Errorf("G-001 status = %q after drop(%q), want dropped", g.Status, reason)
		}
		droppedReason, _ := g.Doc.GetNestedField("delivery.dropped_reason")
		if droppedReason != reason {
			t.Errorf("delivery.dropped_reason = %q, want %q", droppedReason, reason)
		}
	}

	// Already-terminal goal must fail.
	_, _, cfg3 := newGoalEnv(t)
	seedGoalFile(t, cfg3, "G-001", "dropped", 40)
	s3 := reloadStoreGoal(t, cfg3)
	if rc := GoalDrop(s3, cfg3, "G-001", "superseded"); rc == 0 {
		t.Error("GoalDrop on already-dropped goal should fail")
	}

	// Draft goal must fail — drop requires active.
	_, _, cfg4 := newGoalEnv(t)
	seedGoalFile(t, cfg4, "G-001", "draft", 40)
	s4 := reloadStoreGoal(t, cfg4)
	if rc := GoalDrop(s4, cfg4, "G-001", "superseded"); rc == 0 {
		t.Error("GoalDrop on draft goal should fail (must be active)")
	}
}

func TestGoalListGroupsByLifecycleWithSum(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedGoalFile(t, cfg, "G-002", "active", 60)
	seedGoalFile(t, cfg, "G-003", "draft", 20)
	s := reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	goalListTo(&buf, s, cfg)
	out := buf.String()

	if !strings.Contains(out, "ACTIVE") {
		t.Errorf("output missing ACTIVE bucket:\n%s", out)
	}
	if !strings.Contains(out, "DRAFT") {
		t.Errorf("output missing DRAFT bucket:\n%s", out)
	}
	if !strings.Contains(out, "100 / 100") {
		t.Errorf("output missing active weight sum 100/100:\n%s", out)
	}
}

func TestGoalListShowsArchivedGoals(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	seedGoalFile(t, cfg, "G-002", "archived", 30)
	s := reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	goalListTo(&buf, s, cfg)
	out := buf.String()

	if !strings.Contains(out, "ARCHIVED") {
		t.Errorf("archived goals not shown in list output:\n%s", out)
	}
	if !strings.Contains(out, "G-002") {
		t.Errorf("G-002 (archived) missing from list output:\n%s", out)
	}
}

func TestGoalShowRendersWeight(t *testing.T) {
	_, _, cfg := newGoalEnv(t)
	seedGoalFile(t, cfg, "G-001", "active", 40)
	s := reloadStoreGoal(t, cfg)

	var buf bytes.Buffer
	showTo(&buf, s, cfg, "G-001", ShowOpts{})
	out := buf.String()

	if !strings.Contains(out, "weight: 40") {
		t.Errorf("show output missing 'weight: 40':\n%s", out)
	}
}
