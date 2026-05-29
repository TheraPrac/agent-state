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
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// relativePlanPath returns the plan sidecar path for itemID, relative
// to root when possible (so the linked_plans value round-trips between
// machines without absolute-path drift). Falls back to the absolute
// path if relativization fails. I-512.
func relativePlanPath(plansDir, root, itemID string) string {
	abs := filepath.Join(plansDir, itemID+".md")
	if root == "" {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// PrepOpts holds flags for the prep command.
type PrepOpts struct {
	DryRun          bool
	Model           string
	ItemFilter      string // --item: prep only this item
	IncludeRejected bool   // --include-rejected: re-process rejected plans
	// WriteOnly drives prep without the interactive Accept/Reject/Chat
	// gate. Each unplanned item produces TWO sidecar files:
	// .plans/<id>.md (draft, plan_approved=false) and
	// .plans/<id>.report.md (verbose plan-review narrative). Approval
	// is then a separate step via `st plan approve <id>`. I-565.
	WriteOnly bool
}

// PrepInteractive shows sprint selection and runs prep on the selected sprint.
func PrepInteractive(s *store.Store, cfg *config.Config, opts PrepOpts, engine RunEngine) int {
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	// Find sprints with unplanned items
	type candidate struct {
		sprint   *registry.Sprint
		unplanned int
	}
	var candidates []candidate
	for i := range reg.Sprints {
		sp := &reg.Sprints[i]
		if sp.Status != "active" {
			continue
		}
		unplanned := 0
		for _, itemID := range sp.Items {
			item, ok := s.Get(itemID)
			if !ok || cfg.IsTerminalStatus(item.Type, item.Status) {
				continue
			}
			if plan.Exists(cfg.PlansDir(), itemID) {
				p, _ := plan.Load(cfg.PlansDir(), itemID)
				if p != nil && (p.Approved || (p.Rejected && !opts.IncludeRejected)) {
					continue
				}
			}
			unplanned++
		}
		if unplanned > 0 || sp.Status == "active" {
			candidates = append(candidates, candidate{sprint: sp, unplanned: unplanned})
		}
	}

	if len(candidates) == 0 {
		fmt.Println("No sprints with unplanned items")
		return 0
	}

	// Sprint selection menu
	var sprintOpts []menuOption
	for i, c := range candidates {
		label := fmt.Sprintf("%s  (%d unplanned)", c.sprint.Title, c.unplanned)
		sprintOpts = append(sprintOpts, menuOption{
			Key:   fmt.Sprintf("%d", i+1),
			Label: label,
		})
	}
	choiceKey := engineSelectMenu(engine, "Which sprint to prep?", sprintOpts, 0)
	choice := 0
	fmt.Sscanf(choiceKey, "%d", &choice)
	if choice < 1 || choice > len(candidates) {
		fmt.Fprintln(os.Stderr, "invalid selection")
		return 1
	}

	return Prep(s, cfg, candidates[choice-1].sprint.ID, opts, engine)
}

// Prep generates implementation plans for unplanned items in a sprint.
func Prep(s *store.Store, cfg *config.Config, sprintID string, opts PrepOpts, engine RunEngine) int {
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	sp, spErr := reg.SprintByID(sprintID)
	if spErr != nil {
		fmt.Fprintf(os.Stderr, "sprint not found: %s\n", sprintID)
		return 1
	}

	// Find unplanned items (skip approved and rejected plans)
	var unplanned []string
	for _, itemID := range sp.Items {
		if opts.ItemFilter != "" && itemID != opts.ItemFilter {
			continue
		}
		item, ok := s.Get(itemID)
		if !ok || cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		if plan.Exists(cfg.PlansDir(), itemID) {
			p, _ := plan.Load(cfg.PlansDir(), itemID)
			if p != nil && p.Approved {
				continue // already planned and approved
			}
			if p != nil && p.Rejected && !opts.IncludeRejected {
				continue // explicitly rejected — skip unless overridden
			}
			// I-565: in --write-only mode, an item with BOTH a draft
			// plan AND a report sidecar has been fully prepped and is
			// awaiting `st plan approve`. Skip it so re-running the
			// same prep doesn't re-pay the LLM cost.
			if opts.WriteOnly && p != nil && !p.Approved && !p.Rejected && plan.ReportExists(cfg.PlansDir(), itemID) {
				continue
			}
		}
		unplanned = append(unplanned, itemID)
	}

	if len(unplanned) == 0 {
		fmt.Println("All items in sprint are already planned")
		maybeAutoApproveSprintPlan(s, cfg, sprintID, opts, engine)
		return 0
	}

	if opts.DryRun {
		fmt.Printf("Would plan %d item(s):\n", len(unplanned))
		for _, id := range unplanned {
			item, _ := s.Get(id)
			fmt.Printf("  %s  %s\n", id, item.Title)
		}
		return 0
	}

	fmt.Printf("Planning %d item(s) in sprint %s\n\n", len(unplanned), sp.Title)

	planned := 0
	for i, itemID := range unplanned {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}

		fmt.Printf("━━━ [%d/%d] %s — %s ━━━\n\n", i+1, len(unplanned), itemID, item.Title)

		// Resolve worktree dir. I-407: WorktreeForItem handles new vs.
		// legacy location; falls back to cfg.Root() when no worktree
		// exists (which mirrors the previous "BaseDir empty" behavior).
		worktreeDir := ""
		if cfg.Worktree != nil && cfg.Worktree.Enabled {
			wtBase := cfg.WorktreeForItem(itemID)
			if wtBase == "" {
				wtBase = cfg.Root()
			}
			if _, err := os.Stat(wtBase); err == nil {
				worktreeDir = wtBase
			}
		}

		var result string
		if opts.WriteOnly {
			result = prepItemWriteOnly(s, cfg, itemID, item, opts, engine, worktreeDir)
		} else {
			result = prepItem(s, cfg, itemID, item, opts, engine, worktreeDir)
		}
		if result == "accepted" {
			planned++
		} else if result == "abort" {
			fmt.Println("\nPrep aborted.")
			break
		}
		fmt.Println()
	}

	if opts.WriteOnly {
		fmt.Printf("\nWrote %d plan(s); review reports in .plans/*.report.md; approve with `st plan approve <id>`\n", planned)
	} else {
		fmt.Printf("\nPlanned %d/%d item(s)\n", planned, len(unplanned))
	}
	// Only auto-approve when the loop fully succeeded. On partial
	// success, maybeAutoApproveSprintPlan would still short-circuit
	// (since the failed items lack plan_approved), but skipping the
	// call entirely keeps the UX honest and avoids a spurious "still
	// unplanned" notice when the operator already saw the per-item
	// failure messages.
	if planned == len(unplanned) {
		maybeAutoApproveSprintPlan(s, cfg, sprintID, opts, engine)
	}
	return 0
}

// PrepStandalone runs the prep flow on a single sprintless item. It
// mirrors the per-item short-circuits used inside Prep()'s loop body
// (terminal-status skip, approved sidecar no-op, rejected sidecar skip
// unless IncludeRejected, write-only already-prepped pair skip) and
// then dispatches to prepItem or prepItemWriteOnly for that one item.
// It never calls maybeAutoApproveSprintPlan (no sprint to approve) and
// never mutates registry state. I-571.
//
// Returns 0 on success (including idempotent no-op cases) and 1 only
// when the item is not found. Per-item failures from prepItem* are
// surfaced via the same "[<id>] FAILED" messaging they already emit.
func PrepStandalone(s *store.Store, cfg *config.Config, itemID string, opts PrepOpts, engine RunEngine) int {
	item, ok := s.Get(itemID)
	if !ok {
		fmt.Fprintf(os.Stderr, "item not found: %s\n", itemID)
		return 1
	}
	if cfg.IsTerminalStatus(item.Type, item.Status) {
		fmt.Printf("Item %s is %s — nothing to plan\n", itemID, item.Status)
		return 0
	}

	// Sidecar filters — same set used in Prep()'s loop.
	if plan.Exists(cfg.PlansDir(), itemID) {
		p, _ := plan.Load(cfg.PlansDir(), itemID)
		if p != nil && p.Approved {
			fmt.Printf("Item %s is already plan-approved\n", itemID)
			return 0
		}
		if p != nil && p.Rejected && !opts.IncludeRejected {
			fmt.Printf("Item %s has a rejected plan — re-run with --include-rejected to re-process\n", itemID)
			return 0
		}
		if opts.WriteOnly && p != nil && !p.Approved && !p.Rejected && plan.ReportExists(cfg.PlansDir(), itemID) {
			fmt.Printf("Item %s already has draft plan + report — approve with `st plan approve %s`\n", itemID, itemID)
			return 0
		}
	}

	if opts.DryRun {
		fmt.Printf("Would plan 1 item:\n  %s  %s\n", itemID, item.Title)
		return 0
	}

	// Worktree resolution mirrors Prep()'s loop body.
	worktreeDir := ""
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		wtBase := cfg.WorktreeForItem(itemID)
		if wtBase == "" {
			wtBase = cfg.Root()
		}
		if _, err := os.Stat(wtBase); err == nil {
			worktreeDir = wtBase
		}
	}

	fmt.Printf("━━━ Planning %s — %s ━━━\n\n", itemID, item.Title)
	if opts.WriteOnly {
		prepItemWriteOnly(s, cfg, itemID, item, opts, engine, worktreeDir)
	} else {
		prepItem(s, cfg, itemID, item, opts, engine, worktreeDir)
	}
	return 0
}

