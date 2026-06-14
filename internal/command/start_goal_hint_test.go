package command

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// captureStderrStr captures os.Stderr output while fn runs.
func captureStderrStr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	_ = w.Close()
	os.Stderr = old
	<-done
	return buf.String()
}

// newStartGoalHintEnv creates a temp env with tasks/, issues/, goals/, archive/, .as/
// and returns a store+config. Used by goal-hint tests that need both task and goal types.
func newStartGoalHintEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "goals", "archive", ".as"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

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

func writeTaskFixture(t *testing.T, cfg *config.Config, id, status string, goals []string) {
	t.Helper()
	dir := "tasks"
	if status == "done" || status == "abandoned" || status == "resolved" {
		dir = "archive"
	}
	goalsLine := "goals:\n- []"
	if len(goals) > 0 {
		var sb strings.Builder
		sb.WriteString("goals:")
		for _, g := range goals {
			sb.WriteString("\n- ")
			sb.WriteString(g)
		}
		goalsLine = sb.String()
	}
	content := fmt.Sprintf(`id: %s
type: task
status: %s
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Task %s

%s

sbar:
  situation: |-
    Test task for goal-hint tests.
  background: |-
    Test.
  assessment: |-
    Test.
  recommendation: |-
    Test.
`, id, status, id, goalsLine)
	slug := strings.ToLower(strings.ReplaceAll(id, "-", "")) + "-task"
	path := filepath.Join(cfg.ItemDir(), dir, id+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeTaskFixture: %v", err)
	}
}

func writeGoalFixtureHint(t *testing.T, cfg *config.Config, id, status string, weight int) {
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
success_criterion: All done.

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
		t.Fatalf("writeGoalFixtureHint: %v", err)
	}
}

func TestStartGoalHint_FiresWhenNoGoalLink(t *testing.T) {
	s, cfg := newStartGoalHintEnv(t)
	writeTaskFixture(t, cfg, "T-001", "queued", nil)
	writeGoalFixtureHint(t, cfg, "G-001", "active", 40)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if !strings.Contains(stderr, "has no goal link") {
		t.Errorf("expected goal-link hint in stderr; got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "G-001") {
		t.Errorf("expected active goal G-001 listed in hint; got:\n%s", stderr)
	}
	_ = s
}

func TestStartGoalHint_SilentWhenGoalLinked(t *testing.T) {
	s, cfg := newStartGoalHintEnv(t)
	writeTaskFixture(t, cfg, "T-001", "queued", []string{"G-001"})
	writeGoalFixtureHint(t, cfg, "G-001", "active", 40)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if strings.Contains(stderr, "has no goal link") {
		t.Errorf("expected no goal-link hint when item has goals; got:\n%s", stderr)
	}
	_ = s
}

func TestStartGoalHint_SilentForGoalTypeItem(t *testing.T) {
	_, cfg := newStartGoalHintEnv(t)
	writeGoalFixtureHint(t, cfg, "G-001", "draft", 40)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "G-001", StartOpts{NoPush: true})
		})
	})

	if strings.Contains(stderr, "has no goal link") {
		t.Errorf("goal-type item should not receive goal-link hint; got:\n%s", stderr)
	}
}

// I-1328: item linked only to a terminal (met/dropped) goal must fire the hint.
func TestStartGoalHint_FiresWhenAllGoalsTerminal(t *testing.T) {
	s, cfg := newStartGoalHintEnv(t)
	writeTaskFixture(t, cfg, "T-001", "queued", []string{"G-met"})
	writeGoalFixtureHint(t, cfg, "G-met", "met", 40)
	writeGoalFixtureHint(t, cfg, "G-active", "active", 30)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if !strings.Contains(stderr, "has no active goal link") {
		t.Errorf("expected active-goal-link hint; got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "G-active") {
		t.Errorf("expected active goal G-active listed; got:\n%s", stderr)
	}
	_ = s
}

// I-1328: item linked to a draft goal fires the hint with "has no active goal link"
// (not "all linked goals are terminal" — draft is non-terminal).
func TestStartGoalHint_FiresWhenGoalIsDraft(t *testing.T) {
	s, cfg := newStartGoalHintEnv(t)
	writeTaskFixture(t, cfg, "T-001", "queued", []string{"G-draft"})
	writeGoalFixtureHint(t, cfg, "G-draft", "draft", 40)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if !strings.Contains(stderr, "has no active goal link") {
		t.Errorf("expected active-goal-link hint for draft goal; got:\n%s", stderr)
	}
	if strings.Contains(stderr, "terminal") {
		t.Errorf("hint must not claim draft goal is terminal; got:\n%s", stderr)
	}
	_ = s
}

// I-1328: item with one active and one terminal goal must NOT fire the hint.
func TestStartGoalHint_SilentWhenMixedActiveAndTerminalGoals(t *testing.T) {
	s, cfg := newStartGoalHintEnv(t)
	writeTaskFixture(t, cfg, "T-001", "queued", []string{"G-met", "G-active"})
	writeGoalFixtureHint(t, cfg, "G-met", "met", 40)
	writeGoalFixtureHint(t, cfg, "G-active", "active", 30)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			Start(s2, cfg, "T-001", StartOpts{NoPush: true})
		})
	})

	if strings.Contains(stderr, "goal link") {
		t.Errorf("should not hint when at least one goal is active; got:\n%s", stderr)
	}
	_ = s
}
