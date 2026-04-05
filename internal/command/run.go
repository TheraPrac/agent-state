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

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
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

// ClaudeUsage represents token usage from claude -p --output-format json.
type ClaudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ClaudeResult represents parsed JSON output from claude -p --output-format json.
type ClaudeResult struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	DurationMs   int64       `json:"duration_ms"`
	SessionID    string      `json:"session_id"`
	NumTurns     int         `json:"num_turns"`
	Result       string      `json:"result"`
	IsError      bool        `json:"is_error"`
	Errors       []string    `json:"errors"`
	Usage        ClaudeUsage `json:"usage"`
}

// StepResult captures the outcome of a single pipeline step.
type StepResult struct {
	Step         string        `json:"step"`
	Type         string        `json:"type"`
	Passed       bool          `json:"passed"`
	Output       string        `json:"output,omitempty"`
	FullOutput   string        `json:"-"` // full untruncated output (not serialized)
	Error        string        `json:"error,omitempty"`
	Duration     time.Duration `json:"duration"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	AIDurationMs int64         `json:"ai_duration_ms,omitempty"`
	InputTokens  int           `json:"input_tokens,omitempty"`
	OutputTokens int           `json:"output_tokens,omitempty"`
}

// ItemResult captures the outcome of running one sprint item.
type ItemResult struct {
	ItemID       string        `json:"item_id"`
	Title        string        `json:"title"`
	Steps        []StepResult  `json:"steps"`
	Success      bool          `json:"success"`
	TotalCost    float64       `json:"total_cost"`
	Duration     time.Duration `json:"duration"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
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
	fmt.Printf("\n    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s  %21s  %10s\n",
		"ITEM", "PROGRESS", "STATUS", "CREATED", "WALL", "ST TIME", "AI TIME", "COST", "TOKENS (I/O/T)", "NET LOC")
	fmt.Println("    " + strings.Repeat("-", 148))

	for _, epic := range reg.Epics {
		if epic.Status != "active" {
			continue
		}
		epicHasItems := false
		var epicWall, epicST, epicAI time.Duration
		var epicCost float64
		var epicInTok, epicOutTok int
		var epicNetLOC int

		for _, sp := range reg.Sprints {
			if sp.Epic != epic.ID || len(sp.Items) == 0 {
				continue
			}
			if !epicHasItems {
				fmt.Printf("\nEpic: %s  (%s)\n", epic.Title, epic.ID)
				epicHasItems = true
			}
			var sprintWall, sprintST, sprintAI time.Duration
			var sprintCost float64
			var sprintInTok, sprintOutTok int
			var sprintNetLOC int

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
			fmt.Printf("    %s\n", sp.ID)

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

				// Token counts
				var itemInTok, itemOutTok int
				if tt := item.TimeTracking; tt != nil {
					if raw, ok := tt["input_tokens"]; ok {
						switch v := raw.(type) {
						case float64:
							itemInTok = int(v)
						case int:
							itemInTok = v
						case string:
							fmt.Sscanf(v, "%d", &itemInTok)
						}
					}
					if raw, ok := tt["output_tokens"]; ok {
						switch v := raw.(type) {
						case float64:
							itemOutTok = int(v)
						case int:
							itemOutTok = v
						case string:
							fmt.Sscanf(v, "%d", &itemOutTok)
						}
					}
				}
				tokStr := ""
				if itemInTok > 0 || itemOutTok > 0 {
					tokStr = fmt.Sprintf("%s/%s/%s", formatTokens(itemInTok), formatTokens(itemOutTok), formatTokens(itemInTok+itemOutTok))
				}

				// Net LOC from PR manifest
				var itemNetLOC int
				if m, err := manifest.Load(cfg.ManifestDir(), itemID); err == nil {
					for _, pr := range m.PRs {
						itemNetLOC += pr.CodeStats.Insertions - pr.CodeStats.Deletions
					}
				}
				locStr := ""
				if itemNetLOC != 0 {
					locStr = formatLOC(itemNetLOC)
				}

				// Accumulate sprint totals
				sprintWall += wallDur
				sprintST += stDur
				sprintAI += aiDur
				sprintCost += itemCost
				sprintInTok += itemInTok
				sprintOutTok += itemOutTok
				sprintNetLOC += itemNetLOC

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
				planBadge := ""
				if item.PlanApproved {
					planBadge = fmt.Sprintf("  %s󰙅%s", "\033[32m", "\033[0m")
				}
				fmt.Printf("      %-80s%s\n", title, planBadge)
				fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s  %21s  %10s%s\n",
					itemID, bar, statusLabel, createdStr, wallStr, stStr, aiStr, costStr, tokStr, locStr, inFlight)
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
				sprintTokStr := ""
				if sprintInTok > 0 || sprintOutTok > 0 {
					sprintTokStr = fmt.Sprintf("%s/%s/%s", formatTokens(sprintInTok), formatTokens(sprintOutTok), formatTokens(sprintInTok+sprintOutTok))
				}
				sprintLOCStr := ""
				if sprintNetLOC != 0 {
					sprintLOCStr = formatLOC(sprintNetLOC)
				}
				fmt.Printf("    %s\n", strings.Repeat("─", 148))
				fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s  %21s  %10s\n",
					"", "", fmt.Sprintf("%d/%d done", done, len(sp.Items)), "", sprintWallStr, sprintSTStr, sprintAIStr, sprintCostStr, sprintTokStr, sprintLOCStr)
			}

			// Accumulate epic totals
			epicWall += sprintWall
			epicST += sprintST
			epicAI += sprintAI
			epicCost += sprintCost
			epicInTok += sprintInTok
			epicOutTok += sprintOutTok
			epicNetLOC += sprintNetLOC
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
			epicTokStr := ""
			if epicInTok > 0 || epicOutTok > 0 {
				epicTokStr = fmt.Sprintf("%s/%s/%s", formatTokens(epicInTok), formatTokens(epicOutTok), formatTokens(epicInTok+epicOutTok))
			}
			epicLOCStr := ""
			if epicNetLOC != 0 {
				epicLOCStr = formatLOC(epicNetLOC)
			}
			fmt.Printf("\n    %s\n", strings.Repeat("═", 148))
			fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s  %21s  %10s\n",
				"TOTAL", "", epic.Title, "", epicWallStr, epicSTStr, epicAIStr, epicCostStr, epicTokStr, epicLOCStr)
			fmt.Printf("    %s\n", strings.Repeat("═", 148))
		}
	}
	// Standalone items — active items not in any sprint
	sprintItems := make(map[string]bool)
	for _, sp := range reg.Sprints {
		for _, id := range sp.Items {
			sprintItems[id] = true
		}
	}
	var standalone []*model.Item
	for _, item := range s.All() {
		if item.Status == "active" && !sprintItems[item.ID] {
			standalone = append(standalone, item)
		}
	}
	if len(standalone) > 0 {
		fmt.Printf("\nStandalone (not in a sprint)\n")
		for _, item := range standalone {
			// Reuse the same rendering logic as sprint items
			lastStep, _ := getNestedField(item, "delivery", "last_completed_step")
			isDone := cfg.IsTerminalStatus(item.Type, item.Status)

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

			statusLabel := item.Status
			if lastStep != "" {
				nextIdx := stepIndex(lastStep) + 1
				if nextIdx < totalSteps {
					statusLabel = stepNames[nextIdx]
				}
			}
			if len(statusLabel) > 22 {
				statusLabel = statusLabel[:22]
			}

			inFlight := ""
			if item.ClaimedBy != "" {
				inFlight = "  << RUNNING"
			}

			wallStr := ""
			if tt := item.TimeTracking; tt != nil {
				if v, ok := tt["started_at"]; ok {
					if startStr, ok := v.(string); ok {
						if started, err := time.Parse(time.RFC3339, startStr); err == nil {
							wallStr = formatDuration(now.Sub(started))
						}
					}
				}
			}
			stStr := ""
			if tt := item.TimeTracking; tt != nil {
				if raw, ok := tt["run_wall_seconds"]; ok {
					var secs float64
					switch v := raw.(type) {
					case float64:
						secs = v
					case int:
						secs = float64(v)
					}
					if secs > 0 {
						stStr = formatDuration(time.Duration(secs) * time.Second)
					}
				}
			}
			aiStr := ""
			if tt := item.TimeTracking; tt != nil {
				if raw, ok := tt["ai_duration_seconds"]; ok {
					var secs float64
					switch v := raw.(type) {
					case float64:
						secs = v
					case int:
						secs = float64(v)
					}
					if secs > 0 {
						aiStr = formatDuration(time.Duration(secs) * time.Second)
					}
				}
			}
			costStr := ""
			if tt := item.TimeTracking; tt != nil {
				if raw, ok := tt["ai_cost_usd"]; ok {
					var cost float64
					switch v := raw.(type) {
					case float64:
						cost = v
					}
					if cost > 0 {
						costStr = fmt.Sprintf("$%.2f", cost)
					}
				}
			}

			createdStr := ""
			if created, ok := item.Doc.GetField("created"); ok {
				if t, err := time.Parse(time.RFC3339, created); err == nil {
					createdStr = t.Format("Jan 02")
				}
			}

			title := item.Title
			if len(title) > 80 {
				title = title[:77] + "..."
			}
			tokStr := ""
			if tt := item.TimeTracking; tt != nil {
				var inTok, outTok int
				if raw, ok := tt["input_tokens"]; ok {
					switch v := raw.(type) {
					case float64:
						inTok = int(v)
					case int:
						inTok = v
					case string:
						fmt.Sscanf(v, "%d", &inTok)
					}
				}
				if raw, ok := tt["output_tokens"]; ok {
					switch v := raw.(type) {
					case float64:
						outTok = int(v)
					case int:
						outTok = v
					case string:
						fmt.Sscanf(v, "%d", &outTok)
					}
				}
				if inTok > 0 || outTok > 0 {
					tokStr = fmt.Sprintf("%s/%s/%s", formatTokens(inTok), formatTokens(outTok), formatTokens(inTok+outTok))
				}
			}

			locStr := ""
			if m, err := manifest.Load(cfg.ManifestDir(), item.ID); err == nil {
				var netLOC int
				for _, pr := range m.PRs {
					netLOC += pr.CodeStats.Insertions - pr.CodeStats.Deletions
				}
				if netLOC != 0 {
					locStr = formatLOC(netLOC)
				}
			}

			fmt.Printf("      %s\n", title)
			fmt.Printf("    %-8s %-15s %-22s %-8s  %12s  %12s  %10s  %10s  %21s  %10s%s\n",
				item.ID, bar, statusLabel, createdStr, wallStr, stStr, aiStr, costStr, tokStr, locStr, inFlight)
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

	// Clean stale locks — remove locks for items no longer active
	store.CleanStaleLocks(cfg, func(id string) bool {
		item, ok := s.Get(id)
		if !ok {
			return false
		}
		tc, ok := cfg.Types[item.Type]
		if !ok {
			return false
		}
		return item.Status == tc.ActiveStatus
	})

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
		fmt.Printf("\nDone: %s (cost: $%.2f, tokens: %s in / %s out)\n", itemID, result.TotalCost, formatTokens(result.InputTokens), formatTokens(result.OutputTokens))
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

	// Check if this is a verification-only item (no code changes needed).
	// Plan sidecar exists with empty files lists → skip implement through merge.
	if p, pErr := plan.Load(cfg.PlansDir(), itemID); pErr == nil && p != nil && p.Approved {
		if len(p.FilesToCreate) == 0 && len(p.FilesToModify) == 0 {
			lastStep, _ := getNestedField(item, "delivery", "last_completed_step")
			if lastStep == "" || lastStep == "plan" {
				fmt.Printf("[%s] Verification-only item (no code changes) — skipping to verify_tests\n", itemID)
				setNestedField(item, "delivery", "last_completed_step", "merge")
				setNestedField(item, "delivery", "stage", "verification")
				item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
				localStore.Write(item)
				localStore, _ = store.New(cfg)
			}
		}
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

	// Recovery: an active item whose worktrees no longer exist (e.g. cleaned
	// up by `st finish` after a prior aborted run) needs to be restarted so
	// the pipeline has a directory to run in. Reset to start status so the
	// Start() call below recreates the worktrees cleanly.
	if item.Status == tc.ActiveStatus && cfg.Worktree != nil && cfg.Worktree.Enabled {
		wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
		if _, err := os.Stat(wtBase); os.IsNotExist(err) {
			fmt.Printf("[%s] Active item with missing worktree — recreating\n", itemID)
			item.Doc.SetField("status", tc.StartStatus)
			item.Status = tc.StartStatus
			if item.ClaimedBy != "" {
				mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
				_ = mgr.RemoveClaim(item.ClaimedBy, itemID)
				item.ClaimedBy = ""
				item.ClaimedAt = ""
				item.Doc.SetField("claimed_by", "")
				item.Doc.SetField("claimed_at", "")
			}
			store.UnlockItem(cfg, itemID)
			localStore.Write(item)
			localStore, _ = store.New(cfg)
			item, _ = localStore.Get(itemID)
		}
	}

	if item.Status == tc.StartStatus {
		// Precheck dependencies so we can report "blocked" instead of the
		// misleading "fail@start" when unresolved deps are the reason.
		g := deps.Build(localStore.All(), cfg)
		if g.IsBlocked(itemID) {
			unresolved := g.UnresolvedDeps(itemID)
			fmt.Printf("[%s] Blocked by: %v\n", itemID, unresolved)
			result.Steps = append(result.Steps, StepResult{
				Step:  "blocked",
				Error: fmt.Sprintf("blocked by: %v", unresolved),
			})
			return result
		}
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

	// Sync agent-state to git after this item finishes. Registered first
	// so it runs LAST (defers are LIFO), after recordRunMetrics has written
	// the final time_tracking totals. Without this, any uncommitted state
	// changes from close/metrics would be discarded by the next GitPull.
	defer func() {
		if syncStore, err := store.New(cfg); err == nil {
			msg := fmt.Sprintf("st run: %s", itemID)
			if updatedItem, ok := syncStore.Get(itemID); ok && cfg.IsTerminalStatus(updatedItem.Type, updatedItem.Status) {
				msg = fmt.Sprintf("st run: %s closed (%s)", itemID, updatedItem.Status)
			}
			if err := syncStore.GitSync(msg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] warning: sync after run failed: %v\n", itemID, err)
			}
		}
	}()

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

		// Ensure item is active before each step. Pipeline commands
		// (merge, deploy-check, smoke, test) require active status.
		// Status can drift if a previous fix attempt or concurrent
		// process changed it.
		if refreshStore, err := store.New(cfg); err == nil {
			if refreshItem, ok := refreshStore.Get(itemID); ok {
				if refreshItem.Status != "active" {
					refreshItem.Status = "active"
					refreshStore.Write(refreshItem)
					localStore, _ = store.New(cfg)
				}
			}
		}

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

		// Show item changes made by this step (reload and diff against pre-step snapshot)
		if step.Type == "claude" || step.Type == "uat_review" {
			localStore, _ = store.New(cfg) // reload after subprocess may have modified files
			if postItem, ok := localStore.Get(itemID); ok && postItem.Doc != nil {
				postContent := postItem.Doc.String()
				preContent := changelog.LastSnapshot(cfg, itemID, step.Name())
				if preContent != "" && preContent != postContent {
					diff := changelog.DiffSnapshot(preContent, postContent)
					if diff != "(no changes)" && diff != "(whitespace-only changes)" {
						fmt.Fprintf(os.Stderr, "\n[%s] Item changes from %s:\n%s", itemID, step.Name(), diff)
					}
				}
			}
		}

		result.Steps = append(result.Steps, sr)
		result.TotalCost += sr.CostUSD
		result.InputTokens += sr.InputTokens
		result.OutputTokens += sr.OutputTokens

		if !sr.Passed {
			fmt.Printf("[%s] Step %s FAILED: %s\n", itemID, step.Name(), sr.Error)

			// Structural errors that can't be fixed by running more claude —
			// the subprocess can't even start. Bail out immediately instead
			// of burning fix attempts.
			structuralErr := strings.Contains(sr.Error, "chdir") &&
				strings.Contains(sr.Error, "no such file or directory")
			if structuralErr {
				fmt.Printf("[%s] Structural error (worktree missing) — cannot fix via retry\n", itemID)
				releaseItem(cfg, itemID)
				return result
			}

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
							"If the failure is NOT caused by this item's changes (e.g. pre-existing flaky test, "+
							"infrastructure/permission error, item status issue), include the text [NOT_FIXABLE] "+
							"in your final response. This signals the pipeline to auto-skip instead of retrying.\n\n"+
							"Commit and push any fixes. Do NOT merge. Follow all procedures in CLAUDE.md.",
						stepLabel, itemID, attempt, sr.Error)
					fixStep := config.RunStepDef{Type: "claude", Prompt: fixPrompt}
					fixStep.SetName(fmt.Sprintf("ci_fix_%d", attempt))
					fixSR := executeClaude(s, cfg, itemID, sprintID, fixStep, opts, engine, worktreeDir, claudeSessionID, true)
					result.Steps = append(result.Steps, fixSR)
					result.TotalCost += fixSR.CostUSD
					result.InputTokens += fixSR.InputTokens
					result.OutputTokens += fixSR.OutputTokens

					// Check for "not fixable" signal: if the fix agent's output
					// contains the marker, auto-skip instead of retrying.
					if fixSR.Passed && strings.Contains(fixSR.FullOutput, "[NOT_FIXABLE]") {
						fmt.Printf("[%s] Fix agent reported NOT_FIXABLE — auto-skipping %s\n", itemID, step.Name())
						result.Steps = append(result.Steps, StepResult{
							Step: step.Name(), Type: "skipped", Passed: true,
							Output: "auto-skipped: fix agent reported NOT_FIXABLE",
						})
						fixed = true
						break
					}

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
						result.InputTokens += sr2.InputTokens
						result.OutputTokens += sr2.OutputTokens

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

	// Accumulate token counts
	if result.InputTokens > 0 {
		prev := readIntField(item, "time_tracking", "input_tokens")
		setNestedField(item, "time_tracking", "input_tokens", fmt.Sprintf("%d", prev+result.InputTokens))
	}
	if result.OutputTokens > 0 {
		prev := readIntField(item, "time_tracking", "output_tokens")
		setNestedField(item, "time_tracking", "output_tokens", fmt.Sprintf("%d", prev+result.OutputTokens))
	}
	if result.InputTokens > 0 || result.OutputTokens > 0 {
		prev := readIntField(item, "time_tracking", "total_tokens")
		setNestedField(item, "time_tracking", "total_tokens", fmt.Sprintf("%d", prev+result.InputTokens+result.OutputTokens))
	}

	// Accumulate AI duration from all steps that report it (claude, plan, uat_review, etc.)
	var aiDurationMs int64
	for _, sr := range result.Steps {
		aiDurationMs += sr.AIDurationMs
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
	agentID := cfg.AgentID()
	if agentID != "" {
		item.Doc.SetField("last_touched_by", agentID)
	} else {
		item.Doc.SetField("last_touched_by", "st-run")
	}
	localStore.Write(item)
}

// appendAISessionRecord adds a line to the work_tracking.ai_sessions list
// with per-invocation stats: session ID, step, cost, duration, timestamp.
func appendAISessionRecord(item *model.Item, result ItemResult) {
	for _, sr := range result.Steps {
		if sr.Type != "claude" || sr.CostUSD == 0 {
			continue
		}
		// Format: "cost:$X.XXXX duration:Xs in:N out:N step:<name> at:<timestamp>"
		aiDur := time.Duration(sr.AIDurationMs) * time.Millisecond
		record := fmt.Sprintf("cost:$%.4f duration:%s in:%d out:%d step:%s at:%s",
			sr.CostUSD, aiDur.Round(time.Second),
			sr.InputTokens, sr.OutputTokens,
			sr.Step, time.Now().Format(time.RFC3339))

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
	stepStart := time.Now()

	var sr StepResult
	switch step.Type {
	case "plan":
		sr = executePlanWithOpts(s, cfg, itemID, engine, opts, worktreeDir)
	case "claude":
		sr = executeClaude(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID, isResume)
	case "test":
		sr = executeTest(s, cfg, itemID, step, worktreeDir)
	case "verify_tests":
		sr = executeVerifyTests(s, cfg, itemID)
	case "pr":
		sr = executePR(s, cfg, itemID, step, worktreeDir)
	case "merge":
		sr = executeMerge(s, cfg, itemID, worktreeDir)
	case "merge_precheck":
		sr = executeMergePrecheck(cfg, itemID, worktreeDir)
	case "deploy":
		sr = executeDeploy(s, cfg, itemID, worktreeDir)
	case "smoke":
		sr = executeSmoke(s, cfg, itemID, worktreeDir)
	case "uat":
		sr = executeUAT(s, cfg, itemID, worktreeDir)
	case "gate":
		sr = executeGate(itemID, engine)
	case "uat_review":
		sr = executeUATReview(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID)
	case "close":
		sr = executeClose(s, cfg, itemID, step)
	case "command":
		sr = executeCommand(cfg, itemID, sprintID, step, worktreeDir)
	default:
		sr = StepResult{Step: step.Name(), Type: step.Type, Error: fmt.Sprintf("unknown step type: %s", step.Type)}
	}

	// Ensure every step has wall-clock duration recorded.
	// Steps that already set Duration (e.g. claude) keep theirs.
	if sr.Duration == 0 {
		sr.Duration = time.Since(stepStart)
	}

	return sr
}

// --- Step executors ---

func executeClaude(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, isResume bool) StepResult {
	sr := StepResult{Step: step.Name(), Type: "claude"}

	// Snapshot item state before this step for diff tracking
	var preSnapshot string
	if item, ok := s.Get(itemID); ok && item.Doc != nil {
		preSnapshot = item.Doc.String()
		changelog.Snapshot(cfg, itemID, step.Name(), preSnapshot)
	}

	// Set active session for changelog grouping
	prevSession := changelog.ActiveSessionID
	changelog.ActiveSessionID = fmt.Sprintf("%s/%s", itemID, step.Name())
	defer func() { changelog.ActiveSessionID = prevSession }()

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
	sr.InputTokens = claudeResult.Usage.InputTokens + claudeResult.Usage.CacheCreationInputTokens + claudeResult.Usage.CacheReadInputTokens
	sr.OutputTokens = claudeResult.Usage.OutputTokens
	sr.Output = truncate(claudeResult.Result, 500)
	sr.FullOutput = claudeResult.Result

	if exitCode != 0 || (claudeResult.Subtype != "" && claudeResult.Subtype != "success") {
		// Check for expired/invalid session — retry with fresh session
		if isResume && claudeResult.Subtype == "error_during_execution" {
			for _, errMsg := range claudeResult.Errors {
				if strings.Contains(errMsg, "No conversation found") {
					fmt.Fprintf(os.Stderr, "[%s] Session expired — retrying with fresh session\n", itemID)
					// Remove --resume and --session-id args, start fresh
					var freshArgs []string
					for i := 0; i < len(args); i++ {
						if args[i] == "--resume" || args[i] == "--session-id" {
							i++ // skip the value too
							continue
						}
						freshArgs = append(freshArgs, args[i])
					}
					newSessionID := generateSessionID()
					freshArgs = append(freshArgs, "--session-id", newSessionID)
					output2, exitCode2, err2 := engine.RunClaude(worktreeDir, freshArgs, env)
					if err2 != nil {
						sr.Error = fmt.Sprintf("fresh session exec error: %v", err2)
						return sr
					}
					cr2, pe2 := parseClaudeOutput(output2)
					if pe2 != nil {
						if exitCode2 != 0 {
							sr.Error = fmt.Sprintf("claude exited %d (fresh)", exitCode2)
							return sr
						}
						sr.Passed = true
						sr.Output = truncate(string(output2), 500)
						return sr
					}
					sr.CostUSD = cr2.TotalCostUSD
					sr.AIDurationMs = cr2.DurationMs
					sr.InputTokens = cr2.Usage.InputTokens + cr2.Usage.CacheCreationInputTokens + cr2.Usage.CacheReadInputTokens
					sr.OutputTokens = cr2.Usage.OutputTokens
					sr.Output = truncate(cr2.Result, 500)
					sr.FullOutput = cr2.Result
					if exitCode2 == 0 && (cr2.Subtype == "" || cr2.Subtype == "success") {
						sr.Passed = true
					} else {
						sr.Error = fmt.Sprintf("claude exited %d (subtype: %s)", exitCode2, cr2.Subtype)
					}
					return sr
				}
			}
		}
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
		// No PR found. If the item's worktree branches have zero commits
		// relative to main, this is an operational-only item (e.g. worktree
		// cleanup, branch deletion) with no code to land. Mark it as no-op
		// and fast-forward past PR/test/deploy/smoke to close.
		if hasNoBranchCommits(cfg, itemID) {
			fmt.Printf("[%s] No code changes — marking as no-op, skipping PR/test/deploy steps\n", itemID)
			if localStore, err := store.New(cfg); err == nil {
				if item, ok := localStore.Get(itemID); ok {
					setNestedField(item, "delivery", "stage", "no_op")
					setNestedField(item, "delivery", "last_completed_step", "smoke")
					item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
					localStore.Write(item)
				}
			}
			sr.Passed = true
			sr.Output = "no-op item (zero commits)"
			return sr
		}
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

		// Bugbot gate: zero open findings required before merge.
		// Fetch inline comments from Bugbot — if any exist, block merge.
		if ghRepo != "" {
			prNum := detectPRNumber(prDir, ghRepo)
			if prNum > 0 {
				count := countBugbotFindings(ghRepo, prNum, prDir)
				if count > 0 {
					sr.Error = fmt.Sprintf("Cursor Bugbot has %d unresolved finding(s) on %s#%d — fix or resolve before merging", count, ghRepo, prNum)
					return sr
				}
			}
		}
	}
	sr.Passed = true
	return sr
}

func executeDeploy(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "deploy", Type: "deploy"}

	// No-op items were never merged/deployed — nothing to verify.
	if item, ok := s.Get(itemID); ok {
		if stage, _ := getNestedField(item, "delivery", "stage"); stage == "no_op" {
			sr.Passed = true
			sr.Output = "no-op item — skipping deploy check"
			return sr
		}
	}

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

	// No-op items have nothing deployed to smoke-test.
	if item, ok := s.Get(itemID); ok {
		if stage, _ := getNestedField(item, "delivery", "stage"); stage == "no_op" {
			sr.Passed = true
			sr.Output = "no-op item — skipping smoke"
			return sr
		}
	}

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

	autoFixCount := 0
	for iteration := 1; ; iteration++ {
		// Run UAT
		fmt.Printf("\n[%s] Running UAT (iteration %d)...\n", itemID, iteration)
		uatDir := worktreeDir
		if cfg.Worktree != nil && cfg.Worktree.Enabled {
			uatDir = filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
		}
		uatCode := UAT(s, cfg, itemID, UATOpts{
			RunCmd: func(cmd string) ([]byte, int, error) {
				// Rewrite ../repo paths for worktree context
				cmd = rewriteACPaths(cfg, itemID, uatDir, cmd)
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
		reportStart := time.Now()
		reportSR := executeClaude(s, cfg, itemID, sprintID, reportStep, opts, engine, worktreeDir, claudeSessionID, true)
		reviewDur := time.Since(reportStart)
		sr.CostUSD += reportSR.CostUSD
		sr.AIDurationMs += reportSR.AIDurationMs

		// Reload item for current state
		s, _ = store.New(cfg)
		reviewItem, _ := s.Get(itemID)
		itemTitle := ""
		if reviewItem != nil {
			itemTitle = reviewItem.Title
		}

		// Extract recommendation from claude's output
		rec := extractRecommendation(reportSR.FullOutput)

		// Auto-fix "Accept with notes" — feed notes back to claude without user input
		if isAcceptWithNotes(rec) && autoFixCount < maxAutoFixIterations {
			autoFixCount++
			fmt.Printf("[%s] UAT returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
				itemID, autoFixCount, maxAutoFixIterations)
			notes := extractNotesFromReview(reportSR.FullOutput)
			s, _ = store.New(cfg)
			if fixItem, ok := s.Get(itemID); ok {
				runAutoFixFromNotes(s, cfg, itemID, sprintID, fixItem, "UAT review", notes, opts, engine, worktreeDir, claudeSessionID, &sr)
			}
			continue // re-run UAT
		}

		gateMu.Lock()
		choice := showReviewGate(ReviewGateInfo{
			ItemID:         itemID,
			Title:          itemTitle,
			GateType:       "UAT Review",
			Iteration:      iteration,
			Recommendation: rec,
			ReviewDuration: reviewDur,
		}, []menuOption{
			{"1", "Approve     — accept and close"},
			{"2", "Reject      — stop and release for retry"},
			{"3", "Feedback    — type direction, claude acts, UAT re-runs"},
			{"4", "Interactive — full claude session (escape hatch)"},
		}, engine)
		gateMu.Unlock()

		if choice == "^C" {
			sr.Error = "interrupted by Ctrl+C"
			return sr
		}
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

		if choice == "3" {
			// Constrained feedback: user types direction, claude acts under pipeline rules
			reviewItem, _ := s.Get(itemID)
			runConstrainedFeedback(s, cfg, itemID, "", reviewItem, "UAT review", opts, engine, worktreeDir, claudeSessionID, &sr)
			fmt.Printf("\n[%s] Feedback applied. Re-running UAT...\n", itemID)
			s, _ = store.New(cfg)
			continue
		}

		if choice == "4" {
			// Interactive escape hatch
			runInteractiveEscapeHatch(itemID, worktreeDir, engine, cfg)
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
		fmt.Sprintf("Cost:   $%.2f  |  Tokens: %s in / %s out  |  Steps: %d  |  Fails: %d", result.TotalCost, formatTokens(result.InputTokens), formatTokens(result.OutputTokens), len(result.Steps), failCount),
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

	opts := []menuOption{
		{"c", "continue — resume pipeline"},
		{"s", "skip     — skip next step, continue"},
		{"a", "abort    — stop, release item for retry"},
	}

	// Auto-continue after timeout when recommendation is to continue.
	var choice string
	if strings.HasPrefix(recommendation, "[c]") {
		choice = engineSelectMenuTimed(engine, "", opts, 0, autoAcceptTimeout)
	} else {
		choice = engineSelectMenu(engine, "", opts, 0)
	}
	if choice == "^C" {
		return "abort"
	}
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

	// No-op items (housekeeping with zero code changes) have nothing to test.
	if stage, _ := getNestedField(item, "delivery", "stage"); stage == "no_op" {
		sr.Passed = true
		sr.Output = "no-op item — skipping tests"
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

		proposalResult, err := proposePlan(cfg, itemID, item, engine, opts, worktreeDir, needsSummary, needsACs)
		if err != nil {
			sr.Error = fmt.Sprintf("plan proposal failed: %v", err)
			return sr
		}
		sr.CostUSD += proposalResult.CostUSD
		sr.AIDurationMs += proposalResult.AIDurationMs

		fmt.Printf("\n=== Proposed Plan: %s ===\n", itemID)
		fmt.Printf("Title: %s\n", item.Title)
		fmt.Println(proposalResult.Text)

		// Save draft plan immediately — if session is killed, work is preserved
		s, _ = store.New(cfg)
		item, _ = s.Get(itemID)
		if !plan.Exists(cfg.PlansDir(), itemID) {
			draftPlan := &plan.Plan{
				Approach:   item.Summary,
				ACs:        item.AcceptanceCriteria,
				ScopeRepos: inferReposFromItem(cfg, item),
				RawText:    proposalResult.Text,
				Revisions: []plan.Revision{{
					Timestamp: plan.Now(),
					Summary:   "Draft — generated by claude, pending review",
				}},
			}
			if err := plan.Save(cfg.PlansDir(), itemID, draftPlan); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] Warning: failed to save draft plan: %v\n", itemID, err)
			} else {
				fmt.Printf("[%s] Draft plan saved\n", itemID)
			}
		}

		// Plan review loop — claude reviews, user decides
		autoFixCount := 0
		for iteration := 1; ; iteration++ {
			// Launch claude to critically review the plan
			s, _ = store.New(cfg) // reload in case claude updated fields
			item, _ = s.Get(itemID)

			reviewPrompt := buildPlanReviewPrompt(itemID, item)
			reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
			reviewStep.SetName("plan_review")
			reviewStart := time.Now()
			reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, opts, engine, worktreeDir, "", false)
			reviewDur := time.Since(reviewStart)
			sr.CostUSD += reviewSR.CostUSD
			sr.AIDurationMs += reviewSR.AIDurationMs
			rec := extractRecommendation(reviewSR.FullOutput)

			// Auto-fix "Accept with notes" — feed notes back to claude without user input
			if isAcceptWithNotes(rec) && autoFixCount < maxAutoFixIterations {
				autoFixCount++
				fmt.Printf("[%s] Review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
					itemID, autoFixCount, maxAutoFixIterations)
				notes := extractNotesFromReview(reviewSR.FullOutput)
				s, _ = store.New(cfg)
				item, _ = s.Get(itemID)
				runAutoFixFromNotes(s, cfg, itemID, "", item, "plan review", notes, opts, engine, worktreeDir, "", &sr)
				continue // re-run review
			}

			gateMu.Lock()
			choice := showReviewGate(ReviewGateInfo{
				ItemID:         itemID,
				Title:          item.Title,
				GateType:       "Plan Review",
				Iteration:      iteration,
				Recommendation: rec,
				ReviewDuration: reviewDur,
				AcsTotal:       len(item.AcceptanceCriteria),
			}, []menuOption{
				{"1", "Accept      — approve and proceed"},
				{"2", "Reject      — stop and release"},
				{"3", "Feedback    — type direction, claude revises (constrained)"},
				{"4", "Interactive — full claude session (escape hatch)"},
			}, engine)
			gateMu.Unlock()

			if choice == "^C" {
				sr.Error = "interrupted by Ctrl+C"
				return sr
			}
			if choice == "1" {
				break // approved
			}
			if choice == "2" {
				sr.Error = "plan proposal rejected"
				return sr
			}

			if choice == "3" {
				// Constrained feedback: user types direction, claude acts under pipeline rules
				runConstrainedFeedback(s, cfg, itemID, "", item, "plan review", opts, engine, worktreeDir, "", &sr)
			} else {
				// Option 4: interactive escape hatch
				runInteractiveEscapeHatch(itemID, worktreeDir, engine, cfg)
			}

			// Reload item after revision
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

		// Design review loop — claude reviews, user decides
		autoFixCount := 0
		for iteration := 1; ; iteration++ {
			s, _ = store.New(cfg)
			item, _ = s.Get(itemID)

			reviewPrompt := buildPlanReviewPrompt(itemID, item)
			reviewStep := config.RunStepDef{Type: "claude", Prompt: reviewPrompt}
			reviewStep.SetName("design_review")
			reviewStart := time.Now()
			reviewSR := executeClaude(s, cfg, itemID, "", reviewStep, opts, engine, worktreeDir, "", false)
			reviewDur := time.Since(reviewStart)
			sr.CostUSD += reviewSR.CostUSD
			sr.AIDurationMs += reviewSR.AIDurationMs
			rec := extractRecommendation(reviewSR.FullOutput)

			// Auto-fix "Accept with notes" — feed notes back to claude without user input
			if isAcceptWithNotes(rec) && autoFixCount < maxAutoFixIterations {
				autoFixCount++
				fmt.Printf("[%s] Review returned 'Accept with notes' — auto-fixing (attempt %d/%d)\n",
					itemID, autoFixCount, maxAutoFixIterations)
				notes := extractNotesFromReview(reviewSR.FullOutput)
				s, _ = store.New(cfg)
				item, _ = s.Get(itemID)
				runAutoFixFromNotes(s, cfg, itemID, "", item, "design review", notes, opts, engine, worktreeDir, "", &sr)
				continue // re-run review
			}

			gateMu.Lock()
			choice := showReviewGate(ReviewGateInfo{
				ItemID:         itemID,
				Title:          item.Title,
				GateType:       "Design Review",
				Iteration:      iteration,
				Recommendation: rec,
				ReviewDuration: reviewDur,
				AcsTotal:       len(item.AcceptanceCriteria),
			}, []menuOption{
				{"1", "Approve     — accept and proceed"},
				{"2", "Reject      — stop and release"},
				{"3", "Feedback    — type direction, claude revises (constrained)"},
				{"4", "Interactive — full claude session (escape hatch)"},
			}, engine)
			gateMu.Unlock()

			if choice == "^C" {
				sr.Error = "interrupted by Ctrl+C"
				return sr
			}
			if choice == "1" {
				break // approved
			}
			if choice == "2" {
				sr.Error = "design not approved"
				return sr
			}

			if choice == "3" {
				runConstrainedFeedback(s, cfg, itemID, "", item, "design review", opts, engine, worktreeDir, "", &sr)
			} else {
				runInteractiveEscapeHatch(itemID, worktreeDir, engine, cfg)
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

	// Validate AC shell syntax — catch quoting errors before they waste pipeline time
	syntaxErrors := ValidateACsyntax(item.AcceptanceCriteria)
	if len(syntaxErrors) > 0 {
		fmt.Printf("\n⚠ %d AC(s) have shell syntax errors:\n", len(syntaxErrors))
		for _, e := range syntaxErrors {
			fmt.Printf("  %s\n", e)
		}
		fmt.Println("  These will fail at UAT. Fix them before proceeding.")
		fmt.Println()
	}

	item.PlanApproved = true
	item.Doc.SetField("plan_approved", "true")
	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
	s3.Write(item)

	// Create plan sidecar if one doesn't exist (st prep creates them, st run should too)
	if !plan.Exists(cfg.PlansDir(), itemID) {
		p := &plan.Plan{
			Approved:   true,
			ApprovedAt: plan.Now(),
			Approach:   item.Summary,
			ACs:        item.AcceptanceCriteria,
			ScopeRepos: inferReposFromItem(cfg, item),
			Revisions: []plan.Revision{
				{Timestamp: plan.Now(), Summary: "Plan approved via st run"},
			},
		}
		plan.Save(cfg.PlansDir(), itemID, p)

		// Set scope_repos on item if not already set
		if len(p.ScopeRepos) > 0 {
			item.Doc.SetField("scope_repos", strings.Join(p.ScopeRepos, ", "))
			s3.Write(item)
		}
	}

	sr.Passed = true
	return sr
}

// proposePlan launches claude -p to analyze the item and propose summary + ACs.
// proposePlanResult holds the text output and cost data from a proposePlan call.
type proposePlanResult struct {
	Text         string
	CostUSD      float64
	AIDurationMs int64
}

func proposePlan(cfg *config.Config, itemID string, item *model.Item, engine RunEngine, opts RunOpts, worktreeDir string, needsSummary, needsACs bool) (proposePlanResult, error) {
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
		b.WriteString(fmt.Sprintf("   cat <<'EOF' | st update %s summary --stdin\n", itemID))
		b.WriteString("   Your summary text here. Can be multiline.\n")
		b.WriteString("   EOF\n")
	}
	if needsACs {
		b.WriteString(fmt.Sprintf("3. Write specific, testable acceptance criteria and set them by running:\n"))
		b.WriteString(fmt.Sprintf("   cat <<'EOF' | st update %s acceptance_criteria --stdin\n", itemID))
		b.WriteString("   - cmd: first test command\n")
		b.WriteString("   - cmd: second test command\n")
		b.WriteString("   EOF\n")
		b.WriteString("   IMPORTANT: The heredoc MUST contain ONLY '- cmd:' lines. No prose or markdown.\n")
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
	b.WriteString("For test suite execution, use `st test <id> <suite> --run` — NEVER use raw `make e2e` or `make test` in ACs.\n")
	b.WriteString("ACs should be fast to verify — use targeted test runs, not full suite runs.\n")
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
		return proposePlanResult{}, fmt.Errorf("claude exec error: %v", err)
	}

	// Parse JSON to extract the text result
	claudeResult, parseErr := parseClaudeOutput(output)
	if parseErr != nil {
		if exitCode != 0 {
			return proposePlanResult{}, fmt.Errorf("claude exited %d", exitCode)
		}
		return proposePlanResult{Text: string(output)}, nil
	}

	if exitCode != 0 || (claudeResult.Subtype != "" && claudeResult.Subtype != "success") {
		return proposePlanResult{}, fmt.Errorf("claude exited %d (subtype: %s)", exitCode, claudeResult.Subtype)
	}

	return proposePlanResult{
		Text:         claudeResult.Result,
		CostUSD:      claudeResult.TotalCostUSD,
		AIDurationMs: claudeResult.DurationMs,
	}, nil
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

	// PR review bot findings (Cursor Bugbot, etc.)
	if m != nil && len(m.PRs) > 0 {
		for _, pr := range m.PRs {
			findings := fetchPRBotFindings(pr.Repo, pr.PRNumber, worktreeDir, cfg)
			if findings != "" {
				if len(m.PRs) > 1 {
					b.WriteString(fmt.Sprintf("\n### Review Bot Findings (%s#%d)\n", pr.Repo, pr.PRNumber))
				} else {
					b.WriteString("\n### Review Bot Findings\n")
				}
				b.WriteString(findings)
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

// fetchPRBotFindings fetches check run output, review comments, and inline file
// comments from automated review bots (like Cursor Bugbot) on a PR.
// Returns a formatted string with findings, or "" if no bot findings exist.
func fetchPRBotFindings(repoShort string, prNumber int, worktreeDir string, cfg *config.Config) string {
	// Resolve the full GitHub repo (owner/repo) from the short name
	ghRepo := resolveGHRepoFromShortName(repoShort, worktreeDir, cfg)
	if ghRepo == "" {
		return ""
	}

	var findings strings.Builder

	// 1. Fetch check run outputs (Bugbot posts its summary status here)
	sha := getPRHeadSHA(ghRepo, prNumber)
	if sha != "" {
		runsCmd := fmt.Sprintf(`gh api "repos/%s/commits/%s/check-runs" --jq '.check_runs[] | select(.output.summary != null and .output.summary != "") | "\(.name)\t\(.conclusion)\t\(.output.summary)"' 2>/dev/null`, ghRepo, sha)
		runsOut, rc, _ := runCmdInDir("", runsCmd)
		if rc == 0 && len(runsOut) > 0 {
			for _, line := range strings.Split(strings.TrimSpace(string(runsOut)), "\n") {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, "\t", 3)
				if len(parts) < 3 {
					continue
				}
				name, conclusion, summary := parts[0], parts[1], parts[2]
				if isReviewBot(name) {
					findings.WriteString(fmt.Sprintf("**%s** (%s):\n%s\n\n", name, conclusion, summary))
				}
			}
		}
	}

	// 2. Fetch inline file comments from bot accounts (the actionable findings)
	// These are the specific file:line issues Bugbot identifies.
	inlineCmd := fmt.Sprintf(`gh api "repos/%s/pulls/%d/comments" --jq '[.[] | select(.user.login | test("cursor|bugbot"; "i")) | {path, line: (.line // .original_line), body}]' 2>/dev/null`, ghRepo, prNumber)
	inlineOut, rc, _ := runCmdInDir("", inlineCmd)
	if rc == 0 && len(inlineOut) > 0 {
		var comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(inlineOut, &comments); err == nil && len(comments) > 0 {
			findings.WriteString(fmt.Sprintf("**Bugbot Inline Issues (%d):**\n", len(comments)))
			for i, c := range comments {
				// Strip HTML/markdown noise, keep the actionable content
				body := stripBugbotMarkup(c.Body)
				if len(body) > 500 {
					body = body[:500] + "..."
				}
				findings.WriteString(fmt.Sprintf("%d. `%s:%d` — %s\n", i+1, c.Path, c.Line, body))
			}
			findings.WriteString("\n")
		}
	}

	// 3. Fetch PR-level review comments from bot accounts (summary review)
	reviewCmd := fmt.Sprintf(`gh api "repos/%s/pulls/%d/reviews" --jq '.[] | select(.user.login | test("cursor|bugbot"; "i")) | "\(.state)\t\(.body)"' 2>/dev/null`, ghRepo, prNumber)
	reviewOut, rc, _ := runCmdInDir("", reviewCmd)
	if rc == 0 && len(reviewOut) > 0 {
		trimmed := strings.TrimSpace(string(reviewOut))
		if trimmed != "" {
			// Only include if there's meaningful content (not just HTML markers)
			for _, line := range strings.Split(trimmed, "\n") {
				parts := strings.SplitN(line, "\t", 2)
				if len(parts) == 2 {
					body := stripBugbotMarkup(parts[1])
					if body != "" {
						findings.WriteString(fmt.Sprintf("**Bot Review** (%s): %s\n", parts[0], body))
					}
				}
			}
		}
	}

	return findings.String()
}

// resolveGHRepoFromShortName resolves a short repo name (e.g., "theraprac-api")
// to a full GitHub repo path (e.g., "TheraPrac/theraprac-api").
func resolveGHRepoFromShortName(repoShort string, worktreeDir string, cfg *config.Config) string {
	if worktreeDir != "" {
		// Try the specific repo worktree
		if cfg.Worktree != nil && cfg.Worktree.Enabled && cfg.Worktree.BaseDir != "" {
			repoDir := filepath.Join(worktreeDir, "..", repoShort)
			if fi, err := os.Stat(repoDir); err == nil && fi.IsDir() {
				if r := resolveGHRepo(repoDir); r != "" {
					return r
				}
			}
		}
	}
	// Try parent dir + repo name (common worktree layout)
	parentDir := cfg.Root()
	if cfg.Worktree != nil && cfg.Worktree.ParentDir != "" {
		parentDir = filepath.Join(cfg.Root(), cfg.Worktree.ParentDir)
	}
	repoDir := filepath.Join(parentDir, repoShort)
	if fi, err := os.Stat(repoDir); err == nil && fi.IsDir() {
		if r := resolveGHRepo(repoDir); r != "" {
			return r
		}
	}
	return ""
}

// detectPRNumber gets the PR number for the current branch from GitHub.
func detectPRNumber(worktreeDir, ghRepo string) int {
	cmd := fmt.Sprintf(`gh pr view --json number --jq .number --repo %s 2>/dev/null`, ghRepo)
	out, exitCode, _ := runCmdInDir(worktreeDir, cmd)
	if exitCode != 0 {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// countBugbotFindings returns the number of unresolved Bugbot findings on a PR.
// Uses the GraphQL reviewThreads API to check isResolved/isOutdated status —
// only threads that are neither resolved nor outdated count as open.
func countBugbotFindings(ghRepo string, prNumber int, worktreeDir string) int {
	parts := strings.SplitN(ghRepo, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	owner, repo := parts[0], parts[1]

	query := fmt.Sprintf(`query {
  repository(owner: "%s", name: "%s") {
    pullRequest(number: %d) {
      reviewThreads(first: 50) {
        nodes {
          isResolved
          isOutdated
          comments(first: 1) {
            nodes { author { login } }
          }
        }
      }
    }
  }
}`, owner, repo, prNumber)

	cmd := fmt.Sprintf(`gh api graphql -f query='%s' --jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved == false and .isOutdated == false and (.comments.nodes[0].author.login | test("cursor|bugbot"; "i")))] | length' 2>/dev/null`, query)
	out, exitCode, _ := runCmdInDir(worktreeDir, cmd)
	if exitCode != 0 {
		// Fallback: count all Bugbot comments (conservative)
		fallback := fmt.Sprintf(`gh api "repos/%s/pulls/%d/comments" --jq '[.[] | select(.user.login | test("cursor|bugbot"; "i"))] | length' 2>/dev/null`, ghRepo, prNumber)
		out, exitCode, _ = runCmdInDir(worktreeDir, fallback)
		if exitCode != 0 {
			return 0
		}
	}
	var count int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count)
	return count
}

// getPRHeadSHA fetches the head commit SHA for a PR.
func getPRHeadSHA(ghRepo string, prNumber int) string {
	cmd := fmt.Sprintf(`gh api "repos/%s/pulls/%d" --jq .head.sha 2>/dev/null`, ghRepo, prNumber)
	out, exitCode, _ := runCmdInDir("", cmd)
	if exitCode == 0 {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// stripBugbotMarkup removes HTML comments, Bugbot-specific markers, and link markup
// from Bugbot review comments, keeping only the human-readable content.
func stripBugbotMarkup(s string) string {
	// Remove HTML comments (<!-- ... -->)
	for {
		start := strings.Index(s, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "-->")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+end+3:]
	}
	// Remove <a href=...>...</a> tags but keep link text
	for {
		start := strings.Index(s, "<a ")
		if start < 0 {
			break
		}
		tagEnd := strings.Index(s[start:], ">")
		if tagEnd < 0 {
			break
		}
		closeTag := strings.Index(s[start:], "</a>")
		if closeTag < 0 {
			break
		}
		linkText := s[start+tagEnd+1 : start+closeTag]
		s = s[:start] + linkText + s[start+closeTag+4:]
	}
	// Remove <sub>...</sub> tags
	for {
		start := strings.Index(s, "<sub>")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "</sub>")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+end+6:]
	}
	// Clean up resulting whitespace
	lines := strings.Split(s, "\n")
	var clean []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			clean = append(clean, trimmed)
		}
	}
	return strings.Join(clean, " ")
}

// isReviewBot returns true if the check run name matches a known automated review bot.
func isReviewBot(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "bugbot") ||
		strings.Contains(lower, "cursor") ||
		strings.Contains(lower, "coderabbit") ||
		strings.Contains(lower, "codeclimate") ||
		strings.Contains(lower, "sonar")
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
// hasNoBranchCommits returns true if the item's branch across all configured
// repo worktrees has zero commits relative to main. Used to detect operational
// / housekeeping items that made no code changes and shouldn't run through
// PR/test/deploy steps.
func hasNoBranchCommits(cfg *config.Config, itemID string) bool {
	dirs := allWorktreeDirs(cfg, itemID)
	if len(dirs) == 0 {
		return false
	}
	for _, dir := range dirs {
		cmd := exec.Command("git", "log", "main..HEAD", "--oneline")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			// Can't determine — assume it has commits (safer default)
			return false
		}
		if strings.TrimSpace(string(out)) != "" {
			return false
		}
	}
	return true
}

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

	// Release item lock so GitPull stops protecting it
	store.UnlockItem(cfg, itemID)
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

	// Read stream-json events, echo text, capture final result.
	// When we see a "result" event, claude is done producing output.
	// We break out of the scanner loop immediately rather than waiting for
	// the pipe to close (which may take seconds while claude finalizes).
	var lastResult []byte
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	gotResult := false
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
			gotResult = true
		}
		// Once we have the result, stop reading — claude is done.
		// Don't wait for the pipe to close (claude may linger for cache/cleanup).
		if gotResult {
			break
		}
	}
	activity.stop()

	// Wait for process to exit, but with a short timeout if we already got the result.
	// Claude may linger after emitting the result (cache writes, cleanup). Don't block.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	waitTimeout := maxWallTimeout
	if gotResult {
		waitTimeout = 10 * time.Second // result received — process should exit quickly
	}

	var waitErr error
	select {
	case waitErr = <-waitDone:
		// Process exited normally
	case <-time.After(waitTimeout):
		// Process didn't exit in time — kill it
		cancel()
		waitErr = <-waitDone // collect after kill
		if gotResult {
			// We have the result, the linger is harmless
			waitErr = nil
		}
	}

	exitCode := 0
	if waitErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			idle := time.Since(activity.lastSeen)
			if idle >= activity.idleTimeout {
				return lastResult, 1, fmt.Errorf("killed: no output for %s (idle timeout)", idle.Round(time.Second))
			}
			return lastResult, 1, fmt.Errorf("killed: wall time limit (%s)", maxWallTimeout)
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			waitErr = nil
		}
	}

	if len(lastResult) > 0 {
		return lastResult, exitCode, waitErr
	}
	return nil, exitCode, waitErr
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

// ReviewGateInfo holds context for rendering a review gate box.
type ReviewGateInfo struct {
	ItemID        string
	Title         string
	GateType      string // "Plan Review", "Design Review", "UAT Review"
	Iteration     int
	Recommendation string // one-line recommendation from claude's review
	ReviewDuration time.Duration
	AcsPassed     int
	AcsTotal      int
}

// showReviewGate renders a boxed gate with context and returns the user's menu choice.
func showReviewGate(info ReviewGateInfo, options []menuOption, engine RunEngine) string {
	content := []string{
		fmt.Sprintf("%s: %s", info.GateType, info.ItemID),
	}
	if info.Title != "" {
		title := info.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		content = append(content, fmt.Sprintf("  %s", title))
	}
	content = append(content, "")

	// Info section
	var infoLine string
	if info.ReviewDuration > 0 {
		infoLine = fmt.Sprintf("Review: %s", formatDuration(info.ReviewDuration))
	}
	if info.AcsTotal > 0 {
		acStr := fmt.Sprintf("ACs: %d/%d pass", info.AcsPassed, info.AcsTotal)
		if infoLine != "" {
			infoLine += "  |  " + acStr
		} else {
			infoLine = acStr
		}
	}
	if infoLine != "" {
		content = append(content, infoLine)
	}

	if info.Recommendation != "" {
		content = append(content, "")
		// Wrap long recommendations
		rec := info.Recommendation
		if len(rec) > 65 {
			// Simple word-wrap at 65 chars
			words := strings.Fields(rec)
			var lines []string
			current := ">>> "
			for _, word := range words {
				if len(current)+len(word)+1 > 65 && len(current) > 4 {
					lines = append(lines, current)
					current = "    " + word
				} else {
					if len(current) > 4 {
						current += " "
					}
					current += word
				}
			}
			if current != "" {
				lines = append(lines, current)
			}
			content = append(content, lines...)
		} else {
			content = append(content, ">>> "+rec)
		}
	}

	content = append(content, "")
	for _, opt := range options {
		content = append(content, fmt.Sprintf("[%s] %s", opt.Key, opt.Label))
	}

	// Find widest line
	w := 0
	for _, l := range content {
		if len(l) > w {
			w = len(l)
		}
	}
	if w < 50 {
		w = 50
	}

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

	// Auto-accept after timeout when recommendation is positive.
	lower := strings.ToLower(info.Recommendation)
	if strings.Contains(lower, "accept") || strings.Contains(lower, "approve") {
		return engineSelectMenuTimed(engine, "", options, 0, autoAcceptTimeout)
	}
	return engineSelectMenu(engine, "", options, 0)
}

// buildPlanReviewPrompt creates a prompt for claude to critically review a plan.
func buildPlanReviewPrompt(itemID string, item *model.Item) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("You are reviewing the implementation plan for item %s.\n\n", itemID))
	b.WriteString(fmt.Sprintf("Title: %s\n", item.Title))
	if item.Summary != "" {
		b.WriteString(fmt.Sprintf("\nSummary:\n%s\n", item.Summary))
	}
	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("\nAcceptance Criteria:\n")
		for i, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, ac))
		}
	}
	b.WriteString("\nReview the implementation plan and FIX any gaps you find before reporting.\n\n")
	b.WriteString("## Instructions\n\n")
	b.WriteString("1. Evaluate the plan across these dimensions:\n")
	b.WriteString("   - SCOPE — is it appropriate? Too broad, too narrow, or about right?\n")
	b.WriteString("   - APPROACH — does the technical approach make sense? Any risks or alternatives?\n")
	b.WriteString("   - ACCEPTANCE CRITERIA — are the ACs meaningful? Do they test the right things?\n")
	b.WriteString("     Flag any that are trivial (just grep existence), overly broad, or missing.\n")
	b.WriteString("   - GAPS — anything missing? Edge cases, error handling, tests, docs?\n\n")
	b.WriteString("2. If you find actionable gaps (missing ACs, unclear approach, missing error handling\n")
	b.WriteString("   considerations, untested edge cases, etc.), FIX THEM NOW:\n")
	b.WriteString("   - Summary: `cat <<'EOF' | st update " + itemID + " summary --stdin`\n")
	b.WriteString("   - Acceptance criteria: `cat <<'EOF' | st update " + itemID + " acceptance_criteria --stdin`\n")
	b.WriteString("     The heredoc MUST contain ONLY '- cmd:' lines. No prose, no markdown, no commentary.\n")
	b.WriteString("   - Other fields: `st update " + itemID + " <field> <value>`\n")
	b.WriteString("   Do NOT just list problems for the user to fix — resolve them yourself.\n")
	b.WriteString("   Do NOT use `st edit` (requires interactive $EDITOR). Always use `--stdin`.\n\n")
	b.WriteString("3. Then produce a concise report for the user:\n")
	b.WriteString("   - SCOPE — assessment (1 line)\n")
	b.WriteString("   - APPROACH — assessment (1-2 lines)\n")
	b.WriteString("   - CHANGES MADE — list what you fixed (if anything)\n")
	b.WriteString("   - REMAINING CONCERNS — only issues you could NOT fix (e.g., design decisions\n")
	b.WriteString("     that require user input, architectural trade-offs with no clear winner)\n")
	b.WriteString("   - RECOMMENDATION — MUST be exactly one of these three:\n")
	b.WriteString("     a) \"Accept\" — plan is ready, no issues remain. If you have informational\n")
	b.WriteString("        notes (operational steps, coordination needs, caveats), include them in\n")
	b.WriteString("        the summary field via `st update` — do NOT downgrade to a weaker recommendation.\n")
	b.WriteString("        If it's fixable, fix it. Then recommend Accept.\n")
	b.WriteString("     b) \"Reject\" — plan is fundamentally flawed, needs complete rethink\n")
	b.WriteString("     c) \"Feedback\" — plan has design problems that ONLY the user can resolve\n")
	b.WriteString("        (architectural trade-offs, business decisions, ambiguous requirements).\n")
	b.WriteString("        Use Feedback ONLY when you genuinely cannot proceed without human input.\n")
	b.WriteString("     State which one and why in one sentence.\n\n")
	b.WriteString("IMPORTANT: Do NOT use \"Accept with notes\". If you have notes, either:\n")
	b.WriteString("  - Fix the issue yourself (update summary/ACs) → recommend Accept\n")
	b.WriteString("  - Or if it truly requires user input → recommend Feedback\n")
	b.WriteString("There is no middle ground. Fix it or escalate it.\n\n")
	b.WriteString("The goal: the user should be able to accept the plan without a follow-up revision session.\n")
	b.WriteString("Be critical but constructive — flag real issues, not style preferences.\n\n")
	b.WriteString("AC QUALITY RULES — flag and fix violations:\n")
	b.WriteString("- ACs should use `st test <id> <suite> --run` for test suite execution, NOT raw `make` commands\n")
	b.WriteString("- ACs should be fast to verify — avoid full E2E suite runs when a targeted spec suffices\n")
	b.WriteString("- ACs should use relative paths from the worktree base (cd ../theraprac-web, NOT cd /absolute/path)\n")
	return b.String()
}

