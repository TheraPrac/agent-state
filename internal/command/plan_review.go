package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// maxPlanReviewAutoFixIterations bounds the Accept-with-notes auto-fix
// loop. Two passes converges on the vast majority of weak-content
// findings in the I-588 SBAR review precedent; beyond two iterations
// the model usually loops on the same edits.
const maxPlanReviewAutoFixIterations = 2

// defaultPlanReviewWallTimeout is the total wall cap for the plan-review
// sub-agent. I-738 hung 53min on the global 2h ceiling; I-810 raised the
// cap from 10m to 25m after referendum-style plans (I-705..I-709) routinely
// exceeded 10m without being hung. 25m absorbs the referendum long tail
// while still bounding the I-738-class hang.
// Operator override: AS_PLAN_APPROVE_TIMEOUT (duration string).
const defaultPlanReviewWallTimeout = 25 * time.Minute

// planReviewWrapUpBudget is the time reserved at the end of the wall cap
// for a drive-to-conclusion wrap-up pass. The first execution runs under
// wallCap−planReviewWrapUpBudget; on wall-time hit the session is resumed
// with a terse "emit your verdict NOW" prompt capped at this budget.
// I-810: prevents a hard kill from discarding all in-progress analysis.
const planReviewWrapUpBudget = 90 * time.Second

// resolvePlanReviewTimeout reads AS_PLAN_APPROVE_TIMEOUT (a Go duration
// string) and falls back to defaultPlanReviewWallTimeout. On parse error
// it logs to stderr and uses the default — a typo must never silently
// raise the cap back toward the 2h global ceiling.
func resolvePlanReviewTimeout() time.Duration {
	raw := os.Getenv("AS_PLAN_APPROVE_TIMEOUT")
	if raw == "" {
		return defaultPlanReviewWallTimeout
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"AS_PLAN_APPROVE_TIMEOUT=%q: %v — falling back to %s\n",
			raw, err, defaultPlanReviewWallTimeout)
		return defaultPlanReviewWallTimeout
	}
	return parsed
}

