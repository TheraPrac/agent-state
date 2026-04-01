package command

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// RunOpts holds flags for the run/advance commands.
type RunOpts struct {
	DryRun         bool
	MaxBudgetUSD   float64
	Parallelism    int
	ItemFilter     string // --item: run only this item
	Model          string
	PermissionMode string
	StepFilter     string // --step: advance up to this step name
	Fresh          bool   // --fresh: ignore saved progress, restart pipeline from step 0
}

// RunEngine holds injectable dependencies for run/advance.
type RunEngine struct {
	// RunClaude launches a claude -p subprocess and returns its output.
	RunClaude func(cwd string, args []string, env []string) ([]byte, int, error)
	// RunClaudeInteractive launches claude in interactive mode (stdin/stdout attached).
	// Returns exit code. If nil, uses default exec.Command implementation.
	RunClaudeInteractive func(cwd string, args []string) (int, error)
	// PromptUser reads a line from stdin (for gate steps and free-text input).
	PromptUser func(prompt string) (string, error)
	// SelectMenu overrides the interactive arrow-key menu (for testing).
	// If nil, uses the real terminal-based selectMenu.
	SelectMenu func(prompt string, options []menuOption, defaultIdx int) string
	// ConfirmPrompt overrides the y/N confirmation prompt (for testing).
	// If nil, uses the real terminal-based confirmPrompt.
	ConfirmPrompt func(prompt string) bool
}

// ClaudeResult represents parsed JSON output from claude -p --output-format json.
type ClaudeResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMs   int64   `json:"duration_ms"`
	SessionID    string  `json:"session_id"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
}

// StepResult captures the outcome of a single pipeline step.
type StepResult struct {
	Step         string        `json:"step"`
	Type         string        `json:"type"`
	Passed       bool          `json:"passed"`
	Output       string        `json:"output,omitempty"`
	Error        string        `json:"error,omitempty"`
	Duration     time.Duration `json:"duration"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	AIDurationMs int64         `json:"ai_duration_ms,omitempty"`
}

// ItemResult captures the outcome of running one sprint item.
type ItemResult struct {
	ItemID    string       `json:"item_id"`
	Title     string       `json:"title"`
	Steps     []StepResult `json:"steps"`
	Success   bool         `json:"success"`
	TotalCost float64      `json:"total_cost"`
	Duration  time.Duration `json:"duration"`
}

// DefaultRunEngine returns a RunEngine with real implementations.
func DefaultRunEngine() RunEngine {
	return RunEngine{
		RunClaude:  defaultRunClaude,
		PromptUser: defaultPromptUser,
	}
}

// RunInteractive shows available sprints and lets the user pick one to run.
func RunInteractive(s *store.Store, cfg *config.Config, opts RunOpts, engine RunEngine) int {
	pipeline := cfg.RunPipeline()
	if len(pipeline) == 0 {
		fmt.Fprintln(os.Stderr, "no run.pipeline configured — define run.step_order and run.steps in config")
		return 1
	}

	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	// Build sprint progress map
	type sprintProgress struct {
		sprint   *registry.Sprint
		total    int
		complete int
	}
	sprintMap := make(map[string]*sprintProgress)
	for i := range reg.Sprints {
		sp := &reg.Sprints[i]
		if sp.Status != "active" || len(sp.Items) == 0 {
			continue
		}
		total, complete := 0, 0
		for _, itemID := range sp.Items {
			total++
			item, ok := s.Get(itemID)
			if ok && cfg.IsTerminalStatus(item.Type, item.Status) {
				complete++
			}
		}
		if complete < total {
			sprintMap[sp.ID] = &sprintProgress{sprint: sp, total: total, complete: complete}
		}
	}

	// Track which sprints are in an epic (for "loose sprints" section)
	sprintInEpic := make(map[string]bool)

	// Build numbered selection list
	type candidate struct {
		sprint   *registry.Sprint
		total    int
		complete int
	}
	var candidates []candidate
	num := 0

	// --- Section 1: Epics with ordered sprints ---
	hasEpicOutput := false
	for _, epic := range reg.Epics {
		if epic.Status != "active" {
			continue
		}
		// Show all sprints in this epic — archived as completed, active as selectable
		hasSprintsForEpic := false

		// First check if this epic has any sprints at all
		allSprintIDs := epic.SprintOrder
		if len(allSprintIDs) == 0 {
			for _, es := range reg.Sprints {
				if es.Epic == epic.ID {
					allSprintIDs = append(allSprintIDs, es.ID)
				}
			}
		}
		if len(allSprintIDs) == 0 {
			continue
		}

		if !hasEpicOutput {
			fmt.Println("Epics:")
			hasEpicOutput = true
		}
		fmt.Printf("\n  %s\n", epic.Title)

		for _, sid := range allSprintIDs {
			sprintInEpic[sid] = true

			// Check if it's archived/completed
			var sprintRef *registry.Sprint
			for i := range reg.Sprints {
				if reg.Sprints[i].ID == sid {
					sprintRef = &reg.Sprints[i]
					break
				}
			}
			if sprintRef == nil {
				continue
			}

			if sprintRef.Status == "archived" || sprintRef.Status == "completed" {
				fmt.Printf("    done %s  %d/%d\n", sprintRef.Title, len(sprintRef.Items), len(sprintRef.Items))
				hasSprintsForEpic = true
				continue
			}

			// Active sprint with remaining work
			if sp, ok := sprintMap[sid]; ok {
				num++
				approved := ""
				if sp.sprint.PlanApproved {
					approved = " [approved]"
				}
				fmt.Printf("    %d. %s  %d/%d%s\n", num, sp.sprint.Title, sp.complete, sp.total, approved)
				candidates = append(candidates, candidate{sprint: sp.sprint, total: sp.total, complete: sp.complete})
				hasSprintsForEpic = true
			}
		}
		_ = hasSprintsForEpic
	}

	// --- Section 2: Loose sprints (not in any epic) ---
	var looseSprints []*sprintProgress
	for _, sp := range sprintMap {
		if !sprintInEpic[sp.sprint.ID] {
			looseSprints = append(looseSprints, sp)
		}
	}
	if len(looseSprints) > 0 {
		fmt.Printf("\nSprints (no epic):\n")
		for _, sp := range looseSprints {
			num++
			approved := ""
			if sp.sprint.PlanApproved {
				approved = " [approved]"
			}
			fmt.Printf("    %d. %s  %d/%d%s\n", num, sp.sprint.Title, sp.complete, sp.total, approved)
			candidates = append(candidates, candidate{sprint: sp.sprint, total: sp.total, complete: sp.complete})
		}
	}

	// --- Section 3: Queue items not in any sprint, grouped by tag ---
	queueEntries := LoadQueue(cfg)
	var unsprintedItems []struct {
		id, title string
		tags      []string
	}
	for _, e := range queueEntries {
		item, ok := s.Get(e.ID)
		if !ok || item.Sprint != "" {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		unsprintedItems = append(unsprintedItems, struct {
			id, title string
			tags      []string
		}{item.ID, item.Title, item.Tags})
	}
	if len(unsprintedItems) > 0 {
		fmt.Printf("\nQueue (not in a sprint):\n")
		// Group by first tag
		tagGroups := make(map[string][]string)
		var tagOrder []string
		for _, it := range unsprintedItems {
			tag := "(untagged)"
			if len(it.tags) > 0 {
				tag = it.tags[0]
			}
			if _, exists := tagGroups[tag]; !exists {
				tagOrder = append(tagOrder, tag)
			}
			tagGroups[tag] = append(tagGroups[tag], fmt.Sprintf("%-8s %s", it.id, it.title))
		}
		sort.Strings(tagOrder)
		for _, tag := range tagOrder {
			fmt.Printf("  [%s]\n", tag)
			for _, line := range tagGroups[tag] {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	if len(candidates) == 0 {
		fmt.Println("\nNo active sprints with remaining work")
		return 0
	}

	// Prompt for selection
	var sprintOpts []menuOption
	for i, c := range candidates {
		sprintOpts = append(sprintOpts, menuOption{
			Key:   fmt.Sprintf("%d", i+1),
			Label: c.sprint.Title,
		})
	}
	choiceKey := engineSelectMenu(engine, "Which sprint?", sprintOpts, 0)
	choice := 0
	fmt.Sscanf(choiceKey, "%d", &choice)
	if choice < 1 || choice > len(candidates) {
		fmt.Fprintln(os.Stderr, "invalid selection")
		return 1
	}

	selected := candidates[choice-1]
	sp := selected.sprint

	// Plan validation + approval
	if !sp.PlanApproved {
		fmt.Printf("\nSprint %s plan not yet approved. Showing plan:\n\n", sp.ID)
		SprintPlan(s, cfg, sp.ID)
		fmt.Printf("\nPipeline (%d steps):\n", len(pipeline))
		for i, step := range pipeline {
			fmt.Printf("  %d. [%s] %s\n", i+1, step.Type, step.Name())
		}
		if !engineConfirmPrompt(engine, "\nApprove this plan?") {
			fmt.Println("Plan not approved. Exiting.")
			return 0
		}

		// Approve the plan
		sp.PlanApproved = true
		sp.PlanApprovedAt = time.Now().Format(time.RFC3339)
		sp.PlanApprovedBy = "user"
		if err := reg.Save(cfg.EpicsPath()); err != nil {
			fmt.Fprintf(os.Stderr, "saving plan approval: %v\n", err)
			return 1
		}
		fmt.Printf("Plan approved for %s\n\n", sp.ID)
	}

	return Run(s, cfg, sp.ID, opts, engine)
}

// RunItem runs a single item through the pipeline, finding its sprint automatically.
// If the item has no sprint, runs it standalone.
// RunStatus shows the pipeline progress for all items in active sprints.
func RunStatus(s *store.Store, cfg *config.Config) int {
	pipeline := cfg.RunPipeline()
	if len(pipeline) == 0 {
		fmt.Fprintln(os.Stderr, "no run.pipeline configured")
		return 1
	}

	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	stepNames := make([]string, len(pipeline))
	for i, step := range pipeline {
		stepNames[i] = step.Name()
	}
	totalSteps := len(stepNames)

	// Find step index by name
	stepIndex := func(name string) int {
		for i, n := range stepNames {
			if n == name {
				return i
			}
		}
		return -1
	}

	now := time.Now()

	// Header
	fmt.Printf("\n    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s\n",
		"ITEM", "PROGRESS", "STATUS", "CREATED", "WALL", "ST TIME", "AI TIME", "COST")
	fmt.Println("    " + strings.Repeat("-", 112))

	for _, epic := range reg.Epics {
		if epic.Status != "active" {
			continue
		}
		epicHasItems := false
		var epicWall, epicST, epicAI time.Duration
		var epicCost float64

		for _, sp := range reg.Sprints {
			if sp.Epic != epic.ID || len(sp.Items) == 0 {
				continue
			}
			if !epicHasItems {
				fmt.Printf("\nEpic: %s\n", epic.Title)
				epicHasItems = true
			}
			var sprintWall, sprintST, sprintAI time.Duration
			var sprintCost float64

			done := 0
			active := 0
			for _, itemID := range sp.Items {
				item, ok := s.Get(itemID)
				if !ok {
					continue
				}
				if cfg.IsTerminalStatus(item.Type, item.Status) {
					done++
				} else if item.Status == "active" {
					active++
				}
			}

			label := "active"
			if sp.Status != "active" {
				label = sp.Status
			}
			stats := fmt.Sprintf("[%d/%d done, %d active]", done, len(sp.Items), active)
		fmt.Printf("  %-40s  %-24s  (%s)\n", sp.Title, stats, label)

			for _, itemID := range sp.Items {
				item, ok := s.Get(itemID)
				if !ok {
					fmt.Printf("    %-8s  ???\n", itemID)
					continue
				}

				// Determine progress
				lastStep, _ := getNestedField(item, "delivery", "last_completed_step")
				stage, _ := getNestedField(item, "delivery", "stage")
				isDone := cfg.IsTerminalStatus(item.Type, item.Status)

				// Progress bar
				completed := 0
				if isDone {
					completed = totalSteps
				} else if lastStep != "" {
					idx := stepIndex(lastStep)
					if idx >= 0 {
						completed = idx + 1
					}
				}

				bar := ""
				for i := 0; i < totalSteps; i++ {
					if i < completed {
						bar += "█"
					} else {
						bar += "░"
					}
				}

				// Status label
				statusLabel := item.Status
				if isDone {
					statusLabel = "done"
				} else if lastStep != "" {
					nextIdx := stepIndex(lastStep) + 1
					if nextIdx < totalSteps {
						statusLabel = stepNames[nextIdx]
					}
				}
				if stage != "" && !isDone {
					statusLabel += " (" + stage + ")"
				}
				// Truncate to fit column
				if len(statusLabel) > 22 {
					statusLabel = statusLabel[:22]
				}

				// In-flight indicator — claimed OR touched in last 60s
				inFlight := ""
				if item.ClaimedBy != "" {
					inFlight = "  << RUNNING"
				} else if !isDone {
					if lt, ok := item.Doc.GetField("last_touched"); ok {
						if touched, err := time.Parse(time.RFC3339, lt); err == nil {
							if now.Sub(touched) < 60*time.Second {
								inFlight = "  << ACTIVE"
							}
						}
					}
				}

				// Wall time: closed = completed_at - started_at, open = now - started_at
				wallStr := ""
				var wallDur time.Duration
				if tt := item.TimeTracking; tt != nil {
					startedStr := ""
					if v, ok := tt["started_at"]; ok {
						if s, ok := v.(string); ok {
							startedStr = s
						}
					}
					if startedStr != "" {
						if started, err := time.Parse(time.RFC3339, startedStr); err == nil {
							if isDone {
								completedStr := ""
								if v, ok := tt["completed_at"]; ok {
									if s, ok := v.(string); ok {
										completedStr = s
									}
								}
								if completedStr != "" {
									if completed, err := time.Parse(time.RFC3339, completedStr); err == nil {
										wallDur = completed.Sub(started)
										wallStr = formatDuration(wallDur)
									}
								}
							} else {
								wallDur = now.Sub(started)
								wallStr = formatDuration(wallDur)
							}
						}
					}
				}

				// ST time (cumulative st run processing)
				stStr := ""
				var stDur time.Duration
				if tt := item.TimeTracking; tt != nil {
					if raw, ok := tt["run_wall_seconds"]; ok {
						var secs float64
						switch v := raw.(type) {
						case float64:
							secs = v
						case int:
							secs = float64(v)
						case string:
							fmt.Sscanf(v, "%f", &secs)
						}
						if secs > 0 {
							stDur = time.Duration(secs) * time.Second
							stStr = formatDuration(stDur)
						}
					}
				}

				// AI time
				aiStr := ""
				var aiDur time.Duration
				if tt := item.TimeTracking; tt != nil {
					if aiRaw, ok := tt["ai_duration_seconds"]; ok {
						var secs float64
						switch v := aiRaw.(type) {
						case float64:
							secs = v
						case int:
							secs = float64(v)
						case string:
							fmt.Sscanf(v, "%f", &secs)
						}
						if secs > 0 {
							aiDur = time.Duration(secs) * time.Second
							aiStr = formatDuration(aiDur)
						}
					}
				}

				// Cost
				costStr := ""
				var itemCost float64
				if tt := item.TimeTracking; tt != nil {
					if costRaw, ok := tt["ai_cost_usd"]; ok {
						switch v := costRaw.(type) {
						case float64:
							itemCost = v
						case string:
							fmt.Sscanf(v, "%f", &itemCost)
						}
						if itemCost > 0 {
							costStr = fmt.Sprintf("$%.2f", itemCost)
						}
					}
				}

				// Accumulate sprint totals
				sprintWall += wallDur
				sprintST += stDur
				sprintAI += aiDur
				sprintCost += itemCost

				// Created date
				createdStr := ""
				if created, ok := item.Doc.GetField("created"); ok {
					if t, err := time.Parse(time.RFC3339, created); err == nil {
						createdStr = t.Format("Jan 02")
					}
				}

				// Format: title on first row, data on second row
				title := item.Title
				if len(title) > 80 {
					title = title[:77] + "..."
				}
				fmt.Printf("      %s\n", title)
				fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s%s\n",
					itemID, bar, statusLabel, createdStr, wallStr, stStr, aiStr, costStr, inFlight)
			}

			// Sprint subtotal (always printed)
			{
				sprintWallStr := ""
				if sprintWall > 0 {
					sprintWallStr = formatDuration(sprintWall)
				}
				sprintSTStr := ""
				if sprintST > 0 {
					sprintSTStr = formatDuration(sprintST)
				}
				sprintAIStr := ""
				if sprintAI > 0 {
					sprintAIStr = formatDuration(sprintAI)
				}
				sprintCostStr := ""
				if sprintCost > 0 {
					sprintCostStr = fmt.Sprintf("$%.2f", sprintCost)
				}
				fmt.Printf("    %s\n", strings.Repeat("─", 112))
				fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s\n",
					"", "", fmt.Sprintf("%d/%d done", done, len(sp.Items)), "", sprintWallStr, sprintSTStr, sprintAIStr, sprintCostStr)
			}

			// Accumulate epic totals
			epicWall += sprintWall
			epicST += sprintST
			epicAI += sprintAI
			epicCost += sprintCost
		}

		// Epic grand total
		if epicHasItems && (epicWall > 0 || epicCost > 0) {
			epicWallStr := ""
			if epicWall > 0 {
				epicWallStr = formatDuration(epicWall)
			}
			epicSTStr := ""
			if epicST > 0 {
				epicSTStr = formatDuration(epicST)
			}
			epicAIStr := ""
			if epicAI > 0 {
				epicAIStr = formatDuration(epicAI)
			}
			epicCostStr := ""
			if epicCost > 0 {
				epicCostStr = fmt.Sprintf("$%.2f", epicCost)
			}
			fmt.Printf("\n    %s\n", strings.Repeat("═", 112))
			fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s\n",
				"TOTAL", "", epic.Title, "", epicWallStr, epicSTStr, epicAIStr, epicCostStr)
			fmt.Printf("    %s\n", strings.Repeat("═", 112))
		}
	}
	// Legend
	fmt.Println()
	fmt.Println("  ---------------------------------------------------------------")
	fmt.Println("  Progress:  █ complete   ░ remaining")
	fmt.Println("  Status:    << RUNNING   currently being processed by st run")
	fmt.Println("             << ACTIVE    step completed in the last 60s")
	fmt.Println()
	// Wrap pipeline at ~70 chars
	fmt.Printf("  Pipeline:  ")
	col := 13 // "  Pipeline:  " is 13 chars
	for i, name := range stepNames {
		sep := ""
		if i > 0 {
			sep = " > "
		}
		if col+len(sep)+len(name) > 70 && i > 0 {
			fmt.Printf("\n             ")
			col = 13
			sep = ""
		}
		fmt.Printf("%s%s", sep, name)
		col += len(sep) + len(name)
	}
	fmt.Println()
	fmt.Println("  ---------------------------------------------------------------")
	return 0
}

// autoParallelism determines how many items can safely run in parallel
// by analyzing which repos each item touches. Items touching different
// repos can run concurrently; items sharing a repo must be sequential.
func autoParallelism(s *store.Store, cfg *config.Config, itemIDs []string) int {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return 1
	}

	// Build a map of item -> repos it touches
	itemRepos := make(map[string]map[string]bool)
	for _, id := range itemIDs {
		repos := make(map[string]bool)
		item, ok := s.Get(id)
		if !ok {
			repos["unknown"] = true
			itemRepos[id] = repos
			continue
		}

		// Check PR manifest for repo info
		if item.Manifest != nil {
			if prsRaw, ok := item.Manifest["prs"]; ok {
				if prsStr, ok := prsRaw.(string); ok {
					for _, pr := range strings.Split(prsStr, ",") {
						pr = strings.TrimSpace(pr)
						if idx := strings.Index(pr, "#"); idx > 0 {
							repos[pr[:idx]] = true
						}
					}
				}
			}
		}

		// Check plan sidecar for scope_repos
		if len(repos) == 0 {
			if p, err := plan.Load(cfg.PlansDir(), id); err == nil && p != nil && len(p.ScopeRepos) > 0 {
				for _, r := range p.ScopeRepos {
					repos[r] = true
				}
			}
		}

		// If no PR or plan, check which worktree dirs have changes
		if len(repos) == 0 {
			dirs := allWorktreeDirs(cfg, id)
			for _, dir := range dirs {
				out, _, _ := runCmdInDir(dir, "git diff --stat main 2>/dev/null")
				if len(strings.TrimSpace(string(out))) > 0 {
					repos[filepath.Base(dir)] = true
				}
			}
		}

		// If still empty (new item, no work yet), assume all repos
		if len(repos) == 0 {
			for _, r := range cfg.Worktree.Repos {
				repos[r] = true
			}
		}

		itemRepos[id] = repos
	}

	// Find max set of non-overlapping items (greedy)
	used := make(map[string]bool) // repos already claimed
	parallel := 0
	for _, id := range itemIDs {
		repos := itemRepos[id]
		conflict := false
		for repo := range repos {
			if used[repo] {
				conflict = true
				break
			}
		}
		if !conflict {
			parallel++
			for repo := range repos {
				used[repo] = true
			}
		}
	}

	if parallel < 1 {
		parallel = 1
	}

	if parallel > 1 {
		fmt.Printf("  auto-parallel: %d items can run concurrently (no repo overlap)\n", parallel)
	}

	return parallel
}