// planRecommendation evaluates a plan/design and returns a recommendation string.
// extractRecommendation pulls the recommendation from claude's review output
// and returns it as a one-line string. Looks for Accept/Reject/Chat keywords.
func extractRecommendation(output string) string {
	if output == "" {
		return ""
	}

	lines := strings.Split(output, "\n")
	var recLine string

	for i, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "recommendation") {
			continue
		}

		// Try to extract from same line after ":" or "—".
		// ":" first because "RECOMMENDATION: Accept — reason" needs the colon
		// to capture "Accept", not the em dash which loses it.
		for _, sep := range []string{":", "—"} {
			if idx := strings.LastIndex(line, sep); idx >= 0 {
				rest := strings.TrimSpace(line[idx+len(sep):])
				rest = strings.ReplaceAll(rest, "**", "")
				rest = strings.ReplaceAll(rest, "*", "")
				if rest != "" {
					recLine = rest
					break
				}
			}
		}

		// If same line was just a header, grab the next non-empty line
		if recLine == "" && i+1 < len(lines) {
			for j := i + 1; j < len(lines) && j <= i+3; j++ {
				next := strings.TrimSpace(lines[j])
				next = strings.ReplaceAll(next, "**", "")
				next = strings.ReplaceAll(next, "*", "")
				if next != "" && !strings.HasPrefix(next, "#") {
					recLine = next
					break
				}
			}
		}

		if recLine != "" {
			break
		}
	}

	if recLine == "" {
		return ""
	}

	// Map to menu option.
	// Check "accept with notes" before plain "accept" since both contain "accept".
	lower := strings.ToLower(recLine)
	if strings.Contains(lower, "accept with notes") || strings.Contains(lower, "accept with note") {
		return "[1] Accept (with notes) — " + recLine
	}
	if strings.Contains(lower, "accept") || strings.Contains(lower, "approve") {
		return "[1] Accept — " + recLine
	}
	if strings.Contains(lower, "reject") {
		return "[2] Reject — " + recLine
	}
	if strings.Contains(lower, "chat") || strings.Contains(lower, "feedback") {
		return "[3] Feedback — " + recLine
	}
	return recLine
}

