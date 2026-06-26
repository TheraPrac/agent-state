package command

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// fakeClaude builds a fake RunClaude that returns successive canned
// recommendations from the supplied scripts (one entry per call).
// When calls exceed the script length, the final entry is reused so a
// test can simulate an unlimited Accept tail without padding.
type fakeClaude struct {
	mu          sync.Mutex
	calls       int32
	stepResults []string // canned report bodies, indexed by call number
	errOnCall   int      // 1-indexed: return error on this call (0 = never)
	exitOnCall  int      // 1-indexed: return non-zero exit on this call
}

func (f *fakeClaude) run(cwd string, args []string, env []string) ([]byte, int, error) {
	n := int(atomic.AddInt32(&f.calls, 1))
	if f.errOnCall != 0 && n == f.errOnCall {
		return nil, 1, errors.New("simulated claude exec error")
	}
	if f.exitOnCall != 0 && n == f.exitOnCall {
		return nil, 1, nil
	}
	idx := n - 1
	if idx >= len(f.stepResults) {
		idx = len(f.stepResults) - 1
	}
	body := ""
	if idx >= 0 {
		body = f.stepResults[idx]
	}
	result := ClaudeResult{
		Type: "result", Subtype: "success",
		Result: body,
	}
	data, _ := json.Marshal(result)
	return data, 0, nil
}

// engineWithFake returns a RunEngine that delegates RunClaude to the
// supplied fake and stubs PromptUser / SelectMenu to deterministic
// values so showReviewGate doesn't block waiting for stdin.
func engineWithFake(f *fakeClaude, promptReply string, menuChoice string) RunEngine {
	return RunEngine{
		RunClaude: f.run,
		PromptUser: func(prompt string) (string, error) {
			return promptReply, nil
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			return menuChoice
		},
	}
}

// suppressOutput silences both stdout and stderr for the duration of
// fn. runItemReview's progress prints would otherwise drown test
// output; we don't care what was printed, only what was changed.
func suppressOutput(t *testing.T, fn func()) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rO, wO, _ := os.Pipe()
	rE, wE, _ := os.Pipe()
	os.Stdout, os.Stderr = wO, wE
	doneOut, doneErr := make(chan struct{}), make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, rO); close(doneOut) }()
	go func() { _, _ = io.Copy(io.Discard, rE); close(doneErr) }()
	defer func() {
		wO.Close()
		wE.Close()
		<-doneOut
		<-doneErr
		os.Stdout, os.Stderr = origOut, origErr
	}()
	fn()
}

// TestBuildItemReviewPrompt covers the prompt body contract: the
// review prompt must include the item ID, title, all four SBAR
// sub-fields, and the Accept/Reject/Feedback recommendation menu so a
// claude sub-agent can apply the same rules as `st prep`'s plan
// review.
func TestBuildItemReviewPrompt(t *testing.T) {
	item := &model.Item{
		ID:    "T-100",
		Type:  "task",
		Title: "Sample task title",
		SBAR: model.SBAR{
			Situation:      "observable symptom",
			Background:     "prior context",
			Assessment:     "diagnosis here",
			Recommendation: "proposed fix here",
		},
	}
	out := buildItemReviewPrompt("T-100", item)

	mustContain := []string{
		"T-100",
		"Sample task title",
		"observable symptom",
		"prior context",
		"diagnosis here",
		"proposed fix here",
		"st update T-100 title",
		"st update T-100 sbar.",
		"--stdin",
		"\"Accept\"",
		"\"Reject\"",
		"\"Feedback\"",
		"Do NOT use \"Accept with notes\"",
		"TITLE",
		"SBAR.situation",
		"SBAR.background",
		"SBAR.assessment",
		"SBAR.recommendation",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, out)
		}
	}
}