// maybeAutoApproveSprintPlan runs after Prep's main loop. When every
// non-terminal item in the sprint has plan_approved=true and the
// sprint itself is not yet plan-approved, it shows the dependency
// analysis (via SprintPlan) and a 2-option Accept/Reject gate; on
// Accept it sets sp.PlanApproved=true and saves the registry. I-558.
//
// Short-circuits when:
//   - opts.ItemFilter is set (single-item retry — sprint-level approval
//     should not fire from a partial run).
//   - opts.DryRun is set.
//   - opts.WriteOnly is set (--write-only is non-interactive by
//     contract; sprint approval is the operator's separate step).
//   - sp.PlanApproved is already true (idempotent).
//   - any non-terminal item still lacks plan_approved (prints the
//     "still unplanned" notice listing the missing IDs).
//
// The gate is intentionally 2-option (Accept / Reject) — sprint plans
// are deterministic dependency analysis, so there's nothing for claude
// to "revise" via Feedback / Interactive. To re-prep specific items,
// the operator just re-runs `st prep`.
func maybeAutoApproveSprintPlan(s *store.Store, cfg *config.Config, sprintID string, opts PrepOpts, engine RunEngine) {
	if opts.ItemFilter != "" || opts.DryRun || opts.WriteOnly {
		return
	}

	// Reload the registry + store so the freshly-written plan_approved
	// flags from the just-finished prep loop are visible.
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return
	}
	sp, spErr := reg.SprintByID(sprintID)
	if spErr != nil {
		return
	}
	if sp.PlanApproved {
		return
	}
	freshStore, err := store.New(cfg)
	if err != nil {
		return
	}

	var unplanned []string
	for _, itemID := range sp.Items {
		item, ok := freshStore.Get(itemID)
		if !ok {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		if !item.PlanApproved {
			unplanned = append(unplanned, itemID)
		}
	}

	if len(unplanned) > 0 {
		fmt.Printf("\nSprint plan not yet auto-approvable — %d item(s) still need plan-approval: %s\n",
			len(unplanned), strings.Join(unplanned, ", "))
		fmt.Printf("Re-run `st prep %s` after the missing item(s) are prepped, or run `st sprint plan %s` to review what's there.\n",
			sprintID, sprintID)
		return
	}

	fmt.Printf("\n━━━ All items prepped — reviewing sprint plan ━━━\n\n")
	if code := SprintPlan(freshStore, cfg, sprintID); code != 0 {
		fmt.Fprintf(os.Stderr, "sprint plan analysis failed (exit %d) — sprint not auto-approved\n", code)
		return
	}

	choice := showReviewGate(ReviewGateInfo{
		ItemID:   sprintID,
		Title:    sp.Title,
		GateType: "Sprint Plan Review",
	}, []menuOption{
		{"1", "Accept — approve sprint for execution"},
		{"2", "Reject — leave sprint unapproved"},
	}, engine)

	// Treat Ctrl-C the same as Reject — never silently approve when
	// the operator interrupts, and don't print the "approve later" hint
	// (the abort signal speaks for itself).
	if choice == "^C" {
		return
	}
	if choice != "1" {
		fmt.Printf("Sprint plan not approved — run `st sprint plan %s` to approve later\n", sprintID)
		return
	}

	sp.PlanApproved = true
	sp.PlanApprovedAt = time.Now().Format(time.RFC3339)
	approver := cfg.AgentID()
	if approver == "" {
		approver = "user"
	}
	sp.PlanApprovedBy = approver
	if err := reg.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving sprint plan approval: %v\n", err)
		return
	}
	fmt.Printf("Sprint plan approved for %s (by %s)\n", sprintID, approver)
}