// buildFeedbackPrompt creates a constrained prompt for claude to act on user feedback
// during a review gate. The prompt includes the item context and the user's feedback,
// and instructs claude to make changes and report what was done.
func buildFeedbackPrompt(itemID string, item *model.Item, gateType, userFeedback string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("You are revising item %s based on user feedback during %s.\n\n", itemID, gateType))
	b.WriteString(fmt.Sprintf("Title: %s\n", item.Title))
	if item.Summary != "" {
		b.WriteString(fmt.Sprintf("\nCurrent Summary:\n%s\n", item.Summary))
	}
	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("\nCurrent Acceptance Criteria:\n")
		for i, ac := range item.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, ac))
		}
	}
	b.WriteString(fmt.Sprintf("\n## User Feedback\n\n%s\n\n", userFeedback))
	b.WriteString("## Instructions\n\n")
	b.WriteString("Act on the user's feedback. Make the requested changes using st commands:\n")
	b.WriteString("- Summary: `cat <<'EOF' | st update " + itemID + " summary --stdin`\n")
	b.WriteString("- Acceptance criteria: `cat <<'EOF' | st update " + itemID + " acceptance_criteria --stdin`\n")
	b.WriteString("  The heredoc MUST contain ONLY '- cmd:' lines. No prose, no markdown, no commentary.\n")
	b.WriteString("- Other fields: `st update " + itemID + " <field> <value>`\n")
	b.WriteString("Do NOT use `st edit` (requires interactive $EDITOR). Always use `--stdin`.\n\n")
	b.WriteString("After making changes, produce a brief summary of what you changed (2-3 lines max).\n")
	b.WriteString("Do NOT produce a full review report — the review will re-run automatically after your changes.\n")
	return b.String()
}