// TestBuildItemReviewPrompt_EmptySBARMarkedExplicitly ensures empty
// SBAR sub-fields are surfaced to the reviewer as "(empty)" rather
// than as blank lines that get visually swallowed by the prompt's
// surrounding labels. Without this signal, the reviewer might miss
// that a field needs filling and skip the auto-fix.
func TestBuildItemReviewPrompt_EmptySBARMarkedExplicitly(t *testing.T) {
	item := &model.Item{
		ID:    "T-101",
		Type:  "task",
		Title: "Title with empty sbar",
		SBAR:  model.SBAR{}, // all empty
	}
	out := buildItemReviewPrompt("T-101", item)
	if !strings.Contains(out, "(empty)") {
		t.Errorf("prompt should mark empty SBAR fields as (empty); got:\n%s", out)
	}
}

// TestCreateRunsItemReview_TaskAndIssue covers the I-588 wiring: a
// task and an issue created via `st create` each trigger the sub-agent
// review (one RunClaude call per item). The fake returns Accept so
// showReviewGate auto-proceeds without needing a SelectMenu call.
func TestCreateRunsItemReview_TaskAndIssue(t *testing.T) {
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nAccept — substance is fine"}}
	engine := engineWithFake(f, "", "1")

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "New task with engine", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create task returned %d", code)
		}
		if code := Create(s, cfg, "issue", "New issue with engine", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create issue returned %d", code)
		}
	})

	if got := atomic.LoadInt32(&f.calls); got != 2 {
		t.Errorf("expected RunClaude to fire once per task+issue create; got %d", got)
	}
}

// TestCreateSkipsReview_IdeaPromotion: ideas and promotions never
// carry SBAR per the I-487 schema, so the review function must
// short-circuit on those types. We use the fake to assert RunClaude
// is never invoked.
func TestCreateSkipsReview_IdeaPromotion(t *testing.T) {
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nAccept"}}
	engine := engineWithFake(f, "", "1")

	suppressOutput(t, func() {
		// `idea` and `promotion` are present in the default config Types
		// map; if either is absent, Create returns a different error and
		// the assertion below still holds because RunClaude stayed at 0.
		_ = Create(s, cfg, "idea", "An idea", CreateOpts{Priority: 2, Engine: engine})
		_ = Create(s, cfg, "promotion", "A promotion", CreateOpts{Priority: 2, Engine: engine})
	})

	if got := atomic.LoadInt32(&f.calls); got != 0 {
		t.Errorf("expected zero RunClaude calls for idea/promotion creates; got %d", got)
	}
}

// TestCreateItemReview_RejectArchives: a Reject verdict closes the
// freshly-created item with resolution=abandoned. The closed item
// moves from tasks/ to archive/, so we read it back from the archive
// path to confirm.
func TestCreateItemReview_RejectArchives(t *testing.T) {
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nReject — duplicate of T-001"}}
	engine := engineWithFake(f, "", "2") // option 2 = Reject

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Doomed task", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})

	// Reload because Close moves the item file.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	item, ok := s2.Get("T-005") // next id after the four fixture items
	if !ok {
		t.Fatal("expected T-005 to exist post-Reject (closed to archive, not deleted)")
	}
	if item.Status != "abandoned" {
		t.Errorf("expected status=abandoned after review-reject; got %q", item.Status)
	}
}

// TestCreateItemReview_AutoFixFromNotes: an "Accept with notes" verdict
// triggers the auto-fix loop, which spawns a second claude call to fix
// the noted issues, then re-runs the review. We script the fake to
// return Accept-with-notes on call 1, plain Accept on call 3 (call 2
// is the auto-fix subprocess). The total RunClaude calls should be 3:
// review → auto-fix → re-review.
func TestCreateItemReview_AutoFixFromNotes(t *testing.T) {
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{
		stepResults: []string{
			"## REMAINING CONCERNS\n- title is vague\n## RECOMMENDATION\nAccept with notes — fix the title",
			"fixed the title",
			"## RECOMMENDATION\nAccept — clean now",
		},
	}
	engine := engineWithFake(f, "", "1")

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Initial vague title", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})

	if got := atomic.LoadInt32(&f.calls); got != 3 {
		t.Errorf("expected 3 RunClaude calls (review→auto-fix→re-review); got %d", got)
	}
}