func RunItem(s *store.Store, cfg *config.Config, itemID string, opts RunOpts, engine RunEngine) int {
	pipeline := cfg.RunPipeline()
	if len(pipeline) == 0 {
		fmt.Fprintln(os.Stderr, "no run.pipeline configured")
		return 1
	}

	item, ok := s.Get(itemID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", itemID)
		return 1
	}

	// If item has a sprint, run within that sprint context
	if item.Sprint != "" {
		opts.ItemFilter = itemID
		return Run(s, cfg, item.Sprint, opts, engine)
	}

	// No sprint — run standalone (single item, no sprint validation)
	fmt.Printf("Running %s standalone (no sprint)\n", itemID)
	opts.ItemFilter = itemID
	result := runSingleItem(s, cfg, itemID, "", pipeline, opts, engine)
	if result.Success {
		fmt.Printf("\nDone: %s\n", itemID)
		return 0
	}
	fmt.Printf("\nFailed: %s\n", itemID)
	return 1
}

// Run executes a full autonomous sprint loop.
func Run(s *store.Store, cfg *config.Config, sprintID string, opts RunOpts, engine RunEngine) int {
	// Load sprint and validate
	pipeline := cfg.RunPipeline()
	if len(pipeline) == 0 {
		fmt.Fprintln(os.Stderr, "no run.pipeline configured — define run.step_order and run.steps in config")
		return 1
	}

	groups, sp, code := loadSprintGroups(s, cfg, sprintID)
	if code != 0 {
		return code
	}

	if !sp.PlanApproved && opts.ItemFilter == "" {
		// Sprint plan approval only required when running the full sprint
		fmt.Fprintf(os.Stderr, "sprint %s plan not approved — use `st run` (no args) for interactive approval\n", sprintID)
		return 1
	}

	if opts.DryRun {
		return printDryRun(s, cfg, sp, groups, pipeline, opts)
	}

	// Ensure AWS credentials are valid (for evidence uploads)
	ensureAWSCredentials(cfg)

	// Recover items left in broken state from previous failed runs
	recoverStaleItems(s, cfg, sp.Items)
	// Reload store after recovery
	s, _ = store.New(cfg)

	// Set up Ctrl+C handler — first press kills current subprocess and pauses,
	// second press hard-exits.
	activeSigChan = make(chan os.Signal, 1)
	signal.Notify(activeSigChan, os.Interrupt)
	pauseRequested.Store(0)
	resetRunCtx() // creates activeRunCtx + arms signal handler

	// Execute groups sequentially, items within groups up to parallelism
	start := time.Now()
	var allResults []ItemResult

	for i, group := range groups {
		if activeRunCtx != nil && activeRunCtx.Err() != nil {
			break
		}

		fmt.Printf("\n=== Group %d/%d ===\n", i+1, len(groups))
		results := runGroup(s, cfg, group, sprintID, pipeline, opts, engine)
		allResults = append(allResults, results...)
	}
	signal.Stop(activeSigChan)
	if activeRunCancel != nil {
		activeRunCancel()
	}
	activeRunCtx = nil
	activeRunCancel = nil
	activeSigChan = nil
	pauseRequested.Store(0)

	// Clean up any items that were started but didn't complete
	for _, r := range allResults {
		if !r.Success {
			releaseItem(cfg, r.ItemID)
		}
	}

	// Completion report
	printCompletionReport(allResults, sprintID, time.Since(start))

	// Check if sprint is now complete — all items done
	checkSprintCompletion(cfg, sprintID)

	for _, r := range allResults {
		if !r.Success {
			return 1
		}
	}
	return 0
}

// checkSprintCompletion checks if all items in the sprint are terminal,
// and if so, archives the sprint. If all sprints in the epic are done,
// archives the epic too.
func checkSprintCompletion(cfg *config.Config, sprintID string) {
	s, err := store.New(cfg)
	if err != nil {
		return
	}
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return
	}

	sp, err := reg.SprintByID(sprintID)
	if err != nil {
		return
	}

	// Check if all items are terminal
	allDone := true
	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		if !cfg.IsTerminalStatus(item.Type, item.Status) {
			allDone = false
			break
		}
	}

	if !allDone {
		return
	}

	// Archive sprint
	fmt.Printf("\n[sprint] All %d items complete — archiving sprint %s\n", len(sp.Items), sp.Title)
	sp.Status = "archived"
	reg.Save(cfg.EpicsPath())

	// Check if all sprints in the epic are archived
	epicID := sp.Epic
	allSprintsDone := true
	for _, es := range reg.Sprints {
		if es.Epic == epicID && es.Status != "archived" {
			allSprintsDone = false
			break
		}
	}

	if allSprintsDone && epicID != "" {
		for i := range reg.Epics {
			if reg.Epics[i].ID == epicID {
				fmt.Printf("[epic] All sprints complete — archiving epic %s\n", reg.Epics[i].Title)
				reg.Epics[i].Status = "archived"
				reg.Save(cfg.EpicsPath())
				break
			}
		}
	}
}

// Advance finds the next unblocked item and runs its pipeline steps.
func Advance(s *store.Store, cfg *config.Config, sprintID string, opts RunOpts, engine RunEngine) int {
	pipeline := cfg.RunPipeline()
	if len(pipeline) == 0 {
		fmt.Fprintln(os.Stderr, "no run.pipeline configured")
		return 1
	}

	groups, _, code := loadSprintGroups(s, cfg, sprintID)
	if code != 0 {
		return code
	}

	// Find first eligible item across all groups
	itemID := ""
	if opts.ItemFilter != "" {
		itemID = opts.ItemFilter
	} else {
		for _, group := range groups {
			for _, id := range group {
				if isEligible(s, cfg, id) {
					itemID = id
					break
				}
			}
			if itemID != "" {
				break
			}
		}
	}

	if itemID == "" {
		fmt.Println("No unblocked items remaining in sprint")
		return 0
	}

	item, _ := s.Get(itemID)
	if opts.DryRun {
		fmt.Printf("Would advance: %s — %s\n", itemID, item.Title)
		fmt.Println("Steps:")
		for i, step := range pipeline {
			fmt.Printf("  %d. [%s] %s\n", i+1, step.Type, step.Name())
			if opts.StepFilter != "" && step.Name() == opts.StepFilter {
				fmt.Println("  (--step reached, would stop here)")
				break
			}
		}
		return 0
	}

	fmt.Printf("Advancing: %s — %s\n", itemID, item.Title)
	result := runSingleItem(s, cfg, itemID, sprintID, pipeline, opts, engine)

	if result.Success {
		fmt.Printf("\nDone: %s (cost: $%.2f)\n", itemID, result.TotalCost)
	} else {
		fmt.Printf("\nFailed: %s\n", itemID)
		for _, sr := range result.Steps {
			if !sr.Passed {
				fmt.Printf("  step %s failed: %s\n", sr.Step, sr.Error)
				break
			}
		}
		return 1
	}
	return 0
}

// --- Internal execution ---

func runGroup(s *store.Store, cfg *config.Config, group []string, sprintID string, pipeline []config.RunStepDef, opts RunOpts, engine RunEngine) []ItemResult {
	// Filter to eligible items
	var eligible []string
	for _, id := range group {
		if opts.ItemFilter != "" && id != opts.ItemFilter {
			continue
		}
		if isEligible(s, cfg, id) {
			eligible = append(eligible, id)
		}
	}

	if len(eligible) == 0 {
		return nil
	}

	// Determine parallelism
	maxPar := opts.Parallelism
	if maxPar <= 0 && cfg.Run != nil && cfg.Run.MaxParallelism > 0 {
		maxPar = cfg.Run.MaxParallelism
	}
	if maxPar <= 0 {
		maxPar = 1
	}

	// Auto-parallelism: analyze repo overlap to find safe concurrency
	if maxPar == 1 && len(eligible) > 1 && cfg.Run != nil && cfg.Run.AutoParallel {
		maxPar = autoParallelism(s, cfg, eligible)
	}

	if maxPar > len(eligible) {
		maxPar = len(eligible)
	}

	results := make([]ItemResult, len(eligible))
	sem := make(chan struct{}, maxPar)
	var wg sync.WaitGroup

	for i, itemID := range eligible {
		// Check for interrupt or pause before starting next item
		if activeRunCtx != nil && activeRunCtx.Err() != nil {
			break
		}
		if pauseRequested.Load() != 0 {
			break // don't start new items while paused
		}
		wg.Add(1)
		sem <- struct{}{} // acquire — may block waiting for previous item

		// Re-check pause after potentially blocking on semaphore
		if pauseRequested.Load() != 0 || (activeRunCtx != nil && activeRunCtx.Err() != nil) {
			<-sem // release
			wg.Done()
			break
		}

		go func(idx int, id string) {
			defer wg.Done()
			defer func() { <-sem }() // release
			results[idx] = runSingleItem(s, cfg, id, sprintID, pipeline, opts, engine)
		}(i, itemID)
	}

	wg.Wait()
	return results
}

// gateMu serializes gate prompts when parallelism > 1.
var gateMu sync.Mutex

// activeRunCtx is the cancel context for the current st run — Ctrl+C cancels it.
var activeRunCtx context.Context

// activeRunCancel cancels activeRunCtx. Set by Run() and resetRunCtx().
var activeRunCancel context.CancelFunc

// activeSigChan receives os.Interrupt for the current run.
var activeSigChan chan os.Signal

// pauseRequested is set to 1 by Ctrl+C. The step loop checks it between steps
// and shows an interactive menu instead of killing the process immediately.
var pauseRequested atomic.Int32