// prepItem runs the plan proposal + review loop for a single item.
// Returns "accepted", "rejected", or "abort".
func prepItem(s *store.Store, cfg *config.Config, itemID string, item *model.Item, opts PrepOpts, engine RunEngine, worktreeDir string) string {
	cwd := worktreeDir
	if cwd == "" {
		cwd = cfg.Root()
	}

	// Check for an existing draft plan — resume review instead of re-running Claude
	var p *plan.Plan
	if plan.Exists(cfg.PlansDir(), itemID) {
		draft, _ := plan.Load(cfg.PlansDir(), itemID)
		if draft != nil && !draft.Approved {
			fmt.Printf("[%s] Resuming from existing draft plan\n", itemID)
			p = draft
			// Clear rejected state if we're re-processing
			if p.Rejected {
				p.Rejected = false
				p.RejectedAt = ""
			}
		}
	}

	// No draft — run Claude to generate a new plan
	if p == nil {
		prompt := buildPrepPrompt(cfg, itemID, item)

		runOpts := RunOpts{Model: opts.Model}
		args := buildClaudeArgs(cfg, prompt, runOpts, cwd)
		sessionID := generateSessionID()
		env := []string{
			"AS_SESSION_ID=" + sessionID,
			"ST_RUN_ITEM=" + itemID,
			"ST_RUN_STEP=prep",
		}
		if agentID := cfg.AgentID(); agentID != "" {
			env = append(env, "AS_AGENT_ID="+agentID)
		}

		fmt.Printf("[%s] Exploring codebase and generating plan...\n\n", itemID)
		output, exitCode, err := engine.RunClaude(cwd, args, env)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] claude error: %v\n", itemID, err)
			return "rejected"
		}

		// Parse claude output for the plan text
		planText := ""
		claudeResult, parseErr := parseClaudeOutput(output)
		if parseErr == nil {
			planText = claudeResult.Result
		} else if exitCode == 0 {
			planText = string(output)
		} else {
			fmt.Fprintf(os.Stderr, "[%s] claude exited %d\n", itemID, exitCode)
			return "rejected"
		}

		// Parse the plan from claude's output
		p, _ = plan.Parse(planText)
		if p == nil {
			p = &plan.Plan{RawText: planText}
		}

		// Reload item (claude may have updated it via st update)
		if newS, reloadErr := store.New(cfg); reloadErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: failed to reload store: %v\n", itemID, reloadErr)
		} else {
			s = newS
			if reloadedItem, ok := s.Get(itemID); ok {
				item = reloadedItem
			}
		}

		// Fill in ACs from item if claude set them there
		if len(p.ACs) == 0 && len(item.AcceptanceCriteria) > 0 {
			p.ACs = item.AcceptanceCriteria
		}

		// Infer scope repos from plan text if not explicitly set
		if len(p.ScopeRepos) == 0 {
			p.ScopeRepos = inferRepos(cfg, p)
		}

		p.Revisions = append(p.Revisions, plan.Revision{
			Timestamp: plan.Now(),
			Summary:   "Initial plan generated by claude",
		})

		// Save plan as draft immediately — don't wait for approval.
		// If the session is killed, the draft is on disk for next run.
		if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: failed to save draft plan: %v\n", itemID, err)
		} else {
			fmt.Printf("[%s] Draft plan saved\n", itemID)
		}

		// I-982: commit item writes and the plan sidecar together. Placed
		// after plan.Save so the plan file is included in the commit.
		// Non-fatal — a GitSync failure (e.g. non-state dirty file triggers
		// checkNonStateGate) must not abort the plan; the warning names the
		// consequence so the operator can act.
		if syncErr := s.GitSync("plan-prep: commit item updates + draft for " + itemID); syncErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: GitSync after prep: %v — item writes may not be committed\n", itemID, syncErr)
		}
	}

	// I-982: resume-path guard — commit item writes a previous crashed run
	// left dirty before reaching GitSync. No-op when the block above ran
	// (already committed). Placed here so both fresh and resume paths call
	// GitSync at least once.
	if syncErr := s.GitSync("plan-prep: commit pending writes for " + itemID); syncErr != nil {
		fmt.Fprintf(os.Stderr, "[%s] Warning: GitSync on resume: %v — pending item writes may not be committed\n", itemID, syncErr)
	}

	// Review loop
	autoFixCount := 0
	for {
		// Show the plan
		fmt.Printf("\n=== Plan: %s ===\n", itemID)
		fmt.Printf("Title: %s\n", item.Title)
		if p.Approach != "" {
			fmt.Printf("\nApproach:\n%s\n", p.Approach)
		}
		if len(p.ScopeRepos) > 0 {
			fmt.Printf("\nScope: %s\n", strings.Join(p.ScopeRepos, ", "))
		}
		if len(p.Steps) > 0 {
			fmt.Printf("\nImplementation Steps:\n")
			for i, step := range p.Steps {
				fmt.Printf("  %d. %s\n", i+1, step)
			}
		}
		if len(p.FilesToCreate) > 0 {
			fmt.Printf("\nFiles to Create:\n")
			for _, f := range p.FilesToCreate {
				fmt.Printf("  + %s\n", f)
			}
		}
		if len(p.FilesToModify) > 0 {
			fmt.Printf("\nFiles to Modify:\n")
			for _, f := range p.FilesToModify {
				fmt.Printf("  ~ %s\n", f)
			}
		}
		if len(p.ACs) > 0 {
			fmt.Printf("\nAcceptance Criteria:\n")
			for i, ac := range p.ACs {
				fmt.Printf("  %d. %s\n", i+1, ac)
			}
		}

		// Launch claude to critically review the plan
		reviewPrompt := buildPlanReviewPrompt(itemID, item)
		reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
		reviewStep.SetName("plan_review")
		runOpts := RunOpts{Model: opts.Model}
		reviewStart := time.Now()
		reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, runOpts, engine, cwd, "", false)
		reviewDur := time.Since(reviewStart)
		rec := extractRecommendation(reviewSR.FullOutput)

		// Auto-fix "Accept with notes" — feed notes back to claude without user input
		if isAcceptWithNotes(rec) && autoFixCount < maxAutoFixIterations {
			autoFixCount++
			fmt.Printf("[%s] Review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
				itemID, autoFixCount, maxAutoFixIterations)
			notes := extractNotesFromReview(reviewSR.FullOutput)
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)
			var sr StepResult
			runAutoFixFromNotes(s, cfg, itemID, "", item, "plan review", notes, RunOpts{Model: opts.Model}, engine, cwd, "", &sr)
			// Re-save draft after auto-fix
			if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] Warning: failed to save revised plan: %v\n", itemID, err)
			}
			continue // re-run review
		}

		// I-180: full-stack split candidate detection. When the plan
		// classifies as full-stack with 5+ ACs, print a SPLIT
		// RECOMMENDATION banner and append a fifth menu choice. The
		// recommendation is advisory — declining is a single keystroke
		// and proceeds to normal Accept; the decision is recorded in
		// scope_flags for retrospective analysis.
		splitCandidate := plan.DetectFullStack(p, 5)
		if splitCandidate {
			printSplitRecommendation(itemID, item, p)
		}

		menuOpts := []menuOption{
			{"1", "Accept      — save plan and proceed"},
			{"2", "Reject      — skip this item"},
			{"3", "Feedback    — type direction, claude revises (constrained)"},
			{"4", "Interactive — full claude session (escape hatch)"},
		}
		if splitCandidate {
			menuOpts = append(menuOpts, menuOption{
				"5", "Split       — create linked Part A + Part B items and reject this plan",
			})
		}

		gateInfo := ReviewGateInfo{
			ItemID:         itemID,
			Title:          item.Title,
			GateType:       "Plan Review",
			Iteration:      1,
			Recommendation: rec,
			ReviewDuration: reviewDur,
			AcsTotal:       len(p.ACs),
		}
		if splitCandidate {
			// I-180: don't let claude's positive review auto-proceed
			// past the split recommendation — the operator must
			// explicitly choose Accept (kept-unified) or Split.
			gateInfo.BlockAutoProceed = true
			gateInfo.BlockReason = "full-stack split recommendation present"
		}
		choice := showReviewGate(gateInfo, menuOpts, engine)

		if choice == "^C" {
			return "rejected"
		}
		if choice == "5" && splitCandidate {
			// Split: create child items and reject this plan. The
			// parent is closed by Split() with resolution=split, so
			// returning "rejected" here just stops further processing
			// of the parent's plan. On error, also return "rejected"
			// so the review loop doesn't re-enter (a failed Split
			// usually means the parent is already split — re-running
			// would loop forever; the operator can re-prep manually).
			idA, idB, err := Split(s, cfg, itemID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] split failed: %v\n", itemID, err)
				return "rejected"
			}
			fmt.Printf("[%s] Split into linked items: %s (backend) + %s (frontend, depends_on %s)\n",
				itemID, idA, idB, idA)
			return "rejected"
		}
		if choice == "1" {
			// Validate AC shell syntax before accepting
			syntaxErrors := ValidateACsyntax(p.ACs)
			if len(syntaxErrors) > 0 {
				fmt.Printf("\n⚠ %d AC(s) have shell syntax errors — fix before accepting:\n", len(syntaxErrors))
				for _, e := range syntaxErrors {
					fmt.Printf("  %s\n", e)
				}
				fmt.Println()
				continue // back to menu
			}

			// Accept — save plan sidecar
			p.Approved = true
			p.ApprovedAt = plan.Now()
			if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
				fmt.Fprintf(os.Stderr, "saving plan: %v\n", err)
				return "rejected"
			}

			// Update item — reload store first so the local s is fresh,
			// then use Mutate so the approval is written atomically.
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)
			capturedScopeRepos := append([]string(nil), p.ScopeRepos...)
			capturedACs := append([]string(nil), p.ACs...)
			approvedAt := time.Now().Format(time.RFC3339)
			approver := cfg.AgentID()
			if approver == "" {
				approver = "user"
			}
			// I-512: stamp linked_plans with the per-item sidecar path so
			// downstream consumers (the plan-before-code hook, st prime,
			// review tools) can correlate the active item with its plan
			// content. Sidecars live under cfg.PlansDir() with filename
			// `<id>.md`; we record the path RELATIVE to repo root so the
			// value round-trips between machines without absolute-path
			// drift.
			sidecarRel := relativePlanPath(cfg.PlansDir(), cfg.Root(), itemID)

			if err := s.Mutate(itemID, func(item *model.Item) error {
				item.PlanApproved = true
				item.PlanApprovedAt = approvedAt
				item.PlanApprovedBy = approver
				item.Doc.SetField("plan_approved", "true")
				item.Doc.SetField("plan_approved_at", approvedAt)
				item.Doc.SetField("plan_approved_by", approver)
				// Set scope_repos as a field
				if len(capturedScopeRepos) > 0 {
					item.Doc.SetField("scope_repos", strings.Join(capturedScopeRepos, ", "))
				}
				// Ensure ACs are on the item
				if len(item.AcceptanceCriteria) == 0 && len(capturedACs) > 0 {
					item.Doc.ReplaceList("acceptance_criteria", capturedACs)
				}
				// I-512: append the sidecar path to linked_plans, idempotent
				// against re-Accept on a previously rejected plan.
				if sidecarRel != "" {
					already := false
					for _, lp := range item.LinkedPlans {
						if lp == sidecarRel {
							already = true
							break
						}
					}
					if !already {
						item.LinkedPlans = append(item.LinkedPlans, sidecarRel)
						item.Doc.ReplaceList("linked_plans", item.LinkedPlans)
					}
				}
				// I-180: when the operator declines a SPLIT
				// RECOMMENDATION, stamp scope_flags so retrospective
				// analysis can correlate split-vs-unified outcomes
				// against ci_fix rates. SetNestedField writes a true
				// nested block (`scope_flags:` parent + indented
				// child) so downstream readers using GetNestedField
				// see the values.
				if splitCandidate {
					item.Doc.SetNestedField("scope_flags.full_stack", "true")
					item.Doc.SetNestedField("scope_flags.split_recommended", "true")
					item.Doc.SetNestedField("scope_flags.split_decision", "kept-unified")
				}
				return nil
			}); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] ERROR: failed to save plan approval: %v\n", itemID, err)
				return "rejected"
			}

			// Pre-warm model recommendation so `st start` model check resolves instantly.
			stampModelRec(s, cfg, itemID, engine)
			fmt.Printf("[%s] Plan accepted — plan_approved: true written to item\n", itemID)
			return "accepted"
		}

		if choice == "2" {
			// Mark plan as rejected so it's skipped on future runs
			p.Rejected = true
			p.RejectedAt = plan.Now()
			if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] Warning: failed to save rejected plan: %v\n", itemID, err)
			}
			fmt.Printf("[%s] Plan rejected — will skip on future runs (use --include-rejected to re-process)\n", itemID)
			return "rejected"
		}

		if choice == "3" {
			// Constrained feedback
			var sr StepResult
			runConstrainedFeedback(s, cfg, itemID, "", item, "plan review", RunOpts{Model: opts.Model}, engine, cwd, "", &sr)
		} else {
			// Option 4: interactive escape hatch
			runInteractiveEscapeHatch(itemID, cwd, engine, cfg)
		}

		// Reload after revision
		s, _ = store.New(cfg)
		item, _ = s.Get(itemID)

		// Update ACs from item if changed
		if len(item.AcceptanceCriteria) > 0 {
			p.ACs = item.AcceptanceCriteria
		}
		if item.Summary != "" && p.Approach == "" {
			p.Approach = item.Summary
		}

		p.Revisions = append(p.Revisions, plan.Revision{
			Timestamp: plan.Now(),
			Summary:   "Revised after feedback",
		})

		// Save revised draft immediately
		if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: failed to save revised plan: %v\n", itemID, err)
		}
	}
}