// runPlanReview spawns a Claude sub-agent to critically review the
// plan sidecar for `id`, auto-fixing weak content via `st update`
// heredocs when the verdict is "Accept with notes". Returns:
//
//	0  → review accepted (with or without notes after auto-fix);
//	     PlanApprove should proceed to the validator gates.
//	2  → review rejected, paused for feedback, or engine error.
//	     PlanApprove should refuse approval; the operator/agent
//	     needs to redraft the plan or run `st plan reset` + re-prep.
//
// The fail-closed posture on engine error is deliberate — the gate
// is load-bearing for the plan-before-code hook, and an opaque LLM
// failure should not silently waive the substance check.
//
// I-710 — mirrors the I-588 `runItemReview` shape for SBAR creation
// reviews. The richer Accept/Reject/Feedback/Interactive/Split menu
// in `prepItem`/`prepItemWriteOnly` stays where it is; this helper
// is the focused approval-time review, not a refactor of prep.
func runPlanReview(s *store.Store, cfg *config.Config, id string, item *model.Item, engine RunEngine) int {
	if engine.RunClaude == nil {
		// No engine → silent skip, matching the I-588 test-path
		// invariant.
		return 0
	}

	// I-992: skip the duplicate LLM sub-agent when the plan was already
	// reviewed by prepItem or prepItemWriteOnly (prep_reviewed_at stamp
	// present). The static validator gates in PlanApprove still run
	// unconditionally; only this sub-agent is elided.
	if p, err := plan.Load(cfg.PlansDir(), id); err == nil && p != nil && p.PrepReviewedAt != "" {
		fmt.Fprintf(os.Stderr, "%s: plan already reviewed during prep (prep_reviewed_at: %s) — skipping sub-agent\n", id, p.PrepReviewedAt)
		return 0
	}

	cwd := cfg.Root()
	wallCap := resolvePlanReviewTimeout()

	// I-810: split the wall cap into a first-pass budget and a wrap-up
	// reserve. If the cap is too small to split safely (≤ 2× wrapUpBudget,
	// e.g. a tiny AS_PLAN_APPROVE_TIMEOUT override), run the full cap on the
	// first pass and skip the wrap-up entirely — strictly no worse than
	// the pre-I-810 behavior.
	firstPassCap := wallCap - planReviewWrapUpBudget
	wrapEnabled := wallCap > 2*planReviewWrapUpBudget
	if !wrapEnabled {
		firstPassCap = wallCap
	}

	// reviewSessionID is generated once and threaded through all executions
	// so the wrap-up pass can resume the same Claude session via --resume.
	reviewSessionID := generateSessionID()

	// lastNotes holds the most-recent "Accept with notes" body so the
	// post-loop path can persist it even after falling out of the loop.
	var lastNotes string

	// Loop bound is strictly less-than so the final iteration that
	// fails to auto-fix falls out into the post-loop fail-closed
	// path. The Accept/Reject/catch-all branches return inline;
	// only the Accept-with-notes branch continues the loop.
	for iter := 0; iter < maxPlanReviewAutoFixIterations; iter++ {
		// Build the prompt and execute claude. I-752: inject the wall
		// cap via ExtraEnv so defaultRunClaude tightens its ceiling
		// from the 2h global to the plan-review-specific cap.
		// I-810: use firstPassCap (wallCap − wrapUpBudget) so that a
		// wall-time hit leaves room for the wrap-up resume below.
		prompt := buildPlanReviewPrompt(id, item)
		step := config.RunStepDef{
			Type:     "claude",
			Prompt:   prompt,
			ExtraEnv: []string{"AS_CLAUDE_WALL_TIMEOUT=" + firstPassCap.String()},
		}
		step.SetName("plan_review_approve")
		sr := executeClaude(s, cfg, id, "", step, RunOpts{}, engine, cwd, reviewSessionID, false)

		if !sr.Passed && sr.FullOutput == "" {
			if !strings.Contains(sr.Error, "wall time limit") {
				fmt.Fprintf(os.Stderr,
					"%s: plan-review sub-agent failed (%s) — refusing approval. Re-run `st plan approve %s` to retry, or `st plan reset %s` to redraft.\n",
					id, sr.Error, id, id)
				return 2
			}
			// I-810: drive-to-conclusion wrap-up. Resume the same Claude
			// session with a terse "emit verdict NOW" prompt so the operator
			// gets a real decision rather than a hard-kill with zero output.
			if wrapEnabled {
				wrapStep := config.RunStepDef{
					Type:     "claude",
					Prompt:   buildPlanReviewWrapUpPrompt(id, planReviewWrapUpBudget),
					ExtraEnv: []string{"AS_CLAUDE_WALL_TIMEOUT=" + planReviewWrapUpBudget.String()},
				}
				wrapStep.SetName("plan_review_wrapup")
				wrapSR := executeClaude(s, cfg, id, "", wrapStep, RunOpts{}, engine, cwd, reviewSessionID, true)
				// Accept wrap-up output only when the sub-agent exited cleanly
				// (Passed=true). A non-zero exit may still carry non-empty
				// FullOutput (e.g. error_during_execution with a result field);
				// treating that as a verdict would approve the plan on garbage.
				if wrapSR.Passed && wrapSR.FullOutput != "" {
					fmt.Fprintf(os.Stderr,
						"%s: plan review first pass timed out — wrap-up verdict captured (partial analysis)\n", id)
					sr = wrapSR
					// sr.FullOutput is now valid; fall through to rec parsing.
				} else {
					fmt.Fprintf(os.Stderr,
						"%s: plan review timed out after %s — refusing approval. Re-run, set AS_PLAN_APPROVE_TIMEOUT=<longer>, or pass --bypass-review.\n",
						id, wallCap)
					return 2
				}
			} else {
				fmt.Fprintf(os.Stderr,
					"%s: plan review timed out after %s — refusing approval. Re-run, set AS_PLAN_APPROVE_TIMEOUT=<longer>, or pass --bypass-review.\n",
					id, wallCap)
				return 2
			}
		}

		rec := extractRecommendation(sr.FullOutput)
		lowered := strings.ToLower(rec)

		// Accept (clean) → review passes.
		if strings.Contains(lowered, "accept") && !isAcceptWithNotes(rec) {
			fmt.Fprintf(os.Stderr, "%s: plan review accepted by sub-agent\n", id)
			return 0
		}

		// Accept with notes → auto-fix and re-run. Reject the
		// approval if the auto-fix engine call itself fails (any
		// non-Passed StepResult), so an opaque LLM/store failure
		// does not silently waive the gate.
		// I-985: pass the wall cap into the auto-fix subprocess so it
		// shares the same ceiling as the review step.
		if isAcceptWithNotes(rec) {
			fmt.Fprintf(os.Stderr,
				"%s: plan review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
				id, iter+1, maxPlanReviewAutoFixIterations)
			notes := extractNotesFromReview(sr.FullOutput)
			lastNotes = notes
			var fixResult StepResult
			autoFixEnv := []string{"AS_CLAUDE_WALL_TIMEOUT=" + wallCap.String()}
			runAutoFixFromNotes(s, cfg, id, "", item, "plan review", notes, RunOpts{}, engine, cwd, "", autoFixEnv, &fixResult)
			if !fixResult.Passed && fixResult.Error != "" {
				fmt.Fprintf(os.Stderr,
					"%s: plan-review auto-fix failed (%s) — refusing approval. Re-run `st plan approve %s` to retry, or `st plan reset %s` to redraft.\n",
					id, fixResult.Error, id, id)
				return 2
			}
			// Reload item — auto-fix may have mutated fields.
			if refreshed, ok := s.Get(id); ok {
				item = refreshed
			}
			continue
		}

		// Reject → refuse approval, point operator at reset.
		if strings.Contains(lowered, "reject") {
			fmt.Fprintf(os.Stderr,
				"%s: plan review REJECTED by sub-agent — refusing approval.\n  verdict: %s\nRun `st plan reset %s` and redraft via `st prep %s` (or `st plan prep %s` after T-376 lands).\n",
				id, rec, id, id, id)
			return 2
		}

		// Feedback / Interactive / unknown → pause path; refuse
		// approval and instruct the operator to engage interactively.
		fmt.Fprintf(os.Stderr,
			"%s: plan review needs human input (verdict: %s) — refusing approval. Run `st prep %s` interactively to resolve.\n",
			id, rec, id)
		return 2
	}

	// Exhausted auto-fix iterations without converging on a clean
	// Accept — the reviewer returned "Accept with notes" every pass.
	// "Accept with notes" means the plan was fundamentally accepted;
	// the remaining notes are advisory, not blocking. Accept, but
	// persist the notes to the plan sidecar so they survive the session.
	// I-985: failing closed here was incorrect — a plan the reviewer
	// called "Accept" (with suggestions) must not be silently rejected
	// after N fix attempts.
	if lastNotes != "" {
		if err := appendPendingReviewNotes(cfg.PlansDir(), id, lastNotes); err != nil {
			fmt.Fprintf(os.Stderr,
				"%s: plan review 'Accept with notes' notes remain after %d auto-fix pass(es) — accepting as advisory. Warning: could not persist notes to .plans/%s.md (%v) — copy notes above manually. Redraft via 'st plan prep %s' if improvements are needed.\n",
				id, maxPlanReviewAutoFixIterations, id, err, id)
		} else {
			fmt.Fprintf(os.Stderr,
				"%s: plan review 'Accept with notes' notes remain after %d auto-fix pass(es) — accepting as advisory. Notes appended to .plans/%s.md — redraft via 'st plan prep %s' if improvements are needed.\n",
				id, maxPlanReviewAutoFixIterations, id, id)
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"%s: plan review 'Accept with notes' notes remain after %d auto-fix pass(es) — accepting as advisory.\n",
			id, maxPlanReviewAutoFixIterations)
	}
	return 0
}

// appendPendingReviewNotes appends a "## Pending Review Notes" section to the
// plan sidecar so advisory notes from an exhausted auto-fix loop are findable
// via `st plan show` in future sessions rather than lost to terminal scrollback.
// Idempotent: a pre-existing section is replaced rather than duplicated.
func appendPendingReviewNotes(plansDir, id, notes string) error {
	planPath := filepath.Join(plansDir, id+".md")
	existing, err := os.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("could not read plan sidecar: %w", err)
	}
	section := "\n## Pending Review Notes\n\n" + strings.TrimSpace(notes) + "\n"
	// Strip any prior "## Pending Review Notes" section so re-runs don't accumulate duplicates.
	body := string(existing)
	if idx := strings.Index(body, "\n## Pending Review Notes"); idx >= 0 {
		body = body[:idx]
	}
	if err := os.WriteFile(planPath, []byte(body+section), 0644); err != nil {
		return fmt.Errorf("could not write plan sidecar: %w", err)
	}
	return nil
}