// TestCreateItemReview_FeedbackLoops: when claude returns "Feedback"
// (a verdict that requires operator input), the gate prompts the
// operator for direction. If the operator returns empty (cancel via
// EOF / empty input), runConstrainedFeedback no-ops and the review
// loop continues to the next iteration. We assert the gate fires at
// least once with the Feedback recommendation surfaced, and the loop
// doesn't lock up.
func TestCreateItemReview_FeedbackLoops(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Call 1: Feedback. Call 2+: Accept so the loop terminates.
	f := &fakeClaude{
		stepResults: []string{
			"## RECOMMENDATION\nFeedback — operator: is this a duplicate of T-001?",
			"## RECOMMENDATION\nAccept — operator confirmed not a dup",
		},
	}

	var gateSeen int32
	engine := RunEngine{
		RunClaude: f.run,
		PromptUser: func(prompt string) (string, error) {
			return "", nil // cancel feedback — review loop should continue
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			atomic.AddInt32(&gateSeen, 1)
			// First call: choose Feedback. Subsequent calls: Accept.
			if atomic.LoadInt32(&gateSeen) == 1 {
				return "3"
			}
			return "1"
		},
	}

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Maybe a dup", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})

	if atomic.LoadInt32(&gateSeen) < 1 {
		t.Errorf("expected at least one SelectMenu call (Feedback verdict blocks auto-proceed); got %d", gateSeen)
	}
	if got := atomic.LoadInt32(&f.calls); got < 2 {
		t.Errorf("expected at least 2 RunClaude calls (initial Feedback + post-cancel re-review); got %d", got)
	}
}

// TestCreateItemReview_NilEngineSkips: in-process callers (tests,
// migrations) leave CreateOpts.Engine zero, so the review function
// must short-circuit without launching any subprocess. This preserves
// the existing surface for the ~30 test sites that call Create()
// with the default zero CreateOpts.
func TestCreateItemReview_NilEngineSkips(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// We can't assert "no RunClaude call" without a fake — instead, we
	// rely on the fact that the default RunEngine{} has a nil RunClaude
	// and runItemReview returns immediately on that path. A successful
	// Create with no panic / no error proves the early return fires.
	code := Create(s, cfg, "task", "No-engine task", CreateOpts{Priority: 2})
	if code != 0 {
		t.Errorf("Create with zero Engine should return 0; got %d", code)
	}
}

// TestCreateItemReview_ClaudeErrorIsNonFatal: a claude subprocess
// error during review must not fail the create. The item is already
// on disk and a follow-up `st update <id> sbar` is always available.
// We use the fake to return an exec error on the first call; the
// create should still return 0 and the item should exist.
func TestCreateItemReview_ClaudeErrorIsNonFatal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{
		stepResults: []string{""},
		errOnCall:   1,
	}
	engine := engineWithFake(f, "", "1")

	var code int
	suppressOutput(t, func() {
		code = Create(s, cfg, "task", "Reviewer crashed", CreateOpts{Priority: 2, Engine: engine})
	})
	if code != 0 {
		t.Errorf("Create should return 0 even when reviewer fails; got %d", code)
	}
	// Item must still exist on disk.
	if _, ok := s.Get("T-005"); !ok {
		t.Error("item should remain on disk after reviewer crash")
	}
}

// TestCreateItemReview_InternalNoReviewEnvSkips: the
// AS_INTERNAL_NO_REVIEW=1 env var disables review even when an engine
// is wired. Not a public flag — this is for the test harness and for
// callers that pass a real engine for unrelated side effects.
func TestCreateItemReview_InternalNoReviewEnvSkips(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nAccept"}}
	engine := engineWithFake(f, "", "1")

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Env-skipped review", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})
	if got := atomic.LoadInt32(&f.calls); got != 0 {
		t.Errorf("AS_INTERNAL_NO_REVIEW=1 should skip review even with an engine wired; got %d calls", got)
	}
}