// resetRunCtx creates a fresh context after Ctrl+C cancellation.
// Called when user chooses "continue" from pause menu.
func resetRunCtx() {
	ctx, cancel := context.WithCancel(context.Background())
	activeRunCtx = ctx
	activeRunCancel = cancel
	// Re-arm signal handler
	go func() {
		select {
		case <-activeSigChan:
			fmt.Fprintf(os.Stderr, "\n[st run] Ctrl+C received — stopping current step...\n")
			pauseRequested.Store(1)
			cancel()
			// Second Ctrl+C = hard exit
			select {
			case <-activeSigChan:
				fmt.Fprintf(os.Stderr, "\n[st run] Force exit.\n")
				os.Exit(1)
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()
}

func runSingleItem(s *store.Store, cfg *config.Config, itemID, sprintID string, pipeline []config.RunStepDef, opts RunOpts, engine RunEngine) ItemResult {
	start := time.Now()
	result := ItemResult{ItemID: itemID}

	// Reload store for parallel safety
	localStore, err := store.New(cfg)
	if err != nil {
		result.Steps = append(result.Steps, StepResult{Step: "init", Error: err.Error()})
		return result
	}

	item, ok := localStore.Get(itemID)
	if !ok {
		result.Steps = append(result.Steps, StepResult{Step: "init", Error: "item not found"})
		return result
	}
	result.Title = item.Title

	// Check if item is already done (terminal status)
	if cfg.IsTerminalStatus(item.Type, item.Status) {
		fmt.Printf("[%s] Already closed (%s) — skipping\n", itemID, item.Status)
		result.Success = true
		result.Duration = time.Since(start)
		return result
	}

	// Check if PR is already merged — advance past merge and continue pipeline
	// (still need deploy verification, smoke, UAT, and user approval)
	if detectMergedPR(cfg, itemID, item) {
		fmt.Printf("[%s] PR already merged — advancing to post-merge steps\n", itemID)
		setNestedField(item, "delivery", "stage", "merged")
		setNestedField(item, "delivery", "last_completed_step", "merge")
		item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
		localStore.Write(item)
		// Reload and continue — the resume logic will skip to deploy_watch
		localStore, _ = store.New(cfg)
	}

	// Start the item if not already active (creates worktrees + claims)
	tc, _ := cfg.Types[item.Type]
	if item.Status == tc.StartStatus {
		fmt.Printf("[%s] Starting item...\n", itemID)
		slug := slugFromTitle(item.Title)
		startCode := Start(localStore, cfg, itemID, StartOpts{Slug: slug})
		if startCode != 0 {
			result.Steps = append(result.Steps, StepResult{Step: "start", Error: "st start failed"})
			return result
		}
		// Reload after start
		localStore, _ = store.New(cfg)
	}

	// Resolve worktree directory — prefer the repo that has a PR (if any),
	// otherwise fall back to the first available repo worktree.
	worktreeDir := resolveWorktreeDirWithPR(cfg, itemID)

	// Reuse existing claude session if resuming (preserves context across retries).
	// Store the session ID on the item so subsequent runs can --resume it.
	claudeSessionID := ""
	if item, ok := localStore.Get(itemID); ok {
		if sid, _ := getNestedField(item, "delivery", "claude_session_id"); sid != "" {
			claudeSessionID = sid
		}
	}
	isNewSession := claudeSessionID == ""
	if isNewSession {
		claudeSessionID = generateSessionID()
		if progressStore, err := store.New(cfg); err == nil {
			if progressItem, ok := progressStore.Get(itemID); ok {
				setNestedField(progressItem, "delivery", "claude_session_id", claudeSessionID)
				progressStore.Write(progressItem)
			}
		}
	}
	claudeStepCount := 0

	// Always record metrics on exit — success, failure, or Ctrl+C
	defer func() {
		result.Duration = time.Since(start)
		recordRunMetrics(cfg, itemID, result)
	}()

	// Build breakpoints set from config
	breakpoints := make(map[string]bool)
	if cfg.Run != nil {
		for _, bp := range cfg.Run.Breakpoints {
			breakpoints[bp] = true
		}
	}

	// Resume from last completed step if the item has progress
	startIdx := 0
	if !opts.Fresh {
		if item, ok := localStore.Get(itemID); ok {
			if lastStep, _ := getNestedField(item, "delivery", "last_completed_step"); lastStep != "" {
				for j, s := range pipeline {
					if s.Name() == lastStep {
						startIdx = j + 1
						break
					}
				}
				if startIdx > 0 && startIdx < len(pipeline) {
					fmt.Printf("[%s] Resuming after step: %s\n", itemID, lastStep)
				} else if startIdx >= len(pipeline) {
					fmt.Printf("[%s] All steps already completed\n", itemID)
					result.Success = true
					return result
				}
			}
		}
	}

	// Execute each pipeline step (index-based to support skip + resume)
	for i := startIdx; i < len(pipeline); i++ {
		step := pipeline[i]
		stepStart := time.Now()
		// Track which claude invocation this is for session reuse.
		// Resume if: (a) 2nd+ claude step in this run, or (b) reusing
		// a session from a previous run.
		isResume := false
		if step.Type == "claude" {
			claudeStepCount++
			if claudeStepCount > 1 || !isNewSession {
				isResume = true
			}
		}
		sr := executeStepWithSession(localStore, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID, isResume)
		sr.Duration = time.Since(stepStart)
		result.Steps = append(result.Steps, sr)
		result.TotalCost += sr.CostUSD

		if !sr.Passed {
			fmt.Printf("[%s] Step %s FAILED: %s\n", itemID, step.Name(), sr.Error)

			// For CI failures, fix inline: get the failure log, feed
			// it to claude, retry CI. Keep going as long as the error
			// changes (= progress). Pause only after 3 consecutive
			// identical errors.
			//
			// Skip fix loop if interrupted (Ctrl+C / context cancelled) —
			// go straight to pause menu instead.
			interrupted := pauseRequested.Load() != 0 || (activeRunCtx != nil && activeRunCtx.Err() != nil)
			if step.Type != "gate" && step.Type != "close" && !interrupted {
				fixed := false
				lastError := sr.Error
				sameErrorCount := 0
				const maxSameError = 3

				stepLabel := "CI"
				if step.Type == "uat" {
					stepLabel = "UAT"
				}
				for attempt := 1; ; attempt++ {
					// Check for interruption before each attempt
					if pauseRequested.Load() != 0 || (activeRunCtx != nil && activeRunCtx.Err() != nil) {
						fmt.Printf("[%s] Interrupted — skipping fix attempts\n", itemID)
						break
					}
					fmt.Printf("[%s] %s fix attempt %d...\n", itemID, stepLabel, attempt)
					fixPrompt := fmt.Sprintf(
						"The %s step failed for item %s (attempt %d). The error was:\n\n%s\n\n"+
							"Investigate the failure and find the root cause.\n\n"+
							"IMPORTANT: The goal is to verify the IMPLEMENTATION is correct, not to make tests pass.\n"+
							"- If the code has a real bug, fix the code.\n"+
							"- If a test is wrong (testing the wrong thing), fix the test to correctly verify the implementation.\n"+
							"- If an acceptance criterion has escaping/path issues, fix the AC command — but make sure the fixed command still validates the actual implementation.\n"+
							"- NEVER weaken a test or remove a check just to make it pass. Every AC must meaningfully verify the feature works.\n\n"+
							"Commit and push any fixes. Do NOT merge. Follow all procedures in CLAUDE.md.",
						stepLabel, itemID, attempt, sr.Error)
					fixStep := config.RunStepDef{Type: "claude", Prompt: fixPrompt}
					fixStep.SetName(fmt.Sprintf("ci_fix_%d", attempt))
					fixSR := executeClaude(s, cfg, itemID, sprintID, fixStep, opts, engine, worktreeDir, claudeSessionID, true)
					result.Steps = append(result.Steps, fixSR)
					result.TotalCost += fixSR.CostUSD

					if !fixSR.Passed {
						fmt.Printf("[%s] Fix attempt %d failed to run\n", itemID, attempt)
						sameErrorCount++
					} else {
						// Retry the CI step
						fmt.Printf("[%s] Retrying %s after fix...\n", itemID, step.Name())
						localStore, _ = store.New(cfg)
						sr2 := executeStepWithSession(localStore, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID, true)
						sr2.Duration = time.Since(stepStart)
						result.Steps = append(result.Steps, sr2)
						result.TotalCost += sr2.CostUSD

						if sr2.Passed {
							fmt.Printf("[%s] Step %s OK after fix attempt %d (%s)\n", itemID, step.Name(), attempt, sr2.Duration.Round(time.Second))
							if progressStore, err := store.New(cfg); err == nil {
								if progressItem, ok := progressStore.Get(itemID); ok {
									setNestedField(progressItem, "delivery", "last_completed_step", step.Name())
									progressStore.Write(progressItem)
								}
							}
							localStore, _ = store.New(cfg)
							fixed = true
							break
						}

						// Track whether error changed (= progress)
						if sr2.Error == lastError {
							sameErrorCount++
						} else {
							sameErrorCount = 1 // new error, reset counter
							fmt.Printf("[%s] Error changed — still making progress\n", itemID)
						}
						lastError = sr2.Error
						sr = sr2
					}

					// Pause only after same error repeats
					if sameErrorCount >= maxSameError {
						fmt.Printf("[%s] Same error %d times — pausing for input.\n", itemID, sameErrorCount)
						action := showPauseMenu(itemID, step.Name(), step.Name(), result, engine)
						pauseRequested.Store(0)
						resetRunCtx() // fresh context so claude can run again
						switch action {
						case "continue":
							sameErrorCount = 0 // reset and keep trying
						case "skip":
							result.Steps = append(result.Steps, StepResult{
								Step: step.Name(), Type: "skipped", Passed: true,
							})
							fixed = true // not really fixed, but skip means move on
							break
						case "abort":
							break
						}
						if action == "abort" || action == "skip" {
							break
						}
					}
				}

				if fixed {
					continue // proceed to next pipeline step
				}
			}

			releaseItem(cfg, itemID)
			return result // defer records metrics
		}

		fmt.Printf("[%s] Step %s OK (%s)\n", itemID, step.Name(), sr.Duration.Round(time.Second))

		// Record progress so we can resume from here if interrupted
		if progressStore, err := store.New(cfg); err == nil {
			if progressItem, ok := progressStore.Get(itemID); ok {
				setNestedField(progressItem, "delivery", "last_completed_step", step.Name())
				progressStore.Write(progressItem)
			}
		}

		// Reload store after each step (other steps may have modified the item)
		localStore, _ = store.New(cfg)

		// Check if item was closed by this step (e.g., claude detected merged PR and closed it)
		if updatedItem, ok := localStore.Get(itemID); ok && cfg.IsTerminalStatus(updatedItem.Type, updatedItem.Status) {
			fmt.Printf("[%s] Item closed during %s — skipping remaining steps\n", itemID, step.Name())
			break
		}

		// Check if worktree was removed (e.g., st finish cleaned it up)
		if _, err := os.Stat(worktreeDir); err != nil && worktreeDir != cfg.Root() {
			fmt.Printf("[%s] Worktree removed — skipping remaining steps\n", itemID)
			break
		}

		// Stop at --step filter
		if opts.StepFilter != "" && step.Name() == opts.StepFilter {
			break
		}

		// Check for pause (Ctrl+C or breakpoint) between steps
		if i+1 < len(pipeline) {
			nextStep := pipeline[i+1].Name()
			shouldPause := pauseRequested.Load() != 0
			if !shouldPause && breakpoints[nextStep] {
				shouldPause = true
				fmt.Printf("\n[%s] Breakpoint before step: %s\n", itemID, nextStep)
			}
			if shouldPause {
				action := showPauseMenu(itemID, step.Name(), nextStep, result, engine)
				pauseRequested.Store(0) // clear the flag after handling
				// Reset context so subsequent steps can launch subprocesses
				resetRunCtx()
				switch action {
				case "continue":
					// proceed to next step
				case "skip":
					fmt.Printf("[%s] Skipping step: %s\n", itemID, nextStep)
					result.Steps = append(result.Steps, StepResult{
						Step: nextStep, Type: "skipped", Passed: true,
					})
					// Record skipped step on item so close gate can reject
					if ps, err := store.New(cfg); err == nil {
						if pi, ok := ps.Get(itemID); ok {
							existing, _ := getNestedField(pi, "delivery", "skipped_steps")
							if existing == "" {
								setNestedField(pi, "delivery", "skipped_steps", nextStep)
							} else {
								setNestedField(pi, "delivery", "skipped_steps", existing+","+nextStep)
							}
							ps.Write(pi)
						}
					}
					i++ // advance past the skipped step
				case "abort":
					fmt.Printf("[%s] Aborted by user\n", itemID)
					releaseItem(cfg, itemID)
					return result
				}
			}
		}
	}

	result.Success = true
	return result
}

// recordRunMetrics accumulates AI cost, AI duration, and run wall time on the item.
// Each st run / st advance invocation adds to the running totals.
func recordRunMetrics(cfg *config.Config, itemID string, result ItemResult) {
	localStore, err := store.New(cfg)
	if err != nil {
		return
	}
	item, ok := localStore.Get(itemID)
	if !ok {
		return
	}

	// Accumulate AI cost
	if result.TotalCost > 0 {
		prev := readFloatField(item, "time_tracking", "ai_cost_usd")
		setNestedField(item, "time_tracking", "ai_cost_usd", fmt.Sprintf("%.4f", prev+result.TotalCost))
	}

	// Accumulate AI duration from claude's reported duration_ms (not wall clock)
	var aiDurationMs int64
	for _, sr := range result.Steps {
		if sr.Type == "claude" {
			aiDurationMs += sr.AIDurationMs
		}
	}
	if aiDurationMs > 0 {
		prev := readIntField(item, "time_tracking", "ai_duration_seconds")
		setNestedField(item, "time_tracking", "ai_duration_seconds", fmt.Sprintf("%d", prev+int(aiDurationMs/1000)))
	}

	// Accumulate total run wall time
	if result.Duration > 0 {
		prev := readIntField(item, "time_tracking", "run_wall_seconds")
		setNestedField(item, "time_tracking", "run_wall_seconds", fmt.Sprintf("%d", prev+int(result.Duration.Seconds())))
	}

	// Track number of st run invocations
	prevRuns := readIntField(item, "time_tracking", "run_count")
	setNestedField(item, "time_tracking", "run_count", fmt.Sprintf("%d", prevRuns+1))

	// Append per-run stats to ai_sessions array (detailed provenance)
	appendAISessionRecord(item, result)

	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
	localStore.Write(item)
}

// appendAISessionRecord adds a line to the work_tracking.ai_sessions list
// with per-invocation stats: session ID, step, cost, duration, timestamp.
func appendAISessionRecord(item *model.Item, result ItemResult) {
	for _, sr := range result.Steps {
		if sr.Type != "claude" || sr.CostUSD == 0 {
			continue
		}
		// Format: "cost:$X.XXXX duration:Xs step:<name> at:<timestamp>"
		aiDur := time.Duration(sr.AIDurationMs) * time.Millisecond
		record := fmt.Sprintf("cost:$%.4f duration:%s step:%s at:%s",
			sr.CostUSD, aiDur.Round(time.Second), sr.Step,
			time.Now().Format(time.RFC3339))

		if item.WorkTracking == nil {
			item.WorkTracking = make(map[string]interface{})
		}

		// Append to ai_sessions list in document
		appendListField(item, "work_tracking", "ai_sessions", record)
	}
}

// appendListField appends a value to a list field under a parent block in the document.
func appendListField(item *model.Item, parent, key, val string) {
	if item.Doc == nil {
		return
	}

	// Find or create the parent block, then find or create the key as a list
	parentIdx := -1
	keyIdx := -1
	lastInBlock := -1
	for i, line := range item.Doc.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
		}
		if parentIdx >= 0 && i > parentIdx {
			if line.Indent == 0 && !line.IsEmpty && line.Key != "" {
				break // left the parent block
			}
			if line.Key == key && line.Indent > 0 {
				keyIdx = i
			}
			lastInBlock = i
		}
	}

	newLine := model.Line{
		Raw:      fmt.Sprintf("  - %s", val),
		Indent:   2,
		BlockKey: parent,
	}

	if parentIdx < 0 {
		// Create parent + key + value
		item.Doc.Lines = append(item.Doc.Lines,
			model.Line{Raw: "", IsEmpty: true},
			model.Line{Raw: parent + ":", Key: parent},
			model.Line{Raw: "  " + key + ":", Key: key, Indent: 2, BlockKey: parent},
			newLine,
		)
		return
	}

	if keyIdx < 0 {
		// Parent exists but key doesn't — insert at end of parent block
		insertAt := lastInBlock + 1
		if insertAt <= parentIdx {
			insertAt = parentIdx + 1
		}
		lines := make([]model.Line, 0, len(item.Doc.Lines)+2)
		lines = append(lines, item.Doc.Lines[:insertAt]...)
		lines = append(lines, model.Line{Raw: "  " + key + ":", Key: key, Indent: 2, BlockKey: parent})
		lines = append(lines, newLine)
		lines = append(lines, item.Doc.Lines[insertAt:]...)
		item.Doc.Lines = lines
		return
	}

	// Key exists — find the end of the list and append
	insertAt := keyIdx + 1
	for insertAt < len(item.Doc.Lines) {
		line := item.Doc.Lines[insertAt]
		if line.Indent < 2 || (line.Key != "" && !strings.HasPrefix(line.Raw, "  -")) {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line.Raw), "- ") {
			insertAt++
			continue
		}
		break
	}

	lines := make([]model.Line, 0, len(item.Doc.Lines)+1)
	lines = append(lines, item.Doc.Lines[:insertAt]...)
	lines = append(lines, newLine)
	lines = append(lines, item.Doc.Lines[insertAt:]...)
	item.Doc.Lines = lines
}