// runConstrainedFeedback prompts the user for feedback, sends it to claude as a
// constrained subprocess (with full pipeline rules), and returns the step result.
// Returns true if feedback was given and processed, false if user cancelled.
func runConstrainedFeedback(s *store.Store, cfg *config.Config, itemID, sprintID string, item *model.Item,
	gateType string, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, sr *StepResult) bool {

	// Prompt user for feedback
	prompt := "What changes? (empty to cancel): "
	var feedback string
	var err error
	if engine.PromptUser != nil {
		feedback, err = engine.PromptUser(prompt)
	} else {
		fmt.Print(prompt)
		reader := bufio.NewReader(os.Stdin)
		feedback, err = reader.ReadString('\n')
	}
	if err != nil || strings.TrimSpace(feedback) == "" {
		return false
	}
	feedback = strings.TrimSpace(feedback)

	fmt.Printf("\n[%s] Applying feedback...\n", itemID)

	feedbackPrompt := buildFeedbackPrompt(itemID, item, gateType, feedback)
	feedbackStep := config.RunStepDef{Type: "claude", Prompt: feedbackPrompt}
	feedbackStep.SetName("feedback")
	feedbackSR := executeClaude(s, cfg, itemID, sprintID, feedbackStep, opts, engine, worktreeDir, claudeSessionID, true)
	sr.CostUSD += feedbackSR.CostUSD
	sr.AIDurationMs += feedbackSR.AIDurationMs

	return true
}