// prepItemWriteOnly runs plan generation + plan-review for a single
// item without showing the interactive Accept/Reject/Chat gate. Both
// the plan and the verbose review narrative are saved as sidecars
// under cfg.PlansDir(). Approval is a separate gate via the existing
// `st plan approve <id>` command. I-565.
//
// Returns "accepted" on success (a plan + report were saved) and
// "rejected" on per-item failure. Per-item failures emit a `[<id>]
// FAILED: ...` line and the caller continues with the next item.
func prepItemWriteOnly(s *store.Store, cfg *config.Config, itemID string, item *model.Item, opts PrepOpts, engine RunEngine, worktreeDir string) string {
	cwd := worktreeDir
	if cwd == "" {
		cwd = cfg.Root()
	}

	// Reuse the same draft-on-disk recovery as prepItem so a previous
	// failed write-only run (plan saved but report missing) doesn't
	// re-pay the plan-generation cost on retry.
	var p *plan.Plan
	if plan.Exists(cfg.PlansDir(), itemID) {
		draft, _ := plan.Load(cfg.PlansDir(), itemID)
		if draft != nil && !draft.Approved {
			fmt.Printf("[%s] Resuming from existing draft plan\n", itemID)
			p = draft
			if p.Rejected {
				p.Rejected = false
				p.RejectedAt = ""
			}
		}
	}

	// Generate a new plan if no usable draft exists.
	if p == nil {
		prompt := buildPrepPrompt(cfg, itemID, item)

		runOpts := RunOpts{Model: opts.Model}
		args := buildClaudeArgs(cfg, prompt, runOpts, cwd)
		sessionID := generateSessionID()
		env := []string{
			"AS_SESSION_ID=" + sessionID,
			"ST_RUN_ITEM=" + itemID,
			"ST_RUN_STEP=prep",
		}
		if agentID := cfg.AgentID(); agentID != "" {
			env = append(env, "AS_AGENT_ID="+agentID)
		}

		fmt.Printf("[%s] Exploring codebase and generating plan...\n", itemID)
		output, exitCode, err := engine.RunClaude(cwd, args, env)
		if err != nil {
			fmt.Printf("[%s] FAILED: claude error: %v\n", itemID, err)
			return "rejected"
		}

		planText := ""
		claudeResult, parseErr := parseClaudeOutput(output)
		if parseErr == nil {
			planText = claudeResult.Result
		} else if exitCode == 0 {
			planText = string(output)
		} else {
			fmt.Printf("[%s] FAILED: claude exited %d\n", itemID, exitCode)
			return "rejected"
		}

		p, _ = plan.Parse(planText)
		if p == nil {
			p = &plan.Plan{RawText: planText}
		}

		// Reload item — claude may have updated it via `st update`
		if newS, reloadErr := store.New(cfg); reloadErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: failed to reload store: %v\n", itemID, reloadErr)
		} else {
			s = newS
			if reloadedItem, ok := s.Get(itemID); ok {
				item = reloadedItem
			}
		}

		if len(p.ACs) == 0 && len(item.AcceptanceCriteria) > 0 {
			p.ACs = item.AcceptanceCriteria
		}
		if len(p.ScopeRepos) == 0 {
			p.ScopeRepos = inferRepos(cfg, p)
		}

		p.Revisions = append(p.Revisions, plan.Revision{
			Timestamp: plan.Now(),
			Summary:   "Initial plan generated by claude (write-only)",
		})

		if err := plan.Save(cfg.PlansDir(), itemID, p); err != nil {
			fmt.Printf("[%s] FAILED: save draft plan: %v\n", itemID, err)
			return "rejected"
		}

		// I-982: commit item writes and the plan sidecar after plan.Save.
		// Non-fatal — warn to stderr (not stdout) to avoid corrupting the
		// structured batch output that callers parse.
		if syncErr := s.GitSync("plan-prep: commit item updates + draft for " + itemID); syncErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] Warning: GitSync after prep: %v — item writes may not be committed\n", itemID, syncErr)
		}
	}

	// I-982: resume-path guard — commit item writes left dirty by a previous
	// crashed run. No-op on fresh runs (already committed above).
	if syncErr := s.GitSync("plan-prep: commit pending writes for " + itemID); syncErr != nil {
		fmt.Fprintf(os.Stderr, "[%s] Warning: GitSync on resume: %v — pending item writes may not be committed\n", itemID, syncErr)
	}

	// Run the plan-review subprocess (same call shape as prepItem)
	// but capture the narrative output to a sidecar instead of
	// gating on the recommendation interactively.
	reviewPrompt := buildPlanReviewPrompt(itemID, item)
	reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
	reviewStep.SetName("plan_review")
	runOpts := RunOpts{Model: opts.Model}
	reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, runOpts, engine, cwd, "", false)
	// Any non-empty Error means the review subprocess failed. Even if
	// it produced partial output before the failure, that output is
	// not a complete review and must not be persisted as the report.
	if reviewSR.Error != "" {
		fmt.Printf("[%s] FAILED: plan-review error: %s\n", itemID, reviewSR.Error)
		return "rejected"
	}

	if err := plan.SaveReport(cfg.PlansDir(), itemID, reviewSR.FullOutput); err != nil {
		fmt.Printf("[%s] FAILED: save report: %v\n", itemID, err)
		return "rejected"
	}

	// I-982: commit the report sidecar. Both GitSync calls above ran before
	// plan.SaveReport, so the .report.md would otherwise be left untracked on
	// disk until the next independent st sync — exactly the race I-982 fixes.
	if syncErr := s.GitSync("plan-prep: commit plan report for " + itemID); syncErr != nil {
		fmt.Fprintf(os.Stderr, "[%s] Warning: GitSync after SaveReport: %v — report sidecar may not be committed\n", itemID, syncErr)
	}

	planRel := relativePlanPath(cfg.PlansDir(), cfg.Root(), itemID)
	reportRel := relativeReportPath(cfg.PlansDir(), cfg.Root(), itemID)
	fmt.Printf("[%s] plan saved (%s), report saved (%s), pending approval\n", itemID, planRel, reportRel)
	return "accepted"
}