func readFloatField(item *model.Item, parent, key string) float64 {
	if val, exists := getNestedField(item, parent, key); exists {
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	}
	return 0
}

func readIntField(item *model.Item, parent, key string) int {
	if val, exists := getNestedField(item, parent, key); exists {
		var i int
		fmt.Sscanf(val, "%d", &i)
		return i
	}
	return 0
}

// executeStepWithSession dispatches to the appropriate step executor, with claude session reuse.
func executeStepWithSession(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, isResume bool) StepResult {
	switch step.Type {
	case "plan":
		return executePlanWithOpts(s, cfg, itemID, engine, opts, worktreeDir)
	case "claude":
		return executeClaude(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID, isResume)
	case "test":
		return executeTest(s, cfg, itemID, step, worktreeDir)
	case "verify_tests":
		return executeVerifyTests(s, cfg, itemID)
	case "pr":
		return executePR(s, cfg, itemID, step, worktreeDir)
	case "merge":
		return executeMerge(s, cfg, itemID, worktreeDir)
	case "merge_precheck":
		return executeMergePrecheck(cfg, itemID, worktreeDir)
	case "deploy":
		return executeDeploy(s, cfg, itemID, worktreeDir)
	case "smoke":
		return executeSmoke(s, cfg, itemID, worktreeDir)
	case "uat":
		return executeUAT(s, cfg, itemID, worktreeDir)
	case "gate":
		return executeGate(itemID, engine)
	case "uat_review":
		return executeUATReview(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID)
	case "close":
		return executeClose(s, cfg, itemID, step)
	case "command":
		return executeCommand(cfg, itemID, sprintID, step, worktreeDir)
	default:
		return StepResult{Step: step.Name(), Type: step.Type, Error: fmt.Sprintf("unknown step type: %s", step.Type)}
	}
}

// --- Step executors ---

func executeClaude(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, isResume bool) StepResult {
	sr := StepResult{Step: step.Name(), Type: "claude"}

	// Build prompt
	prompt := step.Prompt
	if prompt == "" {
		prompt = buildDefaultPrompt(s, cfg, itemID, sprintID)
	} else {
		prompt = expandTemplate(prompt, itemID, sprintID, worktreeDir, cfg)
	}

	// Always inject full item context so claude never has to rediscover
	prompt += buildItemContext(s, cfg, itemID, worktreeDir)

	// Per-step budget override
	stepOpts := opts
	if step.Budget > 0 {
		stepOpts.MaxBudgetUSD = step.Budget
	}

	// Build args — resume existing session for 2nd+ claude step
	args := buildClaudeArgs(cfg, prompt, stepOpts, worktreeDir)
	if isResume {
		args = append(args, "--resume", claudeSessionID)
	} else {
		args = append(args, "--session-id", claudeSessionID)
	}

	// Build env with unique session ID + context for status display
	sessionID := generateSessionID()
	env := []string{
		"AS_SESSION_ID=" + sessionID,
		"ST_RUN_ITEM=" + itemID,
		"ST_RUN_STEP=" + step.Name(),
	}
	if agentID := cfg.AgentID(); agentID != "" {
		env = append(env, "AS_AGENT_ID="+agentID)
	}

	// Record session on item
	recordSession(s, cfg, itemID, sessionID, step.Name())

	// Launch
	output, exitCode, err := engine.RunClaude(worktreeDir, args, env)
	if err != nil {
		sr.Error = fmt.Sprintf("exec error: %v", err)
		return sr
	}

	// Parse JSON output
	claudeResult, parseErr := parseClaudeOutput(output)
	if parseErr != nil {
		// Non-JSON output — still check exit code
		if exitCode != 0 {
			sr.Error = fmt.Sprintf("claude exited %d", exitCode)
			sr.Output = truncate(string(output), 500)
			return sr
		}
		// Success but no JSON — treat as OK
		sr.Passed = true
		sr.Output = truncate(string(output), 500)
		return sr
	}

	sr.CostUSD = claudeResult.TotalCostUSD
	sr.AIDurationMs = claudeResult.DurationMs
	sr.Output = truncate(claudeResult.Result, 500)

	if exitCode != 0 || (claudeResult.Subtype != "" && claudeResult.Subtype != "success") {
		sr.Error = fmt.Sprintf("claude exited %d (subtype: %s)", exitCode, claudeResult.Subtype)
		return sr
	}

	sr.Passed = true
	return sr
}

func executeTest(s *store.Store, cfg *config.Config, itemID string, step config.RunStepDef, worktreeDir string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "test"}
	suite := step.Command // command field carries the suite name
	if suite == "" {
		sr.Error = "test step requires command field set to suite name"
		return sr
	}
	code := TestRecord(s, cfg, itemID, suite, TestRecordOpts{
		Run:      true,
		Coverage: step.Coverage,
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(worktreeDir, cmd)
		},
	})
	if code != 0 {
		sr.Error = fmt.Sprintf("st test %s exited %d", suite, code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executePR(s *store.Store, cfg *config.Config, itemID string, step config.RunStepDef, worktreeDir string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "pr"}

	// Detect and record PRs from all repo worktrees
	prDirs := allWorktreeDirsWithPR(cfg, itemID)
	if len(prDirs) == 0 {
		// Fallback: check default worktreeDir
		cmd := ghPRCmd(worktreeDir, "view --json number -q .number")
		out, exitCode, _ := runCmdInDir(worktreeDir, cmd+" 2>/dev/null")
		if exitCode == 0 && strings.TrimSpace(string(out)) != "" {
			prDirs = []string{worktreeDir}
		}
	}
	if len(prDirs) == 0 {
		sr.Error = "could not detect PR in any repo worktree"
		return sr
	}

	recorded := 0
	for _, prDir := range prDirs {
		cmd := ghPRCmd(prDir, "view --json number -q .number")
		out, exitCode, _ := runCmdInDir(prDir, cmd+" 2>/dev/null")
		if exitCode != 0 || len(out) == 0 {
			continue
		}
		prNum := 0
		fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &prNum)
		if prNum == 0 {
			continue
		}
		// Derive short repo name from the worktree directory name
		repo := filepath.Base(prDir)
		if step.Command != "" && len(prDirs) == 1 {
			repo = step.Command // use configured repo name if single PR
		}
		code := PR(s, cfg, itemID, PROpts{Repo: repo, PRNumber: prNum})
		if code != 0 {
			sr.Error = fmt.Sprintf("st pr exited %d (%s#%d)", code, repo, prNum)
			return sr
		}
		recorded++
		// Reload store after each PR record (st pr modifies the item)
		s, _ = store.New(cfg)
	}

	if recorded == 0 {
		sr.Error = "could not detect PR number in any repo"
		return sr
	}
	sr.Passed = true
	return sr
}

func executeMerge(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "merge", Type: "merge"}

	// Find all repo worktrees that have PRs (item may span multiple repos)
	prDirs := allWorktreeDirsWithPR(cfg, itemID)
	if len(prDirs) == 0 {
		// No PRs found in any worktree — check the default worktreeDir as fallback
		cmd := ghPRCmd(worktreeDir, "view --json state -q .state")
		out, exitCode, _ := runCmdInDir(worktreeDir, cmd+" 2>/dev/null")
		state := strings.TrimSpace(string(out))
		if exitCode != 0 || state == "" {
			fmt.Println("  no PR on this branch — skipping merge")
			sr.Passed = true
			return sr
		}
		prDirs = []string{worktreeDir}
	}

	// Merge each repo's PR
	for _, prDir := range prDirs {
		cmd := ghPRCmd(prDir, "view --json state -q .state")
		out, exitCode, _ := runCmdInDir(prDir, cmd+" 2>/dev/null")
		state := strings.TrimSpace(string(out))
		if exitCode != 0 || state == "" {
			continue // no PR in this repo
		}
		if state == "MERGED" {
			repo := resolveGHRepo(prDir)
			fmt.Printf("  PR already merged (%s)\n", repo)
			continue
		}

		mergeRepo := resolveGHRepo(prDir)
		mergeBranch := ""
		if bOut, _, _ := runCmdInDir(prDir, "git branch --show-current 2>/dev/null"); len(bOut) > 0 {
			mergeBranch = strings.TrimSpace(string(bOut))
		}
		fmt.Printf("  merging PR in %s\n", mergeRepo)
		pipeOpts := PipelineOpts{
			RunCmd: func(cmd string) ([]byte, int, error) {
				if mergeRepo != "" && strings.Contains(cmd, "gh pr") && !strings.Contains(cmd, "--repo") {
					cmd = injectGHPRContext(cmd, mergeBranch, mergeRepo)
				}
				return runCmdInDir(prDir, cmd)
			},
		}
		code := Merge(s, cfg, itemID, pipeOpts)
		if code != 0 {
			sr.Error = fmt.Sprintf("st merge exited %d (%s)", code, mergeRepo)
			return sr
		}
	}

	sr.Passed = true
	return sr
}

func executeMergePrecheck(cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "merge_precheck", Type: "merge_precheck"}
	if cfg.Pipeline == nil || cfg.Pipeline.Merge == nil || len(cfg.Pipeline.Merge.PreChecks) == 0 {
		sr.Passed = true // no pre-checks configured
		return sr
	}

	// Find all repo worktrees that have PRs
	prDirs := allWorktreeDirsWithPR(cfg, itemID)
	if len(prDirs) == 0 {
		fmt.Println("  no PR on this branch — skipping pre-checks")
		sr.Passed = true
		return sr
	}

	// Run pre-checks for each repo that has a PR
	for _, prDir := range prDirs {
		ghRepo := resolveGHRepo(prDir)
		branch := ""
		if branchOut, _, _ := runCmdInDir(prDir, "git branch --show-current 2>/dev/null"); len(branchOut) > 0 {
			branch = strings.TrimSpace(string(branchOut))
		}
		if len(prDirs) > 1 {
			fmt.Printf("  pre-checks for %s\n", ghRepo)
		}
		for _, check := range cfg.Pipeline.Merge.PreChecks {
			rewritten := check
			if ghRepo != "" && strings.Contains(check, "gh pr") && !strings.Contains(check, "--repo") {
				rewritten = injectGHPRContext(check, branch, ghRepo)
			}
			output, exitCode, err := runCmdGuarded(prDir, rewritten, defaultCIIdleTimeout)
			if err != nil && exitCode == 0 {
				sr.Error = fmt.Sprintf("pre-check exec error (%s): %v", ghRepo, err)
				return sr
			}
			if exitCode != 0 {
				sr.Error = fmt.Sprintf("pre-check failed (%s, exit %d): %s", ghRepo, exitCode, truncate(string(output), 200))
				return sr
			}
		}
	}
	sr.Passed = true
	return sr
}

func executeDeploy(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "deploy", Type: "deploy"}

	// For CI watching, use the repo worktree that had the PR
	prDir := resolveWorktreeDirWithPR(cfg, itemID)
	if prDir == "" {
		prDir = worktreeDir
	}

	pipeOpts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(prDir, cmd)
		},
	}
	code := DeployCheck(s, cfg, itemID, pipeOpts)
	if code != 0 {
		sr.Error = fmt.Sprintf("st deploy-check exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeSmoke(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "smoke", Type: "smoke"}
	pipeOpts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(worktreeDir, cmd)
		},
	}
	code := Smoke(s, cfg, itemID, pipeOpts)
	if code != 0 {
		sr.Error = fmt.Sprintf("st smoke exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeUAT(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "uat", Type: "uat"}
	// Run UAT AC commands from the worktree BASE directory (parent of repo dirs)
	// so that `cd theraprac-api && ...` works correctly.
	uatDir := worktreeDir
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		uatDir = filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
	}
	code := UAT(s, cfg, itemID, UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(uatDir, cmd)
		},
	})
	if code != 0 {
		sr.Error = fmt.Sprintf("st uat exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeGate(itemID string, engine RunEngine) StepResult {
	gateMu.Lock()
	defer gateMu.Unlock()

	sr := StepResult{Step: "approval", Type: "gate"}
	if engineConfirmPrompt(engine, fmt.Sprintf("\nApprove %s?", itemID)) {
		sr.Passed = true
	} else {
		sr.Error = "user rejected"
	}
	return sr
}

// postDeployE2E checks the item's manifest for page files that need E2E coverage,
// then runs the corresponding E2E specs against the deployed dev environment.
// Returns a summary of results (empty string if no post-deploy E2E was needed).
func postDeployE2E(cfg *config.Config, itemID string) string {
	m, err := manifest.Load(cfg.ManifestDir(), itemID)
	if err != nil || len(m.PRs) == 0 {
		return ""
	}

	// Collect unique E2E specs from page files across all PRs
	specSet := map[string]bool{}
	for _, pr := range m.PRs {
		for _, f := range pr.Files {
			if f.Action == "D" {
				continue
			}
			spec := e2eSpecFor(f.Path)
			if spec != "" {
				specSet[spec] = true
			}
		}
	}
	if len(specSet) == 0 {
		return ""
	}

	// Find scope suites with PostDeployCmd
	if cfg.Testing == nil {
		return ""
	}
	var deployCmd string
	for _, suite := range cfg.Testing.ScopeSuites {
		if suite.PostDeployCmd != "" {
			deployCmd = suite.PostDeployCmd
			break
		}
	}
	if deployCmd == "" {
		return ""
	}

	// Run each spec against dev
	var results []string
	specs := make([]string, 0, len(specSet))
	for spec := range specSet {
		specs = append(specs, spec)
	}

	// Determine the run directory (worktree base or project root)
	runDir := cfg.Root()
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
		if _, err := os.Stat(wtBase); err == nil {
			runDir = wtBase
		}
	}

	fmt.Printf("[%s] Running post-deploy E2E against dev (%d spec(s))...\n", itemID, len(specs))
	allPassed := true
	for _, spec := range specs {
		cmd := deployCmd + " " + spec
		fmt.Printf("  → %s\n", spec)
		output, exitCode, err := runCmdInDir(runDir, cmd)
		if err != nil || exitCode != 0 {
			allPassed = false
			results = append(results, fmt.Sprintf("FAIL: %s (exit %d)", spec, exitCode))
			if len(output) > 500 {
				output = output[len(output)-500:]
			}
			results = append(results, string(output))
		} else {
			results = append(results, fmt.Sprintf("PASS: %s", spec))
		}
	}

	if allPassed {
		return fmt.Sprintf("Post-deploy E2E: %d spec(s) passed against dev", len(specs))
	}
	return "Post-deploy E2E results:\n" + strings.Join(results, "\n")
}