// maxAutoFixIterations is how many times the system will auto-fix "Accept with notes"
// before falling through to the user gate. Prevents infinite loops.
const maxAutoFixIterations = 3

// extractNotesFromReview pulls the REMAINING CONCERNS and notes sections from a
// review output to use as auto-feedback. Returns empty string if nothing found.
func extractNotesFromReview(output string) string {
	if output == "" {
		return ""
	}

	lines := strings.Split(output, "\n")
	var notes []string
	capturing := false

	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))

		// Start capturing at REMAINING CONCERNS, notes sections, or recommendation with notes
		if strings.Contains(lower, "remaining concerns") ||
			strings.Contains(lower, "accept with notes") ||
			(strings.Contains(lower, "recommendation") && strings.Contains(lower, "notes")) {
			capturing = true
			continue
		}

		// Stop capturing at the next major section header
		if capturing && (strings.HasPrefix(strings.TrimSpace(line), "##") ||
			(strings.Contains(lower, "recommendation") && !strings.Contains(lower, "notes"))) {
			break
		}

		if capturing {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				notes = append(notes, trimmed)
			}
		}
	}

	return strings.Join(notes, "\n")
}

// runAutoFixFromNotes sends the review notes as auto-feedback to claude so it can
// fix them without user intervention. Returns true if feedback was processed.
func runAutoFixFromNotes(s *store.Store, cfg *config.Config, itemID, sprintID string, item *model.Item,
	gateType string, notes string, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, sr *StepResult) bool {

	if strings.TrimSpace(notes) == "" {
		return false
	}

	fmt.Printf("\n[%s] Auto-fixing review notes (no user input needed)...\n", itemID)

	autoFeedback := "The reviewer flagged the following notes/concerns. Fix all of them — " +
		"update the summary and/or acceptance criteria as needed. Do not leave informational " +
		"notes for the user; resolve everything you can.\n\n" + notes

	feedbackPrompt := buildFeedbackPrompt(itemID, item, gateType, autoFeedback)
	feedbackStep := config.RunStepDef{Type: "claude", Prompt: feedbackPrompt}
	feedbackStep.SetName("auto_fix")
	feedbackSR := executeClaude(s, cfg, itemID, sprintID, feedbackStep, opts, engine, worktreeDir, claudeSessionID, true)
	sr.CostUSD += feedbackSR.CostUSD
	sr.AIDurationMs += feedbackSR.AIDurationMs

	return true
}

