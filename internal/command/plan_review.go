package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// maxPlanReviewAutoFixIterations bounds the Accept-with-notes auto-fix
// loop. Two passes converges on the vast majority of weak-content
// findings in the I-588 SBAR review precedent; beyond two iterations
// the model usually loops on the same edits.
const maxPlanReviewAutoFixIterations = 2

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

	for iter := 0; iter <= maxPlanReviewAutoFixIterations; iter++ {
		// Build the prompt and execute claude.
		prompt := buildPlanReviewPrompt(id, item)
		step := config.RunStepDef{Type: "claude", Prompt: prompt}
		step.SetName("plan_review_approve")
		sr := executeClaude(s, cfg, id, "", step, RunOpts{}, engine, cwd, "", false)

		if !sr.Passed && sr.FullOutput == "" {
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

		// Accept with notes → auto-fix and re-run.
		if isAcceptWithNotes(rec) && iter < maxPlanReviewAutoFixIterations {
			fmt.Fprintf(os.Stderr,
				"%s: plan review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
				id, iter+1, maxPlanReviewAutoFixIterations)
			notes := extractNotesFromReview(sr.FullOutput)
			var resultRef StepResult
			runAutoFixFromNotes(s, cfg, id, "", item, "plan review", notes, RunOpts{}, engine, cwd, "", &resultRef)
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

	// Exhausted auto-fix iterations on Accept-with-notes — treat as
	// pass with a stderr breadcrumb so the operator knows the
	// sub-agent stopped converging.
	fmt.Fprintf(os.Stderr,
		"%s: plan review reached the auto-fix iteration cap (%d) — proceeding with last accepted state\n",
		id, maxPlanReviewAutoFixIterations)
	return 0
}
