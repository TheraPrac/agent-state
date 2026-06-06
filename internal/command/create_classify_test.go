package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// freshStore creates a fresh store from cfg — used after writing fixture
// files to disk so the store picks up the new items.
func freshStore(t *testing.T, cfg *config.Config) *store.Store {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New (fresh): %v", err)
	}
	return s
}

// makeClassifyEngine returns a RunEngine whose RunClaude returns a
// classifyVerdict wrapped in a ClaudeResult envelope, matching what
// defaultRunClaude returns in production from claude --output-format json.
func makeClassifyEngine(goalIDs []string) RunEngine {
	inner, _ := json.Marshal(classifyVerdict{GoalIDs: goalIDs, Reason: "test match"})
	envelope, _ := json.Marshal(ClaudeResult{
		Type:    "result",
		Subtype: "success",
		Result:  string(inner),
	})
	return RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return envelope, 0, nil
		},
	}
}

// newClassifyEnv creates a temp env with tasks/, issues/, goals/, archive/, .as/
// and returns a store+config. Mirrors newStartGoalHintEnv.
func newClassifyEnv(t *testing.T) (*store.Store, *config.Config) {
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

// writeGoalFixtureClassify writes a goal item file. Status "active" puts it in
// goals/; other terminal statuses go to archive/.
func writeGoalFixtureClassify(t *testing.T, cfg *config.Config, id, title, status string) {
	t.Helper()
	dir := "goals"
	if status == "met" || status == "dropped" || status == "archived" {
		dir = "archive"
	}
	content := "id: " + id + "\ntype: goal\nstatus: " + status + `
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: ` + title + `

weight: 25
success_criterion: Done.

sbar:
  situation: |-
    Test goal for classify tests.
  background: |-
    Test.
  assessment: |-
    Test.
  recommendation: |-
    Test.
`
	slug := strings.ToLower(strings.ReplaceAll(id, "-", "")) + "-" + strings.ToLower(strings.ReplaceAll(title, " ", "-"))
	path := filepath.Join(cfg.ItemDir(), dir, id+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeGoalFixtureClassify: %v", err)
	}
}

// TestClassifyGoals_NilEngine: nil engine → immediate nil, nil return.
func TestClassifyGoals_NilEngine(t *testing.T) {
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)
	matched, _, err := classifyGoals(s, cfg, "issue", "fix something", "some situation", RunEngine{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected nil goals with nil engine, got %v", matched)
	}
}

// TestClassifyGoals_NonTaskType: idea type → skip.
func TestClassifyGoals_NonTaskType(t *testing.T) {
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)
	engine := makeClassifyEngine([]string{"G-005"})
	matched, _, err := classifyGoals(s, cfg, "idea", "some idea", "", engine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected nil for non-task/issue type, got %v", matched)
	}
}

// TestClassifyGoals_NoActiveGoals: no active goals in store → skip.
func TestClassifyGoals_NoActiveGoals(t *testing.T) {
	s, cfg := newClassifyEnv(t)
	// Write a met goal — should not be returned.
	writeGoalFixtureClassify(t, cfg, "G-001", "Alpha Go-Live", "met")
	s = freshStore(t, cfg)
	engine := makeClassifyEngine([]string{"G-001"})
	matched, _, err := classifyGoals(s, cfg, "issue", "fix something", "situation", engine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected nil when no active goals, got %v", matched)
	}
}

// TestClassifyGoals_MatchReturned: engine returns a valid active goal → returned.
func TestClassifyGoals_MatchReturned(t *testing.T) {
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)
	engine := makeClassifyEngine([]string{"G-005"})
	matched, _, err := classifyGoals(s, cfg, "issue", "st create goal auto-assign", "situation", engine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 1 || matched[0] != "G-005" {
		t.Errorf("expected [G-005], got %v", matched)
	}
}

// TestClassifyGoals_HallucinatedID: engine returns a non-existent goal ID →
// filtered out → nil returned.
func TestClassifyGoals_HallucinatedID(t *testing.T) {
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)
	engine := makeClassifyEngine([]string{"G-999"}) // hallucinated
	matched, _, err := classifyGoals(s, cfg, "issue", "some title", "situation", engine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected nil after filtering hallucinated ID, got %v", matched)
	}
}

// TestClassifyGoals_EnvBypass: AS_INTERNAL_NO_CLASSIFY=1 skips LLM call.
func TestClassifyGoals_EnvBypass(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_CLASSIFY", "1")
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)
	engine := makeClassifyEngine([]string{"G-005"})
	matched, _, err := classifyGoals(s, cfg, "issue", "some title", "situation", engine)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matched) != 0 {
		t.Errorf("expected nil with env bypass, got %v", matched)
	}
}