// isAcceptWithNotes returns true if the extracted recommendation is "Accept with notes".
func isAcceptWithNotes(rec string) bool {
	lower := strings.ToLower(rec)
	return strings.Contains(lower, "accept") && strings.Contains(lower, "notes")
}

// runInteractiveEscapeHatch launches an ungoverned interactive claude session.
// This is the escape hatch for when the user needs full control.
func runInteractiveEscapeHatch(itemID, worktreeDir string, engine RunEngine, cfg *config.Config) {
	fmt.Printf("\n[%s] Launching interactive session (escape hatch)...\n", itemID)
	fmt.Println("  Full interactive claude — no pipeline constraints.")
	fmt.Println("  When done, exit (Ctrl+D or /exit). Review will re-run.")
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
			return
		}
		cmd := exec.Command(claudeBin)
		cmd.Dir = cwd
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}
}

func planRecommendation(item *model.Item) string {
	var issues []string

	if item.Summary == "" {
		issues = append(issues, "missing summary")
	}
	if len(item.AcceptanceCriteria) == 0 {
		issues = append(issues, "no acceptance criteria")
	} else {
		// Check for non-cmd ACs
		nonCmd := 0
		for _, ac := range item.AcceptanceCriteria {
			trimmed := strings.TrimSpace(strings.TrimPrefix(ac, "- "))
			if !strings.HasPrefix(trimmed, "cmd:") {
				nonCmd++
			}
		}
		if nonCmd > 0 {
			issues = append(issues, fmt.Sprintf("%d AC(s) not cmd: prefixed — will fail validation", nonCmd))
		}
	}

	if len(issues) > 0 {
		return fmt.Sprintf("Issues: %s — consider chatting to fix", strings.Join(issues, ", "))
	}

	acCount := len(item.AcceptanceCriteria)
	return fmt.Sprintf("Plan looks complete — %s, %d AC(s), all cmd: prefixed",
		summarySizeLabel(item.Summary), acCount)
}