// executeUATReview runs UAT, then enters a conversational loop where the user
// can approve, reject, or give plain-text feedback that gets routed to claude.
// Claude acts on the feedback (writes tests, fixes code, etc.), then UAT re-runs
// and the updated report is shown. Loop continues until approve or reject.
func executeUATReview(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "uat_review"}

	// Run post-deploy E2E on first iteration (before UAT assessment)
	e2eSummary := postDeployE2E(cfg, itemID)
	if e2eSummary != "" {
		fmt.Printf("[%s] %s\n", itemID, e2eSummary)
	}

	for iteration := 1; ; iteration++ {
		// Run UAT
		fmt.Printf("\n[%s] Running UAT (iteration %d)...\n", itemID, iteration)
		uatDir := worktreeDir
		if cfg.Worktree != nil && cfg.Worktree.Enabled {
			uatDir = filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
		}
		uatCode := UAT(s, cfg, itemID, UATOpts{
			RunCmd: func(cmd string) ([]byte, int, error) {
				return runCmdInDir(uatDir, cmd)
			},
		})

		// Now launch claude to produce the UAT summary report
		e2eContext := ""
		if e2eSummary != "" {
			e2eContext = fmt.Sprintf("\n\nPost-deploy E2E results:\n%s\n", e2eSummary)
		}
		reportPrompt := fmt.Sprintf(
			"You just ran UAT for item %s. The UAT exit code was %d.%s\n\n"+
				"Produce a concise UAT summary report for the user. Include:\n"+
				"1. WHAT CHANGED — describe the feature in 2-3 sentences\n"+
				"2. WHAT WAS TESTED — list the test suites that passed, any coverage gaps\n"+
				"3. POST-DEPLOY E2E — results of E2E tests run against dev (if any)\n"+
				"4. ACCEPTANCE CRITERIA — how many passed/failed, highlight any failures\n"+
				"5. RECOMMENDATION — should the user approve? Why or why not?\n\n"+
				"Keep it brief and actionable. The user will read this and decide whether to approve.",
			itemID, uatCode, e2eContext)

		reportStep := config.RunStepDef{Type: "claude", Prompt: reportPrompt}
		reportStep.SetName("uat_report")
		reportSR := executeClaude(s, cfg, itemID, sprintID, reportStep, opts, engine, worktreeDir, claudeSessionID, true)
		_ = reportSR // report output goes to stdout via claude streaming

		// Prompt user for decision
		gateMu.Lock()
		fmt.Println()
		fmt.Printf("  ─── [%s] UAT Review (iteration %d) ───\n\n", itemID, iteration)
		choice := engineSelectMenu(engine, "", []menuOption{
			{"1", "Approve — accept and close"},
			{"2", "Reject  — stop and release for retry"},
			{"3", "Chat    — give feedback, claude acts, UAT re-runs"},
		}, 0)
		gateMu.Unlock()

		if choice == "1" {
			// Record UAT approval on item
			if approvalStore, err := store.New(cfg); err == nil {
				if approvalItem, ok := approvalStore.Get(itemID); ok {
					now := time.Now()
					setNestedField(approvalItem, "delivery", "uat_approved_by", "user")
					setNestedField(approvalItem, "delivery", "uat_approved_date", now.Format("2006-01-02"))
					setNestedField(approvalItem, "delivery", "stage", "uat_approved")
					approvalItem.Doc.SetField("last_touched", now.Format(time.RFC3339))
					approvalStore.Write(approvalItem)
				}
			}
			sr.Passed = true
			return sr
		}
		if choice == "2" {
			sr.Error = "user rejected"
			return sr
		}

		// Option 3 — launch interactive claude session
		if choice == "3" {
			fmt.Printf("\n[%s] Launching interactive claude session...\n", itemID)
			fmt.Println("  Chat with claude to make changes. When done, exit claude (Ctrl+D or /exit).")
			fmt.Println("  UAT will re-run automatically when you return.")
			fmt.Println()

			args := []string{"--resume", claudeSessionID}
			if worktreeDir != "" {
				args = append(args, "--add-dir", worktreeDir)
			}

			if engine.RunClaudeInteractive != nil {
				engine.RunClaudeInteractive(worktreeDir, args)
			} else {
				// Default: launch claude binary with stdin/stdout attached
				claudeBin, err := exec.LookPath("claude")
				if err != nil {
					fmt.Printf("[%s] claude not found in PATH\n", itemID)
					continue
				}
				cmd := exec.Command(claudeBin, args...)
				cmd.Dir = worktreeDir
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
			}

			fmt.Printf("\n[%s] Interactive session ended. Re-running UAT...\n", itemID)
			s, _ = store.New(cfg)
			continue
		}

		// Reload store and loop back to re-run UAT
		s, _ = store.New(cfg)
	}
}

// showPauseMenu displays an interactive menu when the pipeline is paused
// (either by Ctrl+C or a breakpoint). Returns "continue", "skip", or "abort".
func showPauseMenu(itemID, lastStep, nextStep string, result ItemResult, engine RunEngine) string {
	gateMu.Lock()
	defer gateMu.Unlock()

	// Summarize what happened so far
	var stepSummary []string
	failCount := 0
	for _, s := range result.Steps {
		status := "OK"
		if !s.Passed {
			status = "FAIL"
			failCount++
		}
		dur := ""
		if s.Duration > 0 {
			dur = fmt.Sprintf(" (%s)", formatDuration(s.Duration))
		}
		stepSummary = append(stepSummary, fmt.Sprintf("  %s: %s%s", s.Step, status, dur))
	}

	// Build recommendation
	recommendation := "[c] continue"
	if nextStep == "code_review" && failCount == 0 {
		recommendation = "[c] continue -- all green, code review should be quick"
	} else if nextStep == "merge" && failCount == 0 {
		recommendation = "[c] continue -- CI passed, ready to merge"
	} else if failCount > 0 {
		recommendation = "[s] skip -- previous failures suggest this step may also fail"
	}

	// Content lines (all ASCII so len() == display width)
	content := []string{
		fmt.Sprintf("PAUSED: %s", itemID),
		"",
	}
	// Show step history (last 5 max to keep it readable)
	historyStart := 0
	if len(stepSummary) > 5 {
		historyStart = len(stepSummary) - 5
	}
	for _, line := range stepSummary[historyStart:] {
		content = append(content, line)
	}
	content = append(content,
		"",
		fmt.Sprintf("Next:   %s", nextStep),
		fmt.Sprintf("Cost:   $%.2f  |  Steps: %d  |  Fails: %d", result.TotalCost, len(result.Steps), failCount),
		"",
		fmt.Sprintf(">>> %s", recommendation),
		"",
		"[c] continue -- resume pipeline",
		"[s] skip     -- skip next step, continue",
		"[a] abort    -- stop, release item for retry",
	)

	// Find widest line
	w := 0
	for _, l := range content {
		if len(l) > w {
			w = len(l)
		}
	}

	// Box drawing: each content line gets 2-char padding on each side
	hline := func(l, m, r string) {
		fmt.Print(l)
		for i := 0; i < w+4; i++ {
			fmt.Print(m)
		}
		fmt.Println(r)
	}

	fmt.Println()
	hline("╔", "═", "╗")
	for _, l := range content {
		if l == "" {
			hline("╠", "═", "╣")
		} else {
			fmt.Printf("║  %-*s  ║\n", w, l)
		}
	}
	hline("╚", "═", "╝")

	choice := engineSelectMenu(engine, "", []menuOption{
		{"c", "continue — resume pipeline"},
		{"s", "skip     — skip next step, continue"},
		{"a", "abort    — stop, release item for retry"},
	}, 0)
	return map[string]string{"c": "continue", "s": "skip", "a": "abort"}[choice]
}

func executeClose(s *store.Store, cfg *config.Config, itemID string, step config.RunStepDef) StepResult {
	sr := StepResult{Step: "close", Type: "close"}

	// Gate: reject items with skipped critical steps
	item, _ := s.Get(itemID)
	if item != nil {
		if skipped, _ := getNestedField(item, "delivery", "skipped_steps"); skipped != "" {
			criticalSteps := map[string]bool{
				"deploy_watch": true, "deploy": true,
				"smoke": true, "uat": true, "uat_review": true,
			}
			for _, step := range strings.Split(skipped, ",") {
				step = strings.TrimSpace(step)
				if criticalSteps[step] {
					sr.Error = fmt.Sprintf("cannot close: critical step %q was skipped — re-run to complete it", step)
					return sr
				}
			}
		}
	}

	resolution := step.Resolution
	if resolution == "" {
		resolution = "completed"
	}

	if item != nil && item.Type == "issue" && resolution == "completed" {
		resolution = "resolved"
	}

	code := Close(s, cfg, itemID, resolution, CloseOpts{})
	if code != 0 {
		sr.Error = fmt.Sprintf("st close exited %d", code)
		return sr
	}

	// Clean up worktree and pull main
	cleanupWorktree(cfg, itemID)

	sr.Passed = true
	return sr
}

// cleanupWorktree removes the item's worktree and pulls main on all repos.
func cleanupWorktree(cfg *config.Config, itemID string) {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return
	}

	// Remove worktree via st finish
	fmt.Printf("[%s] Cleaning up worktree...\n", itemID)
	s, _ := store.New(cfg)
	Finish(s, cfg, itemID, FinishOpts{})

	// Pull main on all repos so next item starts from latest
	parentDir := cfg.Worktree.ParentDir
	if !filepath.IsAbs(parentDir) {
		parentDir = filepath.Join(cfg.Root(), parentDir)
	}
	for _, repo := range cfg.Worktree.Repos {
		repoDir := repo
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				repoDir = mapped
			}
		}
		mainRepo := filepath.Join(parentDir, repoDir)
		fmt.Printf("[%s] Pulling main on %s...\n", itemID, repoDir)
		cmd := exec.Command("git", "pull", "--ff-only")
		cmd.Dir = mainRepo
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Run() // best-effort
	}
}

func executeCommand(cfg *config.Config, itemID, sprintID string, step config.RunStepDef, worktreeDir string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "command"}
	cmd := expandTemplate(step.Command, itemID, sprintID, worktreeDir, cfg)
	timeout := time.Duration(step.Timeout) * time.Second
	output, exitCode, err := runCmdGuarded(worktreeDir, cmd, timeout)
	if err != nil && exitCode == 0 {
		sr.Error = fmt.Sprintf("exec error: %v", err)
		return sr
	}
	if exitCode != 0 {
		sr.Error = fmt.Sprintf("exit %d: %s", exitCode, truncate(string(output), 200))
		return sr
	}
	sr.Passed = true
	sr.Output = truncate(string(output), 200)
	return sr
}

func executeVerifyTests(s *store.Store, cfg *config.Config, itemID string) StepResult {
	sr := StepResult{Step: "verify_tests", Type: "verify_tests"}
	if cfg.Testing == nil {
		sr.Passed = true
		return sr
	}

	item, ok := s.Get(itemID)
	if !ok {
		sr.Error = "item not found"
		return sr
	}

	// Helper to look up evidence — checks both flat and nested storage.
	getEvidence := func(sectionKey, name string) string {
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		if section, ok := item.TestingEvidence[sectionKey]; ok {
			if m, ok := section.(map[string]interface{}); ok {
				if v, ok := m[name]; ok {
					if s, ok := v.(string); ok {
						return s
					}
				}
			}
		}
		return ""
	}

	// Find missing required suites
	var missing []string
	for name := range cfg.Testing.RequiredSuites {
		val := getEvidence("required_suites", name)
		if !strings.HasPrefix(val, "pass") {
			missing = append(missing, name)
		}
	}

	// Find triggered but unrun scope suites
	var missingScope []string
	for name := range cfg.Testing.ScopeSuites {
		val := getEvidence("scope_suites", name)
		if val == "required" {
			missingScope = append(missingScope, name)
		}
	}

	// Self-heal: run missing suites directly instead of failing
	if len(missing) > 0 || len(missingScope) > 0 {
		allMissing := append(missing, missingScope...)
		fmt.Printf("  auto-running %d missing test suite(s): %s\n", len(allMissing), strings.Join(allMissing, ", "))

		for _, name := range allMissing {
			opts := TestRecordOpts{Run: true}
			code := TestRecord(s, cfg, itemID, name, opts)
			if code != 0 {
				sr.Error = fmt.Sprintf("test suite %s failed (exit %d)", name, code)
				return sr
			}
			// Reload store after each test record
			s, _ = store.New(cfg)
		}

		// Re-check after running
		s, _ = store.New(cfg)
		item, _ = s.Get(itemID)

		// Verify everything passes now
		var stillMissing []string
		for name := range cfg.Testing.RequiredSuites {
			val := getEvidence("required_suites", name)
			if !strings.HasPrefix(val, "pass") {
				stillMissing = append(stillMissing, name)
			}
		}
		if len(stillMissing) > 0 {
			sr.Error = fmt.Sprintf("test suites still failing after auto-run: %s", strings.Join(stillMissing, ", "))
			return sr
		}
	}

	sr.Passed = true
	return sr
}

func executePlan(s *store.Store, cfg *config.Config, itemID string, engine RunEngine) StepResult {
	return executePlanWithOpts(s, cfg, itemID, engine, RunOpts{}, "")
}

func executePlanWithOpts(s *store.Store, cfg *config.Config, itemID string, engine RunEngine, opts RunOpts, worktreeDir string) StepResult {
	sr := StepResult{Step: "plan", Type: "plan"}

	item, ok := s.Get(itemID)
	if !ok {
		sr.Error = "item not found"
		return sr
	}

	// Already approved — skip (either via item flag or plan sidecar)
	if item.PlanApproved {
		sr.Passed = true
		return sr
	}
	if p, err := plan.Load(cfg.PlansDir(), itemID); err == nil && p != nil && p.Approved {
		// Plan sidecar exists and is approved — sync flag to item and skip
		item.PlanApproved = true
		item.Doc.SetField("plan_approved", "true")
		s.Write(item)
		sr.Passed = true
		return sr
	}

	// Check what's missing
	needsSummary := item.Summary == ""
	needsACs := len(item.AcceptanceCriteria) == 0

	// If fields are missing, ask claude to propose a plan
	if needsSummary || needsACs {
		fmt.Printf("\n[%s] Item missing %s — asking Claude to propose a plan...\n",
			itemID, planMissingFields(needsSummary, needsACs))

		proposal, err := proposePlan(cfg, itemID, item, engine, opts, worktreeDir, needsSummary, needsACs)
		if err != nil {
			sr.Error = fmt.Sprintf("plan proposal failed: %v", err)
			return sr
		}

		fmt.Printf("\n=== Proposed Plan: %s ===\n", itemID)
		fmt.Printf("Title: %s\n", item.Title)
		fmt.Println(proposal)

		// Plan review loop — accept, reject, or chat to revise
		for {
			choice := engineSelectMenu(engine, fmt.Sprintf("[%s] Plan Review", itemID), []menuOption{
				{"1", "Accept  — approve and proceed"},
				{"2", "Reject  — stop and release"},
				{"3", "Chat    — give feedback, claude revises plan"},
			}, 0)

			if choice == "1" {
				break // approved
			}
			if choice == "2" {
				sr.Error = "plan proposal rejected"
				return sr
			}

			// Option 3: launch interactive claude session for plan revision
			fmt.Printf("\n[%s] Launching interactive session for plan revision...\n", itemID)
			fmt.Println("  Discuss changes with claude. When done, exit (Ctrl+D or /exit).")
			fmt.Println("  The revised plan will be shown when you return.")
			fmt.Println()

			cwd := worktreeDir
			if cwd == "" {
				cwd = cfg.Root()
			}
			args := []string{}
			if engine.RunClaudeInteractive != nil {
				engine.RunClaudeInteractive(cwd, args)
			} else {
				claudeBin, err := exec.LookPath("claude")
				if err != nil {
					fmt.Printf("[%s] claude not found in PATH\n", itemID)
					continue
				}
				cmd := exec.Command(claudeBin, args...)
				cmd.Dir = cwd
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
			}

			// Reload item after interactive session
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)

			// Show updated plan
			fmt.Printf("\n=== Revised Plan: %s ===\n", itemID)
			fmt.Printf("Title: %s\n", item.Title)
			if item.Summary != "" {
				fmt.Printf("\nSummary:\n%s\n", item.Summary)
			}
			if len(item.AcceptanceCriteria) > 0 {
				fmt.Printf("\nAcceptance Criteria:\n")
				for i, ac := range item.AcceptanceCriteria {
					fmt.Printf("  %d. %s\n", i+1, ac)
				}
			}
		}
	} else {
		// Fields present — show for design review
		fmt.Printf("\n=== Design Gate: %s ===\n", itemID)
		fmt.Printf("Title: %s\n", item.Title)
		fmt.Printf("\nSummary:\n%s\n", item.Summary)
		fmt.Printf("\nAcceptance Criteria:\n")
		for i, ac := range item.AcceptanceCriteria {
			fmt.Printf("  %d. %s\n", i+1, ac)
		}
		if len(item.DependsOn) > 0 {
			fmt.Printf("\nDepends on: %s\n", strings.Join(item.DependsOn, ", "))
		}

		// Design review loop — same pattern
		for {
			choice := engineSelectMenu(engine, fmt.Sprintf("[%s] Design Review", itemID), []menuOption{
				{"1", "Approve — accept and proceed"},
				{"2", "Reject  — stop and release"},
				{"3", "Chat    — give feedback, claude revises"},
			}, 0)

			if choice == "1" {
				break // approved
			}
			if choice == "2" {
				sr.Error = "design not approved"
				return sr
			}

			// Option 3: interactive revision
			fmt.Printf("\n[%s] Launching interactive session for design revision...\n", itemID)
			fmt.Println("  Discuss changes with claude. When done, exit (Ctrl+D or /exit).")
			fmt.Println()

			cwd := worktreeDir
			if cwd == "" {
				cwd = cfg.Root()
			}
			if engine.RunClaudeInteractive != nil {
				engine.RunClaudeInteractive(cwd, []string{})
			} else {
				claudeBin, err := exec.LookPath("claude")
				if err != nil {
					fmt.Printf("[%s] claude not found in PATH\n", itemID)
					continue
				}
				cmd := exec.Command(claudeBin)
				cmd.Dir = cwd
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
			}

			// Reload and show updated design
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)

			fmt.Printf("\n=== Revised Design: %s ===\n", itemID)
			fmt.Printf("Title: %s\n", item.Title)
			fmt.Printf("\nSummary:\n%s\n", item.Summary)
			fmt.Printf("\nAcceptance Criteria:\n")
			for i, ac := range item.AcceptanceCriteria {
				fmt.Printf("  %d. %s\n", i+1, ac)
			}
		}
	}

	// Record approval on item (reload in case claude updated it)
	s3, _ := store.New(cfg)
	item, _ = s3.Get(itemID)

	// Validate ACs — all must be cmd: prefixed
	var badACs []string
	for _, ac := range item.AcceptanceCriteria {
		trimmed := strings.TrimSpace(ac)
		trimmed = strings.TrimPrefix(trimmed, "- ")
		if !strings.HasPrefix(trimmed, "cmd:") && !strings.HasPrefix(trimmed, "cmd :") {
			badACs = append(badACs, trimmed)
		}
	}
	if len(badACs) > 0 {
		fmt.Printf("\n⚠ %d AC(s) missing 'cmd:' prefix (will be flagged in UAT):\n", len(badACs))
		for _, ac := range badACs {
			fmt.Printf("  - %s\n", ac)
		}
		fmt.Println()
	}

	item.PlanApproved = true
	item.Doc.SetField("plan_approved", "true")
	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
	s3.Write(item)

	sr.Passed = true
	return sr
}

