package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/quality"
	"github.com/theraprac/agent-state/internal/store"
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
// Outcomes:
//   - Accept (operator menu or agent-mode shortcut) — item kept as-is.
//   - Accept with notes — sub-agent auto-fixes via `st update`, then re-reviews.
//   - Reject (operator menu "2" or agent-mode shortcut) — DESTRUCTIVE: item
//     is closed and moved to `agent-state/archive/` via `archiveAbandonedItem`
//     with `status: abandoned`. In agent mode this fires without operator
//     confirmation; in operator mode the operator selected option "2".
//   - Feedback (operator menu "3" only) — operator types direction, sub-agent
//     revises in a constrained-feedback loop.
//   - Ambiguous verdict in agent mode — item kept (no operator to consult; do
//     not risk a destructive Reject on a non-explicit verdict).
//
// Failure is non-fatal: a claude error or missing engine prints a stderr
// warning and returns without touching the item.
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
	// I-758: detect agent context via the CLAUDECODE=1 env var Claude
	// Code sets in every tool subprocess. Previously this function
	// silently returned on any non-TTY context (line below), which
	// meant every agent-spawned `st create` shipped its item with TODO
	// scaffolds — the I-588 review never fired. Filed after the
	// 2026-05-21 incident where I-731/I-732/I-733 sat with scaffold
	// SBAR for 17 hours after creation.
	//
	// In agent mode the review runs but the operator-input menu is
	// short-circuited deterministically by the sub-agent's
	// recommendation: Accept → keep; Reject → archive; Feedback /
	// unknown → keep (a no-op rather than indefinite hang). The
	// auto-Accept-with-notes loop is unchanged.
	//
	// Operator-TTY behavior (the original skip) is unchanged for
	// genuine pipe-into-st-create contexts: tests, CI runners, and
	// in-process harnesses that aren't tagged CLAUDECODE=1 still skip.
	// Truthy match instead of `== "1"` so a future Claude Code release
	// that ships e.g. `CLAUDECODE=2` (or a version string) still routes
	// agent-spawned creates through the review. The risk of a strict
	// equality check is silent regression: the agent-mode branch would
	// stop firing, items would resume shipping with TODO scaffold SBAR,
	// and the only signal would be I-589's plan-approve gate catching
	// them hours later. Code-review finding on PR #155.
	isAgent := os.Getenv("CLAUDECODE") != ""
	if engine.SelectMenu == nil && !term.IsTerminal(int(os.Stdin.Fd())) && !isAgent {
		return
	}

	autoFixCount := 0
	for iteration := 1; ; iteration++ {
		// Reload in case prior fixes changed the item.
		s2, err := store.New(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: [%s] create_review: store reload failed (iteration %d) — using stale item: %v\n", itemID, iteration, err)
		} else if reloaded, ok := s2.Get(itemID); !ok {
			fmt.Fprintf(os.Stderr, "warning: [%s] create_review: item not found in reloaded store (iteration %d) — using stale item\n", itemID, iteration)
		} else {
			item = reloaded
			s = s2
		}

		reviewPrompt := buildItemReviewPrompt(itemID, item)
		reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
		reviewStep.SetName("create_review")
		reviewStart := time.Now()
		// I-588: opts/worktreeDir/claudeSessionID are zero-value — `st
		// create` runs from the working directory and doesn't carry a
		// resume session. `isResume=false` mints a fresh subprocess each
		// iteration so the prompt window stays small.
		// I-1612: use the validation model (haiku by default) instead of the
		// run model so the review subprocess is lighter. resolveAPIModelID
		// also normalises short names ("haiku" → "claude-haiku-4-5") so the
		// claude CLI receives a full canonical ID regardless of what the
		// operator wrote in quality.validation_model.
		reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, RunOpts{Model: resolveAPIModelID(cfg.ValidationModel())}, engine, "", "", false)
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
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] create_review: auto-fix store reload failed — SBAR update skipped: %v\n", itemID, err)
			} else if reloaded, ok := s3.Get(itemID); !ok {
				fmt.Fprintf(os.Stderr, "warning: [%s] create_review: auto-fix item not found in reloaded store — SBAR update skipped\n", itemID)
			} else {
				var sr StepResult
				runAutoFixFromNotes(s3, cfg, itemID, "", reloaded, "item review", notes, RunOpts{}, engine, "", "", nil, &sr)
				if sr.Error != "" {
					fmt.Fprintf(os.Stderr, "warning: [%s] create_review: auto-fix subprocess failed — SBAR may not have been updated: %s\n", itemID, sr.Error)
				}
			}
			continue
		}

		// I-758: agent context with no SelectMenu wired — convert the
		// sub-agent's recommendation into a deterministic choice without
		// prompting (the TTY menu would hang on empty stdin).
		// Accept-with-notes was already auto-fixed above; remaining
		// cases are Accept / Reject / Feedback-or-unknown.
		//
		// Tests that wire engine.SelectMenu still go through
		// showReviewGate even when CLAUDECODE=1 is inherited from a
		// Claude Code parent — the agent-mode shortcut is the
		// "no operator-input mechanism available" fallback, not a
		// CLAUDECODE-only override.
		var choice string
		if isAgent && engine.SelectMenu == nil {
			loweredRec := strings.ToLower(rec)
			switch {
			case strings.Contains(loweredRec, "reject"):
				choice = "2"
				fmt.Fprintf(os.Stderr, "[%s] agent-mode item review: Reject verdict — auto-archiving\n", itemID)
			case strings.Contains(loweredRec, "accept"):
				choice = "1"
				fmt.Fprintf(os.Stderr, "[%s] agent-mode item review: Accept — keeping item\n", itemID)
			default:
				// Feedback or unknown recommendation in agent mode: keep
				// the item rather than risk a destructive Reject on an
				// ambiguous verdict. The Accept-with-notes auto-fix loop
				// above already burned its iterations; any further
				// refinement is an operator-side decision.
				choice = "1"
				fmt.Fprintf(os.Stderr, "[%s] agent-mode item review: ambiguous verdict (%q) — keeping item (no operator to consult)\n", itemID, rec)
			}
		} else {
			choice = showReviewGate(ReviewGateInfo{
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
		}

		switch choice {
		case "^C":
			fmt.Fprintf(os.Stderr, "warning: SBAR review interrupted for %s — item retained as-is\n", itemID)
			return
		case "1":
			warnIfScaffoldSBAR(cfg, itemID)
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

// warnIfScaffoldSBAR reloads the item and checks whether any SBAR sub-field
// is still on the TODO scaffold (or empty). If so, it emits a prominent
// warning to stderr so the operator knows the review accepted without filling
// in real content, and names the manual recovery path.
func warnIfScaffoldSBAR(cfg *config.Config, itemID string) {
	s2, err := store.New(cfg)
	if err != nil {
		return
	}
	current, ok := s2.Get(itemID)
	if !ok {
		return
	}
	violations := quality.ValidateSBAR(current)
	if len(violations) == 0 {
		return
	}
	var fields []string
	for _, v := range violations {
		fields = append(fields, v.Field)
	}
	fmt.Fprintf(os.Stderr,
		"warning: [%s] post-accept SBAR check: %s still on scaffold — run: st update %s sbar --stdin\n",
		itemID, strings.Join(fields, ", "), itemID)
}

// archiveAbandonedItem closes a freshly-created item that the review judged
// fundamentally a non-item. Uses `command.Close` so the standard close path
// (changelog entry, archive move, queue cleanup) runs uniformly.
//
// I-588: a Reject verdict is rare — most weak items get auto-fixed. When it
// fires, we want the abandonment to be visible in the changelog with the
// recommendation as the reason so a later audit can reconstruct why the
// item disappeared.
func archiveAbandonedItem(s *store.Store, cfg *config.Config, itemID, _ string) {
	// Force=true: tier-1 test gates don't apply to a brand-new item being
	// abandoned at creation time, so we don't want the gate enforcement
	// path to refuse the close.
	code := Close(s, cfg, itemID, "abandoned", CloseOpts{Reason: "unactionable", Force: true})
	if code != 0 {
		fmt.Fprintf(os.Stderr, "warning: archive of %s after review-reject returned %d — item remains in place\n", itemID, code)
	}
}