func summarySizeLabel(summary string) string {
	words := len(strings.Fields(summary))
	if words < 20 {
		return "brief summary"
	}
	if words < 80 {
		return "good summary"
	}
	return "detailed summary"
}

// inferReposFromItem guesses scope repos from item fields (repo, ACs, summary).
func inferReposFromItem(cfg *config.Config, item *model.Item) []string {
	if cfg.Worktree == nil {
		return nil
	}
	repoSet := make(map[string]bool)

	// Check item.Repo field
	if item.Repo != "" {
		for _, r := range cfg.Worktree.Repos {
			if strings.Contains(r, item.Repo) {
				repoSet[r] = true
			}
		}
	}

	// Check ACs and summary for repo references
	allText := item.Summary
	for _, ac := range item.AcceptanceCriteria {
		allText += " " + ac
	}
	for _, repo := range cfg.Worktree.Repos {
		if strings.Contains(allText, repo) {
			repoSet[repo] = true
		}
	}

	var repos []string
	for r := range repoSet {
		repos = append(repos, r)
	}
	return repos
}

// rewriteACPaths rewrites ../repo-name paths in acceptance criteria commands
// to use the worktree path. From the worktree base (worktrees/T-095/),
// repos are direct subdirectories (theraprac-web/), not siblings (../theraprac-web).
func rewriteACPaths(cfg *config.Config, itemID, uatDir, cmd string) string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return cmd
	}

	// Check if the worktree base exists for this item
	wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
	if _, err := os.Stat(wtBase); err != nil {
		return cmd
	}

	// Rewrite ../repo-name → repo-name (direct subdirectory of worktree base)
	for _, repo := range cfg.Worktree.Repos {
		for _, pattern := range []string{
			"cd ../" + repo + " ",
			"cd ../" + repo + "/",
			"../" + repo + "/",
		} {
			if strings.Contains(cmd, pattern) {
				replacement := strings.Replace(pattern, "../"+repo, repo, 1)
				cmd = strings.ReplaceAll(cmd, pattern, replacement)
			}
		}
		// Also handle "cd ../repo &&" (no trailing space before &&)
		pattern := "cd ../" + repo + " &&"
		if strings.Contains(cmd, pattern) {
			cmd = strings.ReplaceAll(cmd, pattern, "cd "+repo+" &&")
		}
	}

	return cmd
}

