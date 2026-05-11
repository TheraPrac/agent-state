package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
	"golang.org/x/term"
)

// runItemReview spawns a Claude sub-agent that critiques a freshly-created
// task or issue across TITLE and the four SBAR sub-fields, auto-fixing weak
// content via `st update` calls before returning a single verdict.
//
// I-588: this replaces the warning-only `quality.PrintWarnings(…)` nudge at
// the bottom of `st create`. The warning was ignored in practice because
// nothing downstream blocks on it; this active sub-agent does the work the
// author skipped instead of asking them to do it.
//
// Mirrors the plan-review loop at run.go:3042 — same max-iterations cap on
// "Accept with notes" auto-fix, same Accept/Reject/Feedback gate menu, same
// extractRecommendation / extractNotesFromReview helpers.
//
// Failure is non-fatal: a claude error or missing engine prints a stderr
// warning and returns. The new item is already on disk and a follow-up
// `st update <id> sbar` is always available.
func runItemReview(s *store.Store, cfg *config.Config, itemID string, item *model.Item, engine RunEngine) {
	if item == nil {
		return
	}
	if item.Type != "task" && item.Type != "issue" {
		return
	}
	// Engine wiring is opt-in — nil engine means no review (tests,
	// migrations, in-process invocations from other commands all hit this
	// path and should continue to work as before). The CLI entry point at
	// cmd/as/app.go always sets engine to DefaultRunEngine() so interactive
	// `st create` runs the review.
	if engine.RunClaude == nil {
		return
	}
	// AS_INTERNAL_NO_REVIEW=1 lets the test harness disable review even
	// when an engine is wired (e.g., test-only callers that pass a real
	// engine for some side effect but don't want the review subprocess).
	// This is NOT a public flag — there is no `--no-review` opt-out per
	// I-588's "every interactive create gets reviewed" rule.
	if os.Getenv("AS_INTERNAL_NO_REVIEW") == "1" {
		return
	}
	// Non-interactive contexts (piped stdin, CI runners, in-process test
	// harnesses) skip the review for the same reason `st create --editor`
	// skips $EDITOR on a non-TTY: the gate prompts for operator input
	// (Accept/Reject/Feedback) and an empty stdin would hang the process.
	// Tests that exercise the review path inject a mock engine + mock
	// SelectMenu, which short-circuits this check by not going through the
	// CLI entry point — they call command.Create() directly with a fake
	// engine, and the fake's SelectMenu replaces the TTY-bound default.
	// The `term.IsTerminal` guard is applied AFTER the engine.RunClaude
	// nil check above so test engines with a real SelectMenu still run.
	if engine.SelectMenu == nil && !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}

	autoFixCount := 0
	for iteration := 1; ; iteration++ {
		// Reload in case prior fixes changed the item.
		s2, err := store.New(cfg)
		if err == nil {
			if reloaded, ok := s2.Get(itemID); ok {
				item = reloaded
				s = s2
			}
		}

		reviewPrompt := buildItemReviewPrompt(itemID, item)
		reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
		reviewStep.SetName("create_review")
		reviewStart := time.Now()
		// I-588: opts/worktreeDir/claudeSessionID are zero-value — `st
		// create` runs from the working directory and doesn't carry a
		// resume session. `isResume=false` mints a fresh subprocess each
		// iteration so the prompt window stays small.
		reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, RunOpts{}, engine, "", "", false)
		reviewDur := time.Since(reviewStart)

		if reviewSR.Error != "" {
			fmt.Fprintf(os.Stderr, "warning: SBAR review failed for %s: %s\n", itemID, reviewSR.Error)
			return
		}

		rec := extractRecommendation(reviewSR.FullOutput)

		// Auto-fix "Accept with notes" by feeding the notes back to claude
		// without operator input. Same cap as plan-review.
		if isAcceptWithNotes(rec) && autoFixCount < maxAutoFixIterations {
			autoFixCount++
			fmt.Printf("[%s] Item review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
				itemID, autoFixCount, maxAutoFixIterations)
			notes := extractNotesFromReview(reviewSR.FullOutput)
			s3, err := store.New(cfg)
			if err == nil {
				if reloaded, ok := s3.Get(itemID); ok {
					var sr StepResult
					runAutoFixFromNotes(s3, cfg, itemID, "", reloaded, "item review", notes, RunOpts{}, engine, "", "", &sr)
				}
			}
			continue
		}

		choice := showReviewGate(ReviewGateInfo{
			ItemID:         itemID,
			Title:          item.Title,
			GateType:       "Item Review",
			Iteration:      iteration,
			Recommendation: rec,
			ReviewDuration: reviewDur,
		}, []menuOption{
			{"1", "Accept   — keep the item and proceed"},
			{"2", "Reject   — archive the item (abandon)"},
			{"3", "Feedback — type direction, claude revises (constrained)"},
		}, engine)

		switch choice {
		case "^C":
			fmt.Fprintf(os.Stderr, "warning: SBAR review interrupted for %s — item retained as-is\n", itemID)
			return
		case "1":
			return // accepted, item stays
		case "2":
			archiveAbandonedItem(s, cfg, itemID, rec)
			return
		case "3":
			// Constrained feedback: operator types direction, claude
			// revises. Loop continues so the revised item is re-reviewed.
			var sr StepResult
			runConstrainedFeedback(s, cfg, itemID, "", item, "item review", RunOpts{}, engine, "", "", &sr)
			continue
		default:
			// Unknown choice (test harness, race, etc.) — treat as Accept
			// so the item is not silently destroyed.
			return
		}
	}
}

// archiveAbandonedItem closes a freshly-created item that the review judged
// fundamentally a non-item. Uses `command.Close` so the standard close path
// (changelog entry, archive move, queue cleanup) runs uniformly.
//
// I-588: a Reject verdict is rare — most weak items get auto-fixed. When it
// fires, we want the abandonment to be visible in the changelog with the
// recommendation as the reason so a later audit can reconstruct why the
// item disappeared.
func archiveAbandonedItem(s *store.Store, cfg *config.Config, itemID, recommendation string) {
	reason := recommendation
	if reason == "" {
		reason = "I-588 item review: archived as non-item"
	}
	// Force=true: tier-1 test gates don't apply to a brand-new item being
	// abandoned at creation time, so we don't want the gate enforcement
	// path to refuse the close.
	code := Close(s, cfg, itemID, "abandoned", CloseOpts{Reason: reason, Force: true})
	if code != 0 {
		fmt.Fprintf(os.Stderr, "warning: archive of %s after review-reject returned %d — item remains in place\n", itemID, code)
	}
}