// relativeReportPath mirrors relativePlanPath for the report sidecar.
// Falls back to the absolute path when filepath.Rel fails — same
// semantics as relativePlanPath so log output stays consistent.
func relativeReportPath(plansDir, root, itemID string) string {
	abs := plan.ReportPath(plansDir, itemID)
	if root == "" {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// printSplitRecommendation renders the I-180 SPLIT RECOMMENDATION
// banner shown when DetectFullStack returns true. The output lists
// the parent's scope and AC count, plus a partition preview of how
// the AC list would be split between Part A (backend) and Part B
// (frontend) so the operator can decide before hitting the gate.
//
// Advisory only — declining is a single keystroke ("Accept") and
// proceeds normally.
func printSplitRecommendation(itemID string, item *model.Item, p *plan.Plan) {
	apiACs, webACs := plan.PartitionACsByLayer(p.ACs)
	fmt.Println()
	fmt.Println("=== SPLIT RECOMMENDATION (I-180) ===")
	fmt.Printf("%s — full-stack item with %d ACs detected.\n", itemID, len(p.ACs))
	fmt.Printf("Cost data: full-stack items average $15.61/item — 2.2× the cost of single-layer items.\n")
	fmt.Printf("Splitting caps the blast radius of a review finding to one layer.\n\n")
	fmt.Printf("Proposed Part A (backend, theraprac-api): %s (Part A: backend)\n", item.Title)
	fmt.Printf("  ACs (%d):\n", len(apiACs))
	for _, ac := range apiACs {
		fmt.Printf("    - %s\n", truncate(ac, 96))
	}
	fmt.Printf("Proposed Part B (frontend, theraprac-web; depends_on Part A): %s (Part B: frontend)\n", item.Title)
	fmt.Printf("  ACs (%d):\n", len(webACs))
	for _, ac := range webACs {
		fmt.Printf("    - %s\n", truncate(ac, 96))
	}
	fmt.Println()
	fmt.Println("Choose Split (option 5) to create the linked items and reject this plan.")
	fmt.Println("Choose Accept to keep unified — your decision is recorded for retrospective analysis.")
}

// buildPrepPrompt creates the exploration prompt for plan generation.
func buildPrepPrompt(cfg *config.Config, itemID string, item *model.Item) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("You are planning implementation for item %s.\n\n", itemID))
	b.WriteString(fmt.Sprintf("Title: %s\n", item.Title))
	if item.Summary != "" {
		b.WriteString(fmt.Sprintf("Existing summary: %s\n", item.Summary))
	}
	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("Existing acceptance criteria:\n")
		for _, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("  %s\n", ac))
		}
	}
	if len(item.DependsOn) > 0 {
		b.WriteString(fmt.Sprintf("Dependencies: %s\n", strings.Join(item.DependsOn, ", ")))
	}

	b.WriteString("\n")
	b.WriteString("Explore the codebase thoroughly. Then:\n\n")
	b.WriteString("1. Write a clear technical SUMMARY describing the approach and set it:\n")
	b.WriteString(fmt.Sprintf("   cat <<'EOF' | st update %s summary --stdin\n", itemID))
	b.WriteString("   Your summary text here. Can be multiline.\n")
	b.WriteString("   EOF\n\n")
	b.WriteString("2. Write ACCEPTANCE CRITERIA — every criterion MUST start with '- cmd:' followed by\n")
	b.WriteString("   an executable command that exits 0 on success. Set them:\n")
	b.WriteString(fmt.Sprintf("   cat <<'EOF' | st update %s acceptance_criteria --stdin\n", itemID))
	b.WriteString("   - cmd: first test command\n")
	b.WriteString("   - cmd: second test command\n")
	b.WriteString("   EOF\n\n")
	b.WriteString("   IMPORTANT: The heredoc MUST contain ONLY AC lines (starting with '- cmd:').\n")
	b.WriteString("   Do NOT include prose, markdown headers, separators (---), or commentary inside the heredoc.\n")
	b.WriteString("   Any non-AC text inside the heredoc will be stored as a broken AC.\n\n")
	b.WriteString("3. Print your full analysis as a structured plan with these sections:\n\n")
	b.WriteString("## Approach\n")
	b.WriteString("High-level description of what you'll do and why.\n\n")
	b.WriteString("## Scope\n")
	b.WriteString("Repos: <list which repos this touches: theraprac-api, theraprac-web, theraprac-infra>\n\n")
	b.WriteString("## Implementation Steps\n")
	b.WriteString("1. Ordered list of implementation steps\n\n")
	b.WriteString("## Files to Create\n")
	b.WriteString("- path/to/new/file\n\n")
	b.WriteString("## Files to Modify\n")
	b.WriteString("- path/to/existing/file\n\n")
	b.WriteString("## Acceptance Criteria\n")
	b.WriteString("- cmd: the same ACs you set on the item\n\n")
	b.WriteString("CRITICAL: Every AC line MUST begin with '- cmd: '. No prose ACs.\n")
	b.WriteString("Use paths relative to the worktree: 'cd ../theraprac-api && ...' or 'cd ../theraprac-web && ...'.\n")
	b.WriteString("For test suite execution, use `st test <id> <suite> --run` — NEVER use raw `make e2e` or `make test` in ACs.\n")
	b.WriteString("ACs should be fast to verify — use targeted test runs (specific spec files), not full suite runs.\n")
	b.WriteString("For new features, name the test function that WILL exist after implementation.\n\n")
	b.WriteString("Do NOT ask permission — explore, analyze, set fields, and print the plan.\n")

	return b.String()
}

// inferRepos guesses which repos the plan touches based on file paths.
func inferRepos(cfg *config.Config, p *plan.Plan) []string {
	repoSet := make(map[string]bool)
	allFiles := append(append([]string{}, p.FilesToCreate...), p.FilesToModify...)
	for _, f := range allFiles {
		for _, repo := range cfg.Worktree.Repos {
			if strings.Contains(f, repo) {
				repoSet[repo] = true
			}
		}
	}
	// Also check approach text
	if p.Approach != "" && cfg.Worktree != nil {
		for _, repo := range cfg.Worktree.Repos {
			if strings.Contains(p.Approach, repo) {
				repoSet[repo] = true
			}
		}
	}
	var repos []string
	for repo := range repoSet {
		repos = append(repos, repo)
	}
	return repos
}