func defaultPromptUser(_ string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return reader.ReadString('\n')
}

// autoAcceptTimeout is how long pause/review gates wait before auto-selecting
// the recommended option (accept, approve, or continue).
const autoAcceptTimeout = 120 * time.Second

// engineSelectMenu uses the engine override if set, otherwise the real terminal menu.
func engineSelectMenu(engine RunEngine, prompt string, options []menuOption, defaultIdx int) string {
	if engine.SelectMenu != nil {
		return engine.SelectMenu(prompt, options, defaultIdx)
	}
	return selectMenu(prompt, options, defaultIdx)
}

// engineSelectMenuTimed is like engineSelectMenu but with a timeout that
// auto-selects the default option. Used when the recommendation is positive.
func engineSelectMenuTimed(engine RunEngine, prompt string, options []menuOption, defaultIdx int, timeout time.Duration) string {
	if engine.SelectMenu != nil {
		return engine.SelectMenu(prompt, options, defaultIdx)
	}
	return selectMenuTimed(prompt, options, defaultIdx, timeout)
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

	sep := strings.Repeat("─", 136)

	fmt.Println()
	if epic != nil {
		fmt.Printf("  Epic: %s\n", epic.Title)
	}
	fmt.Printf("  %s\n", sep)
	fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %21s  %10s  %10s\n",
		"ITEM", "STATUS", "ST TIME", "AI TIME", "COST", "TOKENS (I/O/T)", "NET LOC", "SESSION $")
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
	var epicInTok, epicOutTok int
	var epicNetLOC int

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
		var sprintInTok, sprintOutTok int
		var sprintNetLOC int
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
			var itemInTok, itemOutTok int

			if isCurrent {
				for _, r := range results {
					if r.ItemID == itemID {
						sessCost = r.TotalCost
						if !r.Success {
							for _, sr := range r.Steps {
								if !sr.Passed {
									if sr.Step == "blocked" {
										status = "blocked"
									} else {
										status = "fail@" + sr.Step
									}
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
			if v, ok := getNestedField(item, "time_tracking", "input_tokens"); ok {
				fmt.Sscanf(v, "%d", &itemInTok)
			}
			if v, ok := getNestedField(item, "time_tracking", "output_tokens"); ok {
				fmt.Sscanf(v, "%d", &itemOutTok)
			}

			// Net LOC from PR manifest
			var itemNetLOC int
			if m, err := manifest.Load(cfg.ManifestDir(), itemID); err == nil {
				for _, pr := range m.PRs {
					itemNetLOC += pr.CodeStats.Insertions - pr.CodeStats.Deletions
				}
			}

			sprintWall += totalWall
			sprintAI += totalAI
			sprintCost += totalCostItem
			sprintInTok += itemInTok
			sprintOutTok += itemOutTok
			sprintNetLOC += itemNetLOC

			f := func(d time.Duration) string {
				if d > 0 { return formatDuration(d) }
				return "—"
			}
			fc := func(c float64) string {
				if c > 0 { return fmt.Sprintf("$%.2f", c) }
				return "—"
			}
			ft := func(in, out int) string {
				if in == 0 && out == 0 { return "—" }
				return fmt.Sprintf("%s/%s/%s", formatTokens(in), formatTokens(out), formatTokens(in+out))
			}
			fl := func(n int) string {
				if n == 0 { return "—" }
				return formatLOC(n)
			}

			fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %21s  %10s  %10s\n",
				itemID, truncate(status, 22),
				f(totalWall), f(totalAI), fc(totalCostItem), ft(itemInTok, itemOutTok), fl(itemNetLOC), fc(sessCost))
		}

		// Sprint subtotal
		sprintTokStr := "—"
		if sprintInTok > 0 || sprintOutTok > 0 {
			sprintTokStr = fmt.Sprintf("%s/%s/%s", formatTokens(sprintInTok), formatTokens(sprintOutTok), formatTokens(sprintInTok+sprintOutTok))
		}
		sprintLOCStr := "—"
		if sprintNetLOC != 0 {
			sprintLOCStr = formatLOC(sprintNetLOC)
		}
		fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %21s  %10s\n", "",
			fmt.Sprintf("%d/%d done", sprintDone, sprintTotal),
			formatDuration(sprintWall), formatDuration(sprintAI),
			fmt.Sprintf("$%.2f", sprintCost), sprintTokStr, sprintLOCStr)

		epicWall += sprintWall
		epicAI += sprintAI
		epicCost += sprintCost
		epicInTok += sprintInTok
		epicOutTok += sprintOutTok
		epicNetLOC += sprintNetLOC
	}

	// Epic total
	if epic != nil && len(sprintIDs) > 1 {
		epicTokStr := "—"
		if epicInTok > 0 || epicOutTok > 0 {
			epicTokStr = fmt.Sprintf("%s/%s/%s", formatTokens(epicInTok), formatTokens(epicOutTok), formatTokens(epicInTok+epicOutTok))
		}
		epicLOCStr := "—"
		if epicNetLOC != 0 {
			epicLOCStr = formatLOC(epicNetLOC)
		}
		fmt.Printf("\n  %s\n", sep)
		fmt.Printf("  %-8s %-22s  %12s  %12s  %10s  %21s  %10s\n",
			"TOTAL", truncate(epic.Title, 22),
			formatDuration(epicWall), formatDuration(epicAI),
			fmt.Sprintf("$%.2f", epicCost), epicTokStr, epicLOCStr)
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
