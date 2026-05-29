package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// maxPlanReviewAutoFixIterations bounds the Accept-with-notes auto-fix
// loop. Two passes converges on the vast majority of weak-content
// findings in the I-588 SBAR review precedent; beyond two iterations
// the model usually loops on the same edits.
const maxPlanReviewAutoFixIterations = 2

// defaultPlanReviewWallTimeout is the per-step wall cap injected into the
// plan-review sub-agent via AS_CLAUDE_WALL_TIMEOUT. I-738 hung 53min on the
// global 2h ceiling; observed normal runtime is 1–4 min (I-735/736/737).
// Operator override: AS_PLAN_APPROVE_TIMEOUT (duration string).
const defaultPlanReviewWallTimeout = 10 * time.Minute

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
	cwd := cfg.Root()
	wallCap := resolvePlanReviewTimeout()

	// Loop bound is strictly less-than so the final iteration that
	// fails to auto-fix falls out into the post-loop fail-closed
	// path. The Accept/Reject/catch-all branches return inline;
	// only the Accept-with-notes branch continues the loop.
	for iter := 0; iter < maxPlanReviewAutoFixIterations; iter++ {
		// Build the prompt and execute claude. I-752: inject the wall
		// cap via ExtraEnv so defaultRunClaude tightens its ceiling
		// from the 2h global to the plan-review-specific cap.
		prompt := buildPlanReviewPrompt(id, item)
		step := config.RunStepDef{
			Type:     "claude",
			Prompt:   prompt,
			ExtraEnv: []string{"AS_CLAUDE_WALL_TIMEOUT=" + wallCap.String()},
		}
		step.SetName("plan_review_approve")
		sr := executeClaude(s, cfg, id, "", step, RunOpts{}, engine, cwd, "", false)

		if !sr.Passed && sr.FullOutput == "" {
			// I-752: surface the wall-time case explicitly so the operator
			// knows to extend AS_PLAN_APPROVE_TIMEOUT or use --bypass-review,
			// rather than chasing a generic "sub-agent failed" message.
			if strings.Contains(sr.Error, "wall time limit") {
				fmt.Fprintf(os.Stderr,
					"%s: plan review timed out after %s — refusing approval. Re-run, set AS_PLAN_APPROVE_TIMEOUT=<longer>, or pass --bypass-review.\n",
					id, wallCap)
				return 2
			}
			fmt.Fprintf(os.Stderr,
				"%s: plan-review sub-agent failed (%s) — refusing approval. Re-run `st plan approve %s` to retry, or `st plan reset %s` to redraft.\n",
				id, sr.Error, id, id)
			return 2
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
	// the remaining notes are advisory, not blocking. Accept and log
	// the notes so the operator can act on them if they choose.
	// I-985: failing closed here was incorrect — a plan the reviewer
	// called "Accept" (with suggestions) must not be silently rejected
	// after N fix attempts.
	fmt.Fprintf(os.Stderr,
		"%s: plan review 'Accept with notes' notes remain after %d auto-fix pass(es) — accepting as advisory. Review notes above and redraft via 'st plan prep %s' if improvements are needed.\n",
		id, maxPlanReviewAutoFixIterations, id)
	return 0
}