// proposePlan launches claude -p to analyze the item and propose summary + ACs.
func proposePlan(cfg *config.Config, itemID string, item *model.Item, engine RunEngine, opts RunOpts, worktreeDir string, needsSummary, needsACs bool) (string, error) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Analyze item %s and propose a plan.\n\n", itemID))
	b.WriteString(fmt.Sprintf("Title: %s\n", item.Title))
	if item.Summary != "" {
		b.WriteString(fmt.Sprintf("Existing summary: %s\n", item.Summary))
	}
	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("Existing acceptance criteria:\n")
		for _, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("- %s\n", ac))
		}
	}

	b.WriteString("\nIMPORTANT: You MUST set the fields using the st CLI. Do NOT ask for permission. Just do it.\n\n")
	b.WriteString("Steps:\n")
	b.WriteString("1. Read the codebase to understand the context\n")
	if needsSummary {
		b.WriteString(fmt.Sprintf("2. Write a clear technical summary and set it by running:\n"))
		b.WriteString(fmt.Sprintf("   echo 'your summary text here' | st update %s summary --stdin\n", itemID))
	}
	if needsACs {
		b.WriteString(fmt.Sprintf("3. Write specific, testable acceptance criteria and set them by running:\n"))
		b.WriteString(fmt.Sprintf("   printf '- criterion 1\\n- criterion 2\\n' | st update %s acceptance_criteria --stdin\n", itemID))
	}
	b.WriteString(fmt.Sprintf("4. Print your analysis and what you set to stdout\n\n"))
	b.WriteString("Acceptance criteria format — EVERY criterion must start with 'cmd:' followed by\n")
	b.WriteString("an executable command that exits 0 on success. ACs ARE tests. Examples:\n")
	b.WriteString("- cmd: go test ./cmd/server/ -run TestServiceDefinitions_RBAC_Staff_POST_Returns403\n")
	b.WriteString("- cmd: go test ./internal/db/ -run TestVoidClientCharge_Concurrent_OnlyOneSucceeds\n")
	b.WriteString("- cmd: cd ../theraprac-web && npx playwright test tests/e2e/staff-role-split.spec.ts\n")
	b.WriteString("- cmd: grep -q 'SELECT.*FOR UPDATE' internal/db/client_charges.go\n")
	b.WriteString("\nFor new features, name the test function that WILL exist after implementation.\n")
	b.WriteString("The implement step writes the actual test. No prose ACs — if it can't be a command, it's not an AC.\n")
	b.WriteString("\nCRITICAL: Every AC line MUST begin with '- cmd: '. Lines without this prefix will be rejected.\n")
	b.WriteString("Use relative paths from the worktree: '../theraprac-api' or '../theraprac-web' (NOT 'theraprac-api').\n")
	b.WriteString("\nDo NOT ask 'shall I go ahead' — just set the fields and report what you did.\n")

	// Use the worktree dir if available, otherwise the config root
	cwd := worktreeDir
	if cwd == "" {
		cwd = cfg.Root()
	}

	args := buildClaudeArgs(cfg, b.String(), opts, cwd)
	sessionID := generateSessionID()
	env := []string{"AS_SESSION_ID=" + sessionID}
	if agentID := cfg.AgentID(); agentID != "" {
		env = append(env, "AS_AGENT_ID="+agentID)
	}

	output, exitCode, err := engine.RunClaude(cwd, args, env)
	if err != nil {
		return "", fmt.Errorf("claude exec error: %v", err)
	}

	// Parse JSON to extract the text result
	claudeResult, parseErr := parseClaudeOutput(output)
	if parseErr != nil {
		if exitCode != 0 {
			return "", fmt.Errorf("claude exited %d", exitCode)
		}
		return string(output), nil
	}

	if exitCode != 0 || (claudeResult.Subtype != "" && claudeResult.Subtype != "success") {
		return "", fmt.Errorf("claude exited %d (subtype: %s)", exitCode, claudeResult.Subtype)
	}

	return claudeResult.Result, nil
}

func planMissingFields(needsSummary, needsACs bool) string {
	var parts []string
	if needsSummary {
		parts = append(parts, "summary")
	}
	if needsACs {
		parts = append(parts, "acceptance_criteria")
	}
	return strings.Join(parts, ", ")
}

// recordSession appends a session ID to the item's sessions list.
func recordSession(s *store.Store, cfg *config.Config, itemID, sessionID, stepName string) {
	item, ok := s.Get(itemID)
	if !ok {
		return
	}

	// Append to sessions list
	item.Sessions = append(item.Sessions, sessionID)
	updateListInDoc(item, "sessions", item.Sessions)

	// Record in time_tracking which step used this session
	setNestedField(item, "time_tracking", "last_session", sessionID)
	setNestedField(item, "time_tracking", "last_step", stepName)

	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
	s.Write(item)
}

// --- Prompt and args ---

// buildDefaultPrompt constructs the full default prompt for the implement step.
func buildDefaultPrompt(s *store.Store, cfg *config.Config, itemID, sprintID string) string {
	item, ok := s.Get(itemID)
	if !ok {
		return fmt.Sprintf("Work on item %s.", itemID)
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("You are working on %s: %s\n\n", itemID, item.Title))

	if item.Summary != "" {
		b.WriteString("## Summary\n")
		b.WriteString(item.Summary)
		b.WriteString("\n\n")
	}

	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("## Acceptance Criteria\n")
		for i, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, ac))
		}
		b.WriteString("\n")
	}

	// Required test suites
	if cfg.Testing != nil && len(cfg.Testing.RequiredSuites) > 0 {
		b.WriteString("## Required Test Suites\n")
		b.WriteString("ALL of these must pass BEFORE committing:\n")
		for name := range cfg.Testing.RequiredSuites {
			b.WriteString(fmt.Sprintf("  st test %s %s --run\n", itemID, name))
		}
		b.WriteString("\n")
	}

	// Scope test suites (triggered by st pr based on file changes)
	if cfg.Testing != nil && len(cfg.Testing.ScopeSuites) > 0 {
		b.WriteString("## Scope Test Suites\n")
		b.WriteString("After recording the PR with `st pr`, check which scope suites were triggered.\n")
		b.WriteString("Run any that show as 'required' in testing_evidence:\n")
		for name := range cfg.Testing.ScopeSuites {
			b.WriteString(fmt.Sprintf("  st test %s %s --run  # if triggered\n", itemID, name))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Delivery Instructions\n")
	b.WriteString("1. Implement the changes\n")
	b.WriteString("2. Run ALL required test suites (above) — they must pass BEFORE committing\n")
	b.WriteString("3. Self-review: run `git diff` and review all changes\n")
	b.WriteString("4. Commit and push your branch\n")
	b.WriteString("5. Create a pull request with `gh pr create`\n")
	b.WriteString(fmt.Sprintf("6. Record the PR: `st pr %s --repo <repo-name> --pr <number>`\n", itemID))
	b.WriteString("7. Check if `st pr` triggered any scope suites — run them if so\n")
	b.WriteString("8. STOP here. Do NOT merge. Report your results.\n\n")

	// Add environment context so claude doesn't waste time discovering the worktree
	b.WriteString("## Environment\n")
	b.WriteString(fmt.Sprintf("- You are running in a worktree. Your CWD is already the correct repo.\n"))
	if item.Manifest != nil {
		if prsRaw, ok := item.Manifest["prs"]; ok {
			if prsStr, ok := prsRaw.(string); ok && prsStr != "" {
				b.WriteString(fmt.Sprintf("- PR already exists: %s — do NOT create a new one\n", prsStr))
			}
		}
	}
	if item.Delivery != nil {
		if stage, ok := item.Delivery["stage"]; ok {
			if stageStr, ok := stage.(string); ok && stageStr != "" {
				b.WriteString(fmt.Sprintf("- Delivery stage: %s\n", stageStr))
			}
		}
	}
	// Show which test suites already passed
	if item.TestingEvidence != nil {
		var passed []string
		for name, val := range item.TestingEvidence {
			if s, ok := val.(string); ok && strings.HasPrefix(s, "pass") {
				passed = append(passed, name)
			}
		}
		if len(passed) > 0 {
			sort.Strings(passed)
			b.WriteString(fmt.Sprintf("- Test suites already passing: %s\n", strings.Join(passed, ", ")))
			b.WriteString("  If these suites already passed on the current HEAD, do NOT re-run them.\n")
		}
	}
	b.WriteString("\n")

	b.WriteString("## Already Complete?\n")
	b.WriteString("If the branch already has commits (check `git log main..HEAD`):\n")
	b.WriteString("1. Verify acceptance criteria pass — do NOT re-run test suites that already passed\n")
	b.WriteString("2. If everything passes, just report results and STOP\n")
	b.WriteString("3. Only re-run tests if you made NEW changes\n\n")

	b.WriteString("## State Tracking\n")
	b.WriteString(fmt.Sprintf("- `st test %s <suite> --run` — execute and record test evidence\n", itemID))
	b.WriteString(fmt.Sprintf("- `st pr %s --repo <repo> --pr <N>` — record PR manifest\n", itemID))
	b.WriteString(fmt.Sprintf("- `st update %s delivery.stage <stage>` — advance delivery stage\n", itemID))
	b.WriteString("Do NOT close the item.\n")

	return b.String()
}

func buildClaudeArgs(cfg *config.Config, prompt string, opts RunOpts, worktreeDir string) []string {
	args := []string{"-p", prompt}

	// Permission mode
	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = cfg.RunPermissionMode()
	}
	if permMode == "dangerously-skip-permissions" {
		args = append(args, "--dangerously-skip-permissions")
	} else if permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}

	// Output format — stream-json for real-time visibility (requires --verbose)
	args = append(args, "--output-format", "stream-json", "--verbose")

	// Add agent-state directory
	args = append(args, "--add-dir", cfg.Root())

	// Model
	model := opts.Model
	if model == "" && cfg.Run != nil && cfg.Run.DefaultModel != "" {
		model = cfg.Run.DefaultModel
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	// Budget
	budget := opts.MaxBudgetUSD
	if budget <= 0 && cfg.Run != nil && cfg.Run.DefaultBudgetUSD > 0 {
		budget = cfg.Run.DefaultBudgetUSD
	}
	if budget > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", budget))
	}

	return args
}