// TestCreateItemReview_AgentModeAcceptKeeps: I-758 — when CLAUDECODE=1
// is set (agent context), the review runs in non-interactive mode.
// On an Accept verdict the item is kept with no operator menu prompt.
// This is the path that previously silently skipped via the
// term.IsTerminal check, shipping items with TODO scaffolds.
func TestCreateItemReview_AgentModeAcceptKeeps(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nAccept"}}
	// Critical: SelectMenu is nil, simulating the agent-spawned
	// non-TTY case that used to silently skip the review entirely.
	engine := RunEngine{RunClaude: f.run}

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Agent-created task", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})
	if got := atomic.LoadInt32(&f.calls); got != 1 {
		t.Errorf("agent-mode Accept should call review exactly once; got %d", got)
	}
	// Item should remain (not archived).
	s2, _ := store.New(cfg)
	item, ok := s2.Get("T-005")
	if !ok {
		t.Fatal("expected T-005 to exist after agent-mode Accept")
	}
	if item.Status == "abandoned" {
		t.Errorf("agent-mode Accept should keep item; got status=%q", item.Status)
	}
}

// TestCreateItemReview_AgentModeRejectArchives: when CLAUDECODE=1 and
// the sub-agent recommends Reject, the item is auto-archived without
// an operator menu prompt — the agent-mode analog of the operator
// flow that hits option "2".
func TestCreateItemReview_AgentModeRejectArchives(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nReject — duplicate of T-001"}}
	engine := RunEngine{RunClaude: f.run}

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Agent-rejected task", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})
	s2, _ := store.New(cfg)
	item, ok := s2.Get("T-005")
	if !ok {
		t.Fatal("expected T-005 to exist after agent-mode Reject (closed to archive)")
	}
	if item.Status != "abandoned" {
		t.Errorf("agent-mode Reject should archive; got status=%q", item.Status)
	}
}

// TestCreateItemReview_AgentModeAmbiguousKeeps: an ambiguous verdict
// (Feedback / unknown / empty) in agent mode keeps the item rather
// than risking a destructive Reject — there's no operator to consult
// for clarification.
func TestCreateItemReview_AgentModeAmbiguousKeeps(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nFeedback — operator: please clarify scope"}}
	engine := RunEngine{RunClaude: f.run}

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Agent ambiguous task", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})
	s2, _ := store.New(cfg)
	item, ok := s2.Get("T-005")
	if !ok {
		t.Fatal("expected T-005 to exist after agent-mode ambiguous verdict")
	}
	if item.Status == "abandoned" {
		t.Errorf("agent-mode ambiguous verdict should keep item (no destructive Reject); got status=%q", item.Status)
	}
}

// TestCreateItemReview_NonAgentNonTTYStillSkips: the original
// non-TTY skip is preserved for non-agent contexts (genuine
// pipe-into-st-create from CI runners that don't set CLAUDECODE).
// The operative guard in create_review.go is the three-way
// conjunction `engine.SelectMenu == nil && !term.IsTerminal(stdin)
// && !isAgent`. All three must hold for the skip to fire:
// SelectMenu is nil (test wires RunClaude only), stdin is not a
// TTY (true under `go test`), and isAgent is false (CLAUDECODE
// cleared below). The review should NOT fire here — preserves the
// I-588 carve-out for piped contexts that would hang on the
// operator menu.
func TestCreateItemReview_NonAgentNonTTYStillSkips(t *testing.T) {
	// Explicitly clear CLAUDECODE in case the test harness inherits it.
	t.Setenv("CLAUDECODE", "")
	s, cfg := setupTestEnv(t)
	f := &fakeClaude{stepResults: []string{"## RECOMMENDATION\nAccept"}}
	engine := RunEngine{RunClaude: f.run} // SelectMenu nil → original skip path

	suppressOutput(t, func() {
		if code := Create(s, cfg, "task", "Non-agent piped task", CreateOpts{Priority: 2, Engine: engine}); code != 0 {
			t.Fatalf("Create returned %d", code)
		}
	})
	if got := atomic.LoadInt32(&f.calls); got != 0 {
		t.Errorf("non-agent non-TTY without SelectMenu should still skip review; got %d calls", got)
	}
}