// TestCreate_AutoAssignsGoal: full Create() call with mock classify engine →
// auto-assigns goal, prints hint to stderr.
func TestCreate_AutoAssignsGoal(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	t.Setenv("AS_INTERNAL_NO_DEDUP", "1")
	t.Setenv("AS_INTERNAL_NO_CLASSIFY", "0") // classify enabled
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)

	stderr := captureStderrStr(t, func() {
		code := Create(s, cfg, "issue", "auto-assign goal test", CreateOpts{
			Priority:       2,
			EnforceGate:    true,
			NoValidate:     true,
			Engine:         makeClassifyEngine([]string{"G-005"}),
			Situation:      "Testing auto-goal assignment at create time for I-904.",
			Background:     "No explicit --goals flag was supplied by the operator or calling agent in this scenario.",
			Assessment:     "Classifier LLM should detect G-005 (st improvements) from the title.",
			Recommendation: "Auto-assign G-005 and print a hint to stderr so the operator can override.",
		})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	if !strings.Contains(stderr, "auto-assigned goal") {
		t.Errorf("expected auto-assign hint in stderr; got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "G-005") {
		t.Errorf("expected G-005 in auto-assign hint; got:\n%s", stderr)
	}

	// Verify item has the goal.
	all := s.List()
	var created *struct{ Goals []string }
	for _, it := range all {
		if it.Type == "issue" {
			created = &struct{ Goals []string }{Goals: it.Goals}
			break
		}
	}
	if created == nil {
		t.Fatal("item was not created")
	}
	if len(created.Goals) != 1 || created.Goals[0] != "G-005" {
		t.Errorf("expected item.Goals = [G-005], got %v", created.Goals)
	}
}

// TestCreate_WarnOnNoMatch: classify returns [] → item created, warning printed.
func TestCreate_WarnOnNoMatch(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	t.Setenv("AS_INTERNAL_NO_DEDUP", "1")
	t.Setenv("AS_INTERNAL_NO_CLASSIFY", "0")
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)

	stderr := captureStderrStr(t, func() {
		code := Create(s, cfg, "issue", "unclassifiable item", CreateOpts{
			Priority:       2,
			EnforceGate:    true,
			NoValidate:     true,
			Engine:         makeClassifyEngine([]string{}), // no match
			Situation:      "Something entirely unrelated to any active goal in the system.",
			Background:     "This item is for testing the no-match warning path; it spans no known goal domain.",
			Assessment:     "The classifier correctly returns an empty goal_ids list for this scenario.",
			Recommendation: "Item should be created with a warning; operator can add a goal link later.",
		})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	if !strings.Contains(stderr, "no active goal matched") {
		t.Errorf("expected no-match warning in stderr; got:\n%s", stderr)
	}
}

// TestCreate_ExplicitGoalsSkipsClassify: --goals supplied → classify never called.
func TestCreate_ExplicitGoalsSkipsClassify(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	t.Setenv("AS_INTERNAL_NO_DEDUP", "1")
	t.Setenv("AS_INTERNAL_NO_CLASSIFY", "0")
	s, cfg := newClassifyEnv(t)
	writeGoalFixtureClassify(t, cfg, "G-005", "st improvements", "active")
	s = freshStore(t, cfg)

	called := false
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			called = true
			inner, _ := json.Marshal(classifyVerdict{GoalIDs: []string{"G-005"}})
			data, _ := json.Marshal(ClaudeResult{Type: "result", Subtype: "success", Result: string(inner)})
			return data, 0, nil
		},
	}

	stderr := captureStderrStr(t, func() {
		code := Create(s, cfg, "issue", "explicit goal item", CreateOpts{
			Priority:       2,
			EnforceGate:    true,
			NoValidate:     true,
			Goals:          []string{"G-005"},
			Engine:         engine,
			Situation:      "Explicit goal G-005 was supplied via the --goals flag by the operator.",
			Background:     "This test verifies the classifier is never called when goals are explicitly provided.",
			Assessment:     "The classify LLM call should be skipped entirely — goals list is already populated.",
			Recommendation: "Assert no RunClaude call happens; item gets G-005 from the explicit flag.",
		})
		if code != 0 {
			t.Errorf("expected exit 0, got %d", code)
		}
	})

	// Classify LLM should NOT have been called (dedup and review are also
	// suppressed, so any RunClaude invocation must be the classifier).
	if called {
		t.Error("classify engine called even though --goals was supplied explicitly")
	}
	if strings.Contains(stderr, "auto-assigned goal") {
		t.Errorf("unexpected auto-assign hint when goals were explicit; got:\n%s", stderr)
	}
}