func parseClaudeOutput(output []byte) (*ClaudeResult, error) {
	// claude -p --output-format json outputs a JSON object
	// Try to find the last JSON object in the output
	s := strings.TrimSpace(string(output))
	if s == "" {
		return nil, fmt.Errorf("empty output")
	}

	// Try direct parse first
	var result ClaudeResult
	if err := json.Unmarshal([]byte(s), &result); err == nil && result.Type != "" {
		return &result, nil
	}

	// Try finding last { ... } block (claude may output progress before JSON)
	lastBrace := strings.LastIndex(s, "}")
	if lastBrace < 0 {
		return nil, fmt.Errorf("no JSON object found")
	}
	// Find matching opening brace
	depth := 0
	start := -1
	for i := lastBrace; i >= 0; i-- {
		switch s[i] {
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				start = i
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no matching JSON object found")
	}

	if err := json.Unmarshal([]byte(s[start:lastBrace+1]), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Helpers ---

func expandTemplate(s, itemID, sprintID, worktreeDir string, cfg *config.Config) string {
	s = strings.ReplaceAll(s, "{id}", itemID)
	s = strings.ReplaceAll(s, "{sprint}", sprintID)
	s = strings.ReplaceAll(s, "{worktree}", worktreeDir)

	// Resolve branch from worktree
	branch := ""
	if worktreeDir != "" {
		out, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err == nil {
			branch = strings.TrimSpace(string(out))
		}
	}
	s = strings.ReplaceAll(s, "{branch}", branch)

	// Resolve PR info from item manifest
	prInfo := ""
	repo := ""
	st, _ := store.New(cfg)
	if st != nil {
		if item, ok := st.Get(itemID); ok && item.Manifest != nil {
			if prsRaw, ok := item.Manifest["prs"]; ok {
				if prsStr, ok := prsRaw.(string); ok && prsStr != "" {
					prInfo = prsStr
					// Extract repo from first PR entry (e.g., "theraprac-web#94" → "theraprac-web")
					if idx := strings.Index(prsStr, "#"); idx > 0 {
						repo = prsStr[:idx]
					}
				}
			}
		}
	}
	s = strings.ReplaceAll(s, "{pr}", prInfo)
	s = strings.ReplaceAll(s, "{repo}", repo)

	// {pr_number} — just the number from the first PR (e.g., "94" from "theraprac-web#94")
	prNumber := ""
	if idx := strings.Index(prInfo, "#"); idx >= 0 {
		prNumber = prInfo[idx+1:]
		// Handle comma-separated: take only first number
		if ci := strings.Index(prNumber, ","); ci >= 0 {
			prNumber = strings.TrimSpace(prNumber[:ci])
		}
	}
	s = strings.ReplaceAll(s, "{pr_number}", prNumber)

	// Inject context block if template uses {context}
	if strings.Contains(s, "{context}") {
		s = strings.ReplaceAll(s, "{context}", buildContextBlock(itemID, worktreeDir, branch, prInfo, repo, cfg))
	}

	return s
}

// buildContextBlock assembles a context section for claude prompts so the
// subprocess doesn't have to rediscover the environment.
func buildContextBlock(itemID, worktreeDir, branch, prInfo, repo string, cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("\n## Environment Context\n")
	b.WriteString(fmt.Sprintf("- Working directory: %s\n", worktreeDir))
	if branch != "" {
		b.WriteString(fmt.Sprintf("- Branch: %s\n", branch))
	}
	if repo != "" {
		b.WriteString(fmt.Sprintf("- Repo: %s\n", repo))
	}
	if prInfo != "" {
		b.WriteString(fmt.Sprintf("- PR: %s\n", prInfo))
	}
	ghRepo := resolveGHRepo(worktreeDir)
	if ghRepo != "" {
		b.WriteString(fmt.Sprintf("- GitHub repo: %s\n", ghRepo))
	}
	return b.String()
}

// buildItemContext builds a comprehensive context block from the item's current state.
// Injected into every claude invocation so it never has to rediscover the environment.
func buildItemContext(s *store.Store, cfg *config.Config, itemID, worktreeDir string) string {
	var b strings.Builder
	b.WriteString("\n\n---\n## Full Item Context (auto-injected)\n")

	item, ok := s.Get(itemID)
	if !ok {
		b.WriteString(fmt.Sprintf("Item %s not found in store.\n", itemID))
		return b.String()
	}

	// Identity
	b.WriteString(fmt.Sprintf("- Item: %s (%s)\n", itemID, item.Type))
	b.WriteString(fmt.Sprintf("- Title: %s\n", item.Title))
	b.WriteString(fmt.Sprintf("- Status: %s\n", item.Status))

	// Environment
	b.WriteString(fmt.Sprintf("- Working directory: %s\n", worktreeDir))
	branch := ""
	if worktreeDir != "" {
		out, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err == nil {
			branch = strings.TrimSpace(string(out))
		}
	}
	if branch != "" {
		b.WriteString(fmt.Sprintf("- Branch: %s\n", branch))
	}
	ghRepo := resolveGHRepo(worktreeDir)
	if ghRepo != "" {
		b.WriteString(fmt.Sprintf("- GitHub repo: %s\n", ghRepo))
	}

	// PRs
	if item.Manifest != nil {
		if prsRaw, ok := item.Manifest["prs"]; ok {
			if prsStr, ok := prsRaw.(string); ok && prsStr != "" {
				b.WriteString(fmt.Sprintf("- PRs: %s\n", prsStr))
				// Extract PR number for convenience
				if idx := strings.Index(prsStr, "#"); idx >= 0 {
					rest := prsStr[idx+1:]
					if ci := strings.Index(rest, ","); ci >= 0 {
						rest = rest[:ci]
					}
					b.WriteString(fmt.Sprintf("- PR number: %s\n", strings.TrimSpace(rest)))
				}
			}
		}
	}

	// Delivery stage
	if item.Delivery != nil {
		if stage, ok := item.Delivery["stage"]; ok {
			if stageStr, ok := stage.(string); ok && stageStr != "" {
				b.WriteString(fmt.Sprintf("- Delivery stage: %s\n", stageStr))
			}
		}
		if lastStep, ok := item.Delivery["last_completed_step"]; ok {
			if stepStr, ok := lastStep.(string); ok && stepStr != "" {
				b.WriteString(fmt.Sprintf("- Last completed step: %s\n", stepStr))
			}
		}
	}

	// Test evidence
	if item.TestingEvidence != nil {
		if reqSuites, ok := item.TestingEvidence["required_suites"]; ok {
			if m, ok := reqSuites.(map[string]interface{}); ok {
				var passed, failing []string
				for name, val := range m {
					if s, ok := val.(string); ok {
						if strings.HasPrefix(s, "pass") {
							passed = append(passed, name)
						} else if s != "null" && s != "" {
							failing = append(failing, name+": "+s)
						}
					}
				}
				if len(passed) > 0 {
					sort.Strings(passed)
					b.WriteString(fmt.Sprintf("- Tests passing: %s\n", strings.Join(passed, ", ")))
				}
				if len(failing) > 0 {
					sort.Strings(failing)
					b.WriteString(fmt.Sprintf("- Tests failing: %s\n", strings.Join(failing, ", ")))
				}
			}
		}
	}

	// Summary (truncated)
	if item.Summary != "" {
		summary := item.Summary
		if len(summary) > 500 {
			summary = summary[:500] + "..."
		}
		b.WriteString(fmt.Sprintf("\n### Summary\n%s\n", summary))
	}

	// Acceptance criteria
	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("\n### Acceptance Criteria\n")
		for i, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, ac))
		}
	}

	// Implementation plan (from plan sidecar)
	if p, err := plan.Load(cfg.PlansDir(), itemID); err == nil && p != nil {
		planText := plan.PlainText(p)
		if planText != "" {
			b.WriteString("\n### Implementation Plan\n")
			b.WriteString(planText)
			b.WriteString("\n")
		}
	}

	// Files changed (from PR manifest)
	m, err := manifest.Load(cfg.ManifestDir(), itemID)
	if err == nil && len(m.PRs) > 0 {
		b.WriteString("\n### Files Changed\n")
		for _, pr := range m.PRs {
			if len(m.PRs) > 1 {
				b.WriteString(fmt.Sprintf("**%s#%d** (%d files, +%d/-%d):\n",
					pr.Repo, pr.PRNumber, pr.CodeStats.FilesChanged,
					pr.CodeStats.Insertions, pr.CodeStats.Deletions))
			}
			for _, f := range pr.Files {
				b.WriteString(fmt.Sprintf("  %s %s (+%d/-%d) [%s]\n",
					f.Action, f.Path, f.LinesAdded, f.LinesDeleted, f.Type))
			}
		}
	}

	return b.String()
}

func resolveWorktreeDir(cfg *config.Config, itemID string) string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled || cfg.Worktree.BaseDir == "" {
		return cfg.Root()
	}

	wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)

	// Return the first repo directory that exists
	repos := cfg.Worktree.Repos
	if len(repos) == 0 {
		return wtBase
	}

	for _, repo := range repos {
		dir := repo
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				dir = mapped
			}
		}
		candidate := filepath.Join(wtBase, dir)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return wtBase
}

// allWorktreeDirs returns all existing repo worktree directories for an item.
func allWorktreeDirs(cfg *config.Config, itemID string) []string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled || cfg.Worktree.BaseDir == "" {
		return []string{cfg.Root()}
	}

	wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
	repos := cfg.Worktree.Repos
	if len(repos) == 0 {
		if _, err := os.Stat(wtBase); err == nil {
			return []string{wtBase}
		}
		return nil
	}

	var dirs []string
	for _, repo := range repos {
		dir := repo
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				dir = mapped
			}
		}
		candidate := filepath.Join(wtBase, dir)
		if _, err := os.Stat(candidate); err == nil {
			dirs = append(dirs, candidate)
		}
	}
	return dirs
}

// allWorktreeDirsWithPR returns all repo worktree directories that have an open PR.
// First checks the item's manifest prs field (e.g., "theraprac-web#94, theraprac-api#55"),
// then falls back to probing all worktrees with gh pr view.
func allWorktreeDirsWithPR(cfg *config.Config, itemID string) []string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled || cfg.Worktree.BaseDir == "" {
		return []string{cfg.Root()}
	}
	wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)

	// Try to resolve from item's recorded PR manifest (e.g., "theraprac-web#94, theraprac-api#55")
	s, _ := store.New(cfg)
	if s != nil {
		if item, ok := s.Get(itemID); ok && item.Manifest != nil {
			if prsRaw, ok := item.Manifest["prs"]; ok {
				if prsStr, ok := prsRaw.(string); ok && prsStr != "" {
					seen := make(map[string]bool)
					var dirs []string
					for _, pr := range strings.Split(prsStr, ",") {
						pr = strings.TrimSpace(pr)
						if idx := strings.Index(pr, "#"); idx > 0 {
							repo := pr[:idx]
							if seen[repo] {
								continue // deduplicate same repo
							}
							seen[repo] = true
							candidate := filepath.Join(wtBase, repo)
							if _, err := os.Stat(candidate); err == nil {
								dirs = append(dirs, candidate)
							}
						}
					}
					if len(dirs) > 0 {
						return dirs
					}
				}
			}
		}
	}

	// Fallback: probe all worktrees with gh pr view
	var dirs []string
	for _, dir := range allWorktreeDirs(cfg, itemID) {
		cmd := ghPRCmd(dir, "view --json number -q .number")
		out, exitCode, _ := runCmdInDir(dir, cmd+" 2>/dev/null")
		if exitCode == 0 && strings.TrimSpace(string(out)) != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// resolveWorktreeDirWithPR returns the first repo worktree that has a PR.
// Falls back to resolveWorktreeDir if no PR is found.
func resolveWorktreeDirWithPR(cfg *config.Config, itemID string) string {
	dirs := allWorktreeDirsWithPR(cfg, itemID)
	if len(dirs) > 0 {
		return dirs[0]
	}
	return resolveWorktreeDir(cfg, itemID)
}

// releaseItem resets an item back to startable state after a pipeline failure.
// Clears claim, resets status so the item can be retried.
func releaseItem(cfg *config.Config, itemID string) {
	localStore, err := store.New(cfg)
	if err != nil {
		return
	}
	item, ok := localStore.Get(itemID)
	if !ok {
		return
	}

	// Keep item active — just clear the claim so the next run can pick it up.
	// Do NOT reset status to start. The work (code, PR, tests) is preserved.
	fmt.Printf("[%s] Releasing claim for retry (keeping status: %s)\n", itemID, item.Status)

	// Clear claim
	if item.ClaimedBy != "" {
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		_ = mgr.RemoveClaim(item.ClaimedBy, itemID)
		item.ClaimedBy = ""
		item.ClaimedAt = ""
		item.Doc.SetField("claimed_by", "")
		item.Doc.SetField("claimed_at", "")
	}

	// Keep plan_approved if it was set — the user already approved the design.
	// Only the plan step itself should set/clear this flag.

	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
	localStore.Write(item)
}

// recoverStaleItems finds items in the sprint that are active but not
// claimed by the current session, and resets them for retry.
// Called at the start of st run.
func recoverStaleItems(s *store.Store, cfg *config.Config, sprintItems []string) {
	currentSession := cfg.SessionID()
	for _, itemID := range sprintItems {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}
		if item.Status != tc.ActiveStatus {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}

		// If claimed by current session, leave it (we're resuming our own work)
		if item.ClaimedBy == currentSession && currentSession != "" {
			continue
		}

		// Check if this item's PR was already merged — advance past merge
		// so the pipeline continues with deploy verification, UAT, etc.
		if detectMergedPR(cfg, itemID, item) {
			fmt.Printf("[%s] PR already merged — advancing to post-merge steps\n", itemID)
			setNestedField(item, "delivery", "stage", "merged")
			setNestedField(item, "delivery", "last_completed_step", "merge")
			item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
			s.Write(item)
			continue
		}

		// Active item not claimed by us — recover it
		reason := "unclaimed"
		if item.ClaimedBy != "" {
			reason = fmt.Sprintf("claimed by %s (not current session)", item.ClaimedBy[:8])
		}
		fmt.Printf("[%s] Recovering: %s\n", itemID, reason)
		releaseItem(cfg, itemID)
	}
}

// detectMergedPR checks if the item has a PR that's already been merged.
// Checks both the manifest and the worktree branch directly.
func detectMergedPR(cfg *config.Config, itemID string, item *model.Item) bool {
	// Check all repo worktrees that have PRs — ALL must be merged
	prDirs := allWorktreeDirsWithPR(cfg, itemID)
	if len(prDirs) == 0 {
		// No known PRs — check all worktrees as fallback
		prDirs = allWorktreeDirs(cfg, itemID)
	}
	if len(prDirs) == 0 {
		return false
	}

	foundAny := false
	for _, dir := range prDirs {
		cmd := ghPRCmd(dir, "view --json state -q .state")
		out, exitCode, _ := runCmdInDir(dir, cmd+" 2>/dev/null")
		if exitCode != 0 || strings.TrimSpace(string(out)) == "" {
			continue // no PR in this repo
		}
		foundAny = true
		state := strings.TrimSpace(string(out))
		if state != "MERGED" {
			return false // at least one PR is not merged
		}
	}
	return foundAny
}

// autoCloseItem was removed — merged PRs now continue through the pipeline
// (deploy verification, smoke, UAT, approval) instead of being force-closed.

// ensureAWSCredentials checks if AWS credentials are valid and runs SSO login if needed.
func ensureAWSCredentials(cfg *config.Config) {
	if cfg.Evidence == nil || cfg.Evidence.Backend != "s3" {
		return
	}

	profile := cfg.Evidence.S3Profile
	if profile == "" {
		return
	}

	// Test credentials with a lightweight STS call
	args := []string{"sts", "get-caller-identity", "--profile", profile}
	cmd := exec.Command("aws", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err == nil {
		return // credentials are valid
	}

	// Credentials expired or missing — run SSO login
	fmt.Printf("[st run] AWS credentials expired for profile %s — logging in...\n", profile)
	loginCmd := exec.Command("aws", "sso", "login", "--profile", profile)
	loginCmd.Stdin = os.Stdin
	loginCmd.Stdout = os.Stdout
	loginCmd.Stderr = os.Stderr
	if err := loginCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[st run] AWS SSO login failed: %v (evidence uploads will be skipped)\n", err)
	}
}

// resolveGHRepo detects the GitHub "owner/repo" from a worktree's git remote.
// Returns e.g. "TheraPrac/theraprac-web" or "" if not detectable.
func resolveGHRepo(worktreeDir string) string {
	out, exitCode, _ := runCmdInDir(worktreeDir, "gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null")
	if exitCode == 0 {
		repo := strings.TrimSpace(string(out))
		if repo != "" {
			return repo
		}
	}
	// Fallback: parse git remote
	out, _, _ = runCmdInDir(worktreeDir, "git remote get-url origin 2>/dev/null")
	remote := strings.TrimSpace(string(out))
	// Parse github.com:Owner/Repo.git or https://github.com/Owner/Repo.git
	for _, prefix := range []string{"git@github.com:", "https://github.com/"} {
		if strings.HasPrefix(remote, prefix) {
			repo := strings.TrimPrefix(remote, prefix)
			repo = strings.TrimSuffix(repo, ".git")
			return repo
		}
	}
	return ""
}

// ghPRCmd builds a gh pr command with --repo flag and branch name for worktree context.
func ghPRCmd(worktreeDir, subcmd string) string {
	repo := resolveGHRepo(worktreeDir)
	if repo == "" {
		return fmt.Sprintf("gh pr %s", subcmd)
	}
	// When using --repo, gh pr view needs the branch name
	branch := ""
	out, _, _ := runCmdInDir(worktreeDir, "git branch --show-current 2>/dev/null")
	branch = strings.TrimSpace(string(out))
	if branch != "" {
		return fmt.Sprintf("gh pr %s %s --repo %s", subcmd, branch, repo)
	}
	return fmt.Sprintf("gh pr %s --repo %s", subcmd, repo)
}

// injectGHPRContext rewrites a "gh pr <subcmd> ..." command to include
// branch and --repo flags in the correct position (after the subcommand).
// e.g., "gh pr checks --watch" → "gh pr checks <branch> --repo <repo> --watch"
func injectGHPRContext(cmd, branch, repo string) string {
	// Find "gh pr " prefix and extract the subcommand
	idx := strings.Index(cmd, "gh pr ")
	if idx < 0 {
		return cmd
	}
	prefix := cmd[:idx]
	rest := cmd[idx+len("gh pr "):]

	// Split rest into subcommand and remaining args
	// e.g., "checks --watch" → subcmd="checks", args="--watch"
	parts := strings.SplitN(rest, " ", 2)
	subcmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	// Rebuild: gh pr <subcmd> <branch> --repo <repo> <remaining-args>
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("gh pr ")
	b.WriteString(subcmd)
	if branch != "" {
		b.WriteString(" ")
		b.WriteString(branch)
	}
	if repo != "" {
		b.WriteString(" --repo ")
		b.WriteString(repo)
	}
	if args != "" {
		b.WriteString(" ")
		b.WriteString(args)
	}
	return b.String()
}

