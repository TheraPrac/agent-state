package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// PrepOpts holds flags for the prep command.
type PrepOpts struct {
	DryRun     bool
	Model      string
	ItemFilter string // --item: prep only this item
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
			if !plan.Exists(cfg.PlansDir(), itemID) {
				item, ok := s.Get(itemID)
				if ok && !cfg.IsTerminalStatus(item.Type, item.Status) {
					unplanned++
				}
			}
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

	// Find unplanned items
	var unplanned []string
	for _, itemID := range sp.Items {
		if opts.ItemFilter != "" && itemID != opts.ItemFilter {
			continue
		}
		if plan.Exists(cfg.PlansDir(), itemID) {
			p, _ := plan.Load(cfg.PlansDir(), itemID)
			if p != nil && p.Approved {
				continue // already planned and approved
			}
		}
		item, ok := s.Get(itemID)
		if !ok || cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		unplanned = append(unplanned, itemID)
	}

	if len(unplanned) == 0 {
		fmt.Println("All items in sprint are already planned")
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

		// Resolve worktree dir
		worktreeDir := ""
		if cfg.Worktree != nil && cfg.Worktree.Enabled {
			wtBase := cfg.Root()
			if cfg.Worktree.BaseDir != "" {
				wtBase = fmt.Sprintf("%s/%s/%s", cfg.Root(), cfg.Worktree.BaseDir, itemID)
			}
			if _, err := os.Stat(wtBase); err == nil {
				worktreeDir = wtBase
			}
		}

		result := prepItem(s, cfg, itemID, item, opts, engine, worktreeDir)
		if result == "accepted" {
			planned++
		} else if result == "abort" {
			fmt.Println("\nPrep aborted.")
			break
		}
		fmt.Println()
	}

	fmt.Printf("\nPlanned %d/%d item(s)\n", planned, len(unplanned))
	return 0
}

// prepItem runs the plan proposal + review loop for a single item.
// Returns "accepted", "rejected", or "abort".
func prepItem(s *store.Store, cfg *config.Config, itemID string, item *model.Item, opts PrepOpts, engine RunEngine, worktreeDir string) string {
	// Build the exploration prompt
	prompt := buildPrepPrompt(cfg, itemID, item)

	cwd := worktreeDir
	if cwd == "" {
		cwd = cfg.Root()
	}

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
	p, _ := plan.Parse(planText)
	if p == nil {
		p = &plan.Plan{RawText: planText}
	}

	// Reload item (claude may have updated it via st update)
	s, _ = store.New(cfg)
	item, _ = s.Get(itemID)

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

	// Review loop
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

		choice := showReviewGate(ReviewGateInfo{
			ItemID:         itemID,
			Title:          item.Title,
			GateType:       "Plan Review",
			Iteration:      1,
			Recommendation: rec,
			ReviewDuration: reviewDur,
			AcsTotal:       len(p.ACs),
		}, []menuOption{
			{"1", "Accept      — save plan and proceed"},
			{"2", "Reject      — skip this item"},
			{"3", "Feedback    — type direction, claude revises (constrained)"},
			{"4", "Interactive — full claude session (escape hatch)"},
		}, engine)

		if choice == "^C" {
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

			// Update item
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)
			item.PlanApproved = true
			item.Doc.SetField("plan_approved", "true")
			// Set scope_repos as a field
			if len(p.ScopeRepos) > 0 {
				item.Doc.SetField("scope_repos", strings.Join(p.ScopeRepos, ", "))
			}
			// Ensure ACs are on the item
			if len(item.AcceptanceCriteria) == 0 && len(p.ACs) > 0 {
				item.Doc.ReplaceList("acceptance_criteria", p.ACs)
			}
			item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
			if err := s.Write(item); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] ERROR: failed to save plan approval: %v\n", itemID, err)
				return "rejected"
			}

			fmt.Printf("[%s] Plan accepted — plan_approved: true written to item\n", itemID)
			return "accepted"
		}

		if choice == "2" {
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
	}
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