func isEligible(s *store.Store, cfg *config.Config, itemID string) bool {
	item, ok := s.Get(itemID)
	if !ok {
		return false
	}
	if cfg.IsTerminalStatus(item.Type, item.Status) {
		return false
	}
	// Allow items claimed by other sessions — runSingleItem handles
	// merged-PR detection and recovery before entering the pipeline
	return true
}

func slugFromTitle(title string) string {
	slug := strings.ToLower(title)
	slug = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r == ' ' || r == '-' || r == '_' {
			return '-'
		}
		return -1
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return slug
}

func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Idle timeouts — process is killed if no output for this long.
// Active processes that keep producing output run indefinitely.
// Claude needs a generous timeout — it legitimately thinks for long
// periods without producing stdout (especially during planning).
const defaultClaudeIdleTimeout = 15 * time.Minute
const defaultCommandIdleTimeout = 3 * time.Minute
const defaultCIIdleTimeout = 10 * time.Minute

// Hard safety cap — even active processes get killed after this.
const maxWallTimeout = 2 * time.Hour

func defaultRunClaude(cwd string, args []string, env []string) ([]byte, int, error) {
	// Use the run context if available (Ctrl+C cancels it), with wall timeout as safety
	parentCtx := context.Background()
	if activeRunCtx != nil {
		parentCtx = activeRunCtx
	}
	ctx, cancel := context.WithTimeout(parentCtx, maxWallTimeout)
	defer cancel()

	// Resolve claude binary — may not be on subprocess PATH
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return nil, 127, fmt.Errorf("claude not found in PATH")
	}
	cmd := exec.CommandContext(ctx, claudeBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("start: %w", err)
	}

	// Activity watchdog — kills process if no output for idleTimeout
	// Extract item ID from env for status display
	label := "claude"
	for _, e := range env {
		if strings.HasPrefix(e, "ST_RUN_ITEM=") {
			label = strings.TrimPrefix(e, "ST_RUN_ITEM=")
		}
	}
	stepName := "claude"
	for _, e := range env {
		if strings.HasPrefix(e, "ST_RUN_STEP=") {
			stepName = strings.TrimPrefix(e, "ST_RUN_STEP=")
		}
	}

	activity := &activityTracker{
		lastSeen:    time.Now(),
		startTime:   time.Now(),
		idleTimeout: defaultClaudeIdleTimeout,
		cancel:      cancel,
		label:       label,
		step:        stepName,
	}
	go activity.watch()

	// Read stream-json events, echo text, capture final result
	var lastResult []byte
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		activity.ping() // activity detected

		var event map[string]interface{}
		if json.Unmarshal(line, &event) != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "assistant":
			if msg, ok := event["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						block, ok := c.(map[string]interface{})
						if !ok {
							continue
						}
						blockType, _ := block["type"].(string)
						switch blockType {
						case "text":
							if text, ok := block["text"].(string); ok && text != "" {
								fmt.Fprint(os.Stderr, text)
							}
						case "tool_use":
							name, _ := block["name"].(string)
							input, _ := block["input"].(map[string]interface{})
							summary := formatToolCall(name, input)
							fmt.Fprintf(os.Stderr, "\n  -> %s\n", summary)
						}
					}
				}
			}
		case "result":
			lastResult = make([]byte, len(line))
			copy(lastResult, line)
		}
	}
	activity.stop()

	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			idle := time.Since(activity.lastSeen)
			if idle >= activity.idleTimeout {
				return lastResult, 1, fmt.Errorf("killed: no output for %s (idle timeout)", idle.Round(time.Second))
			}
			return lastResult, 1, fmt.Errorf("killed: wall time limit (%s)", maxWallTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			err = nil
		}
	}

	if len(lastResult) > 0 {
		return lastResult, exitCode, err
	}
	return nil, exitCode, err
}

// activityTracker monitors a subprocess for idle timeout.
// It kills the process (via context cancel) if no ping() is received within idleTimeout.
// Also prints periodic status ticks so the user knows it's alive.
type activityTracker struct {
	lastSeen    time.Time
	startTime   time.Time
	idleTimeout time.Duration
	cancel      context.CancelFunc
	label       string // item ID for display
	step        string // step name for display
	mu          sync.Mutex
	stopped     bool
}

func (a *activityTracker) ping() {
	a.mu.Lock()
	a.lastSeen = time.Now()
	a.mu.Unlock()
}

func (a *activityTracker) stop() {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
}

func (a *activityTracker) watch() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	statusCount := 0
	for range ticker.C {
		a.mu.Lock()
		if a.stopped {
			a.mu.Unlock()
			return
		}
		idle := time.Since(a.lastSeen)
		elapsed := time.Since(a.startTime)
		a.mu.Unlock()

		if idle >= a.idleTimeout {
			fmt.Fprintf(os.Stderr, "\n[st run] No activity for %s — killing process\n", idle.Round(time.Second))
			a.cancel()
			return
		}

		// Status tick every 30s so user knows it's alive
		statusCount++
		if statusCount%3 == 0 {
			if idle < 5*time.Second {
				fmt.Fprintf(os.Stderr, "\r[%s] %s — processing (%s elapsed)   ", a.label, a.step, elapsed.Round(time.Second))
			} else {
				fmt.Fprintf(os.Stderr, "\r[%s] %s — waiting (%s elapsed, last output %s ago)   ", a.label, a.step, elapsed.Round(time.Second), idle.Round(time.Second))
			}
		}
	}
}

// runCmdGuarded runs a shell command with activity-based timeout and streams output.
// idleTimeout: kill if no output for this long (0 = use default).
func runCmdGuarded(dir, command string, idleTimeout time.Duration) ([]byte, int, error) {
	if idleTimeout <= 0 {
		idleTimeout = defaultCommandIdleTimeout
	}

	parentCtx := context.Background()
	if activeRunCtx != nil {
		parentCtx = activeRunCtx
	}
	ctx, cancel := context.WithTimeout(parentCtx, maxWallTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}

	// Pipe stdout+stderr, stream to terminal, track activity
	stdoutPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}

	activity := &activityTracker{
		lastSeen:    time.Now(),
		startTime:   time.Now(),
		idleTimeout: idleTimeout,
		cancel:      cancel,
		label:       "cmd",
		step:        truncate(command, 30),
	}
	go activity.watch()

	// Read stdout, echo to terminal, capture
	var captured bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, readErr := stdoutPipe.Read(buf)
		if n > 0 {
			activity.ping()
			os.Stderr.Write(buf[:n]) // echo to terminal
			captured.Write(buf[:n])
		}
		if readErr != nil {
			break
		}
	}
	activity.stop()

	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			idle := time.Since(activity.lastSeen)
			if idle >= idleTimeout {
				return captured.Bytes(), 1, fmt.Errorf("killed: no output for %s", idle.Round(time.Second))
			}
			return captured.Bytes(), 1, fmt.Errorf("killed: wall time limit")
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			err = nil
		}
	}
	return captured.Bytes(), exitCode, err
}

// formatToolCall returns a concise summary of a claude tool invocation.
func formatToolCall(name string, input map[string]interface{}) string {
	switch name {
	case "Read":
		if p, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Read %s", shortenPath(p))
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("Glob %s", p)
		}
	case "Grep":
		pat, _ := input["pattern"].(string)
		path, _ := input["path"].(string)
		return fmt.Sprintf("Grep %q in %s", truncate(pat, 30), shortenPath(path))
	case "Edit":
		if p, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Edit %s", shortenPath(p))
		}
	case "Write":
		if p, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("Write %s", shortenPath(p))
		}
	case "Bash":
		if c, ok := input["command"].(string); ok {
			return fmt.Sprintf("$ %s", truncate(c, 60))
		}
	case "Agent":
		if d, ok := input["description"].(string); ok {
			return fmt.Sprintf("Agent: %s", d)
		}
	}
	return name
}

func shortenPath(p string) string {
	// Show last 3 path components
	parts := strings.Split(p, "/")
	if len(parts) > 3 {
		return ".../" + strings.Join(parts[len(parts)-3:], "/")
	}
	return p
}

func defaultPromptUser(_ string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return reader.ReadString('\n')
}

// engineSelectMenu uses the engine override if set, otherwise the real terminal menu.
func engineSelectMenu(engine RunEngine, prompt string, options []menuOption, defaultIdx int) string {
	if engine.SelectMenu != nil {
		return engine.SelectMenu(prompt, options, defaultIdx)
	}
	return selectMenu(prompt, options, defaultIdx)
}

// engineConfirmPrompt uses the engine override if set, otherwise the real terminal prompt.
func engineConfirmPrompt(engine RunEngine, prompt string) bool {
	if engine.ConfirmPrompt != nil {
		return engine.ConfirmPrompt(prompt)
	}
	return confirmPrompt(prompt)
}

// --- Sprint loading ---

func loadSprintGroups(s *store.Store, cfg *config.Config, sprintID string) ([][]string, *registry.Sprint, int) {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return nil, nil, 1
	}

	sp, err := r.SprintByID(sprintID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return nil, nil, 1
	}

	if len(sp.Items) == 0 {
		fmt.Println("Sprint has no items")
		return nil, nil, 0
	}

	// Compute parallel groups (reuse sprint_plan logic)
	sprintItems := make(map[string]bool)
	for _, id := range sp.Items {
		sprintItems[id] = true
	}
	intraDeps := make(map[string][]string)
	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		for _, depID := range item.DependsOn {
			if sprintItems[depID] {
				intraDeps[itemID] = append(intraDeps[itemID], depID)
			}
		}
	}
	groups := computeParallelGroups(sp.Items, intraDeps, s)

	return groups, sp, 0
}

// --- Output ---

func printDryRun(s *store.Store, cfg *config.Config, sp *registry.Sprint, groups [][]string, pipeline []config.RunStepDef, opts RunOpts) int {
	fmt.Printf("Sprint: %s — %s\n", sp.ID, sp.Title)
	fmt.Printf("Pipeline: %d steps\n\n", len(pipeline))

	for i, step := range pipeline {
		fmt.Printf("  %d. [%s] %s\n", i+1, step.Type, step.Name())
	}
	fmt.Println()

	for i, group := range groups {
		fmt.Printf("Group %d:\n", i+1)
		for _, id := range group {
			item, ok := s.Get(id)
			status := "eligible"
			title := id
			if ok {
				title = item.Title
				if cfg.IsTerminalStatus(item.Type, item.Status) {
					status = "done"
				} else if item.ClaimedBy != "" {
					status = "claimed"
				} else if opts.ItemFilter != "" && id != opts.ItemFilter {
					status = "filtered"
				}
			}
			fmt.Printf("  %-8s %-40s %s\n", id, truncate(title, 40), status)
		}
	}
	return 0
}

func printCompletionReport(results []ItemResult, sprintID string, totalDuration time.Duration) {
	// Load full context for the report
	cfg, cfgErr := config.Load(".")
	if cfgErr != nil || !cfg.Discovered {
		printSimpleReport(results, sprintID, totalDuration)
		return
	}
	reg, regErr := registry.Load(cfg.EpicsPath())
	if regErr != nil {
		printSimpleReport(results, sprintID, totalDuration)
		return
	}
	s, storeErr := store.New(cfg)
	if storeErr != nil {
		printSimpleReport(results, sprintID, totalDuration)
		return
	}

	sp, spErr := reg.SprintByID(sprintID)
	if spErr != nil {
		printSimpleReport(results, sprintID, totalDuration)
		return
	}

	// Find the epic and all its sprints
	var epic *registry.Epic
	for i := range reg.Epics {
		if reg.Epics[i].ID == sp.Epic {
			epic = &reg.Epics[i]
			break
		}
	}

	sep := strings.Repeat("─", 105)

	fmt.Println()
	if epic != nil {
		fmt.Printf("  Epic: %s\n", epic.Title)
	}
	fmt.Printf("  %s\n", sep)
	fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %10s\n",
		"ITEM", "STATUS", "ST TIME", "AI TIME", "COST", "SESSION $")
	fmt.Printf("  %s\n", sep)

	// Collect all sprints in this epic
	var sprintIDs []string
	if epic != nil && len(epic.SprintOrder) > 0 {
		sprintIDs = epic.SprintOrder
	} else if epic != nil {
		for _, es := range reg.Sprints {
			if es.Epic == epic.ID {
				sprintIDs = append(sprintIDs, es.ID)
			}
		}
	} else {
		sprintIDs = []string{sprintID}
	}

	var epicWall, epicAI time.Duration
	var epicCost float64

	for _, sid := range sprintIDs {
		sprint, err := reg.SprintByID(sid)
		if err != nil {
			continue
		}

		isCurrent := sid == sprintID
		marker := ""
		if isCurrent {
			marker = " ◀"
		}
		fmt.Printf("\n  %-40s%s\n", sprint.Title, marker)

		var sprintWall, sprintAI time.Duration
		var sprintCost float64
		sprintDone, sprintTotal := 0, 0

		for _, itemID := range sprint.Items {
			sprintTotal++
			item, ok := s.Get(itemID)
			if !ok {
				fmt.Printf("  %-8s %-22s\n", itemID, "(not found)")
				continue
			}

			// Determine status
			status := item.Status
			if cfg.IsTerminalStatus(item.Type, item.Status) {
				status = "done"
				sprintDone++
			}

			// Session values (this run only) and total values (accumulated)
			var sessCost float64
			var totalWall, totalAI time.Duration
			var totalCostItem float64

			if isCurrent {
				for _, r := range results {
					if r.ItemID == itemID {
						sessCost = r.TotalCost
						if !r.Success {
							for _, sr := range r.Steps {
								if !sr.Passed {
									status = "fail@" + sr.Step
									break
								}
							}
						} else {
							status = "done"
						}
						break
					}
				}
			}

			// Accumulated totals from time_tracking
			if v, ok := getNestedField(item, "time_tracking", "run_wall_seconds"); ok {
				var secs int
				fmt.Sscanf(v, "%d", &secs)
				totalWall = time.Duration(secs) * time.Second
			}
			if v, ok := getNestedField(item, "time_tracking", "ai_duration_seconds"); ok {
				var secs int
				fmt.Sscanf(v, "%d", &secs)
				totalAI = time.Duration(secs) * time.Second
			}
			if v, ok := getNestedField(item, "time_tracking", "ai_cost_usd"); ok {
				fmt.Sscanf(v, "%f", &totalCostItem)
			}

			sprintWall += totalWall
			sprintAI += totalAI
			sprintCost += totalCostItem

			f := func(d time.Duration) string {
				if d > 0 { return formatDuration(d) }
				return "—"
			}
			fc := func(c float64) string {
				if c > 0 { return fmt.Sprintf("$%.2f", c) }
				return "—"
			}

			fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %10s\n",
				itemID, truncate(status, 22),
				f(totalWall), f(totalAI), fc(totalCostItem), fc(sessCost))
		}

		// Sprint subtotal
		fmt.Printf("  %-8s %-22s  %12s  %12s  %10s\n", "",
			fmt.Sprintf("%d/%d done", sprintDone, sprintTotal),
			formatDuration(sprintWall), formatDuration(sprintAI),
			fmt.Sprintf("$%.2f", sprintCost))

		epicWall += sprintWall
		epicAI += sprintAI
		epicCost += sprintCost
	}

	// Epic total
	if epic != nil && len(sprintIDs) > 1 {
		fmt.Printf("\n  %s\n", sep)
		fmt.Printf("  %-8s %-22s  %12s  %12s  %10s\n",
			"TOTAL", truncate(epic.Title, 22),
			formatDuration(epicWall), formatDuration(epicAI),
			fmt.Sprintf("$%.2f", epicCost))
	}
	fmt.Printf("  %s\n\n", sep)
}

// printSimpleReport is the fallback when registry/store aren't available.
func printSimpleReport(results []ItemResult, sprintID string, totalDuration time.Duration) {
	completed, failed := 0, 0
	var totalCost float64
	for _, r := range results {
		totalCost += r.TotalCost
		if r.Success {
			completed++
		} else {
			failed++
		}
	}
	fmt.Printf("\n=== Sprint %s ===\n", sprintID)
	fmt.Printf("  %d done, %d fail | Wall: %s | Cost: $%.2f\n\n",
		completed, failed, formatDuration(totalDuration), totalCost)
}
