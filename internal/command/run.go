package command

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
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
}

// RunEngine holds injectable dependencies for run/advance.
type RunEngine struct {
	// RunClaude launches a claude -p subprocess and returns its output.
	RunClaude func(cwd string, args []string, env []string) ([]byte, int, error)
	// PromptUser reads a line from stdin (for gate steps).
	PromptUser func(prompt string) (string, error)
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
	AIDurationMs int64         `json:"ai_duration_ms,omitempty"` // from claude's reported duration_ms
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

	// Find sprints with remaining work
	type candidate struct {
		sprint   *registry.Sprint
		epic     string
		total    int
		complete int
	}
	var candidates []candidate

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
			epicTitle := sp.Epic
			for _, e := range reg.Epics {
				if e.ID == sp.Epic {
					epicTitle = e.Title
					break
				}
			}
			candidates = append(candidates, candidate{
				sprint: sp, epic: epicTitle, total: total, complete: complete,
			})
		}
	}

	if len(candidates) == 0 {
		fmt.Println("No active sprints with remaining work")
		return 0
	}

	// Display
	fmt.Println("Sprints with remaining work:")
	for i, c := range candidates {
		approved := ""
		if c.sprint.PlanApproved {
			approved = " [plan approved]"
		}
		fmt.Printf("  %d. %s — %s\n", i+1, c.sprint.ID, c.sprint.Title)
		fmt.Printf("     Epic: %s  Progress: %d/%d%s\n\n", c.epic, c.complete, c.total, approved)
	}

	// Prompt for selection
	fmt.Printf("Which sprint? [1-%d]: ", len(candidates))
	response, err := engine.PromptUser("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "prompt error: %v\n", err)
		return 1
	}
	choice := 0
	fmt.Sscanf(strings.TrimSpace(response), "%d", &choice)
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
		fmt.Printf("\nApprove this plan? [y/N]: ")
		resp, err := engine.PromptUser("")
		if err != nil {
			return 1
		}
		answer := strings.TrimSpace(strings.ToLower(resp))
		if answer != "y" && answer != "yes" {
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

	if !sp.PlanApproved {
		fmt.Fprintf(os.Stderr, "sprint %s plan not approved — use `st run` (no args) for interactive approval\n", sprintID)
		return 1
	}

	if opts.DryRun {
		return printDryRun(s, cfg, sp, groups, pipeline, opts)
	}

	// Execute groups sequentially, items within groups up to parallelism
	start := time.Now()
	var allResults []ItemResult

	for i, group := range groups {
		fmt.Printf("\n=== Group %d/%d ===\n", i+1, len(groups))
		results := runGroup(s, cfg, group, sprintID, pipeline, opts, engine)
		allResults = append(allResults, results...)
	}

	// Completion report
	printCompletionReport(allResults, sprintID, time.Since(start))

	for _, r := range allResults {
		if !r.Success {
			return 1
		}
	}
	return 0
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
	if maxPar > len(eligible) {
		maxPar = len(eligible)
	}

	results := make([]ItemResult, len(eligible))
	sem := make(chan struct{}, maxPar)
	var wg sync.WaitGroup

	for i, itemID := range eligible {
		wg.Add(1)
		sem <- struct{}{} // acquire
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

	// Resolve worktree directory
	worktreeDir := resolveWorktreeDir(cfg, itemID)

	// Execute each pipeline step
	for _, step := range pipeline {
		stepStart := time.Now()
		sr := executeStep(localStore, cfg, itemID, sprintID, step, opts, engine, worktreeDir)
		sr.Duration = time.Since(stepStart)
		result.Steps = append(result.Steps, sr)
		result.TotalCost += sr.CostUSD

		if !sr.Passed {
			fmt.Printf("[%s] Step %s FAILED: %s\n", itemID, step.Name(), sr.Error)
			result.Duration = time.Since(start)
			return result
		}

		fmt.Printf("[%s] Step %s OK (%s)\n", itemID, step.Name(), sr.Duration.Round(time.Second))

		// Reload store after each step (other steps may have modified the item)
		localStore, _ = store.New(cfg)

		// Stop at --step filter
		if opts.StepFilter != "" && step.Name() == opts.StepFilter {
			break
		}
	}

	result.Success = true
	result.Duration = time.Since(start)

	// Write time tracking + cost back to item
	recordRunMetrics(cfg, itemID, result)

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

// executeStep dispatches to the appropriate step executor.
func executeStep(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir string) StepResult {
	switch step.Type {
	case "plan":
		return executePlanWithOpts(s, cfg, itemID, engine, opts, worktreeDir)
	case "claude":
		return executeClaude(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir)
	case "test":
		return executeTest(s, cfg, itemID, step, worktreeDir)
	case "verify_tests":
		return executeVerifyTests(s, cfg, itemID)
	case "pr":
		return executePR(s, cfg, itemID, step, worktreeDir)
	case "merge":
		return executeMerge(s, cfg, itemID, worktreeDir)
	case "merge_precheck":
		return executeMergePrecheck(cfg, worktreeDir)
	case "deploy":
		return executeDeploy(s, cfg, itemID, worktreeDir)
	case "smoke":
		return executeSmoke(s, cfg, itemID, worktreeDir)
	case "uat":
		return executeUAT(s, cfg, itemID)
	case "gate":
		return executeGate(itemID, engine)
	case "close":
		return executeClose(s, cfg, itemID, step)
	case "command":
		return executeCommand(cfg, itemID, sprintID, step, worktreeDir)
	default:
		return StepResult{Step: step.Name(), Type: step.Type, Error: fmt.Sprintf("unknown step type: %s", step.Type)}
	}
}

// --- Step executors ---

func executeClaude(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "claude"}

	// Build prompt
	prompt := step.Prompt
	if prompt == "" {
		prompt = buildDefaultPrompt(s, cfg, itemID, sprintID)
	} else {
		prompt = expandTemplate(prompt, itemID, sprintID, worktreeDir, cfg)
	}

	// Build args
	args := buildClaudeArgs(cfg, prompt, opts, worktreeDir)

	// Build env with unique session ID
	sessionID := generateSessionID()
	env := []string{
		"AS_SESSION_ID=" + sessionID,
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
	repo := step.Command // command field carries the repo name
	if repo == "" {
		sr.Error = "pr step requires command field set to repo name"
		return sr
	}
	// Detect PR number from current branch
	out, exitCode, err := runCmdInDir(worktreeDir, "gh pr view --json number -q .number")
	if err != nil || exitCode != 0 || len(out) == 0 {
		sr.Error = "could not detect PR number (is there an open PR on this branch?)"
		return sr
	}
	prNum := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &prNum)
	if prNum == 0 {
		sr.Error = fmt.Sprintf("invalid PR number from gh pr view: %s", string(out))
		return sr
	}
	code := PR(s, cfg, itemID, PROpts{Repo: repo, PRNumber: prNum})
	if code != 0 {
		sr.Error = fmt.Sprintf("st pr exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeMerge(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "merge", Type: "merge"}
	pipeOpts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(worktreeDir, cmd)
		},
	}
	code := Merge(s, cfg, itemID, pipeOpts)
	if code != 0 {
		sr.Error = fmt.Sprintf("st merge exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeMergePrecheck(cfg *config.Config, worktreeDir string) StepResult {
	sr := StepResult{Step: "merge_precheck", Type: "merge_precheck"}
	if cfg.Pipeline == nil || cfg.Pipeline.Merge == nil || len(cfg.Pipeline.Merge.PreChecks) == 0 {
		sr.Passed = true // no pre-checks configured
		return sr
	}
	for _, check := range cfg.Pipeline.Merge.PreChecks {
		output, exitCode, err := runCmdInDir(worktreeDir, check)
		if err != nil && exitCode == 0 {
			sr.Error = fmt.Sprintf("pre-check exec error: %v", err)
			return sr
		}
		if exitCode != 0 {
			sr.Error = fmt.Sprintf("pre-check failed (exit %d): %s", exitCode, truncate(string(output), 200))
			return sr
		}
	}
	sr.Passed = true
	return sr
}

func executeDeploy(s *store.Store, cfg *config.Config, itemID, worktreeDir string) StepResult {
	sr := StepResult{Step: "deploy", Type: "deploy"}
	pipeOpts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return runCmdInDir(worktreeDir, cmd)
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

func executeUAT(s *store.Store, cfg *config.Config, itemID string) StepResult {
	sr := StepResult{Step: "uat", Type: "uat"}
	code := UAT(s, cfg, itemID, UATOpts{})
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
	fmt.Printf("\nApprove %s? [y/N]: ", itemID)
	response, err := engine.PromptUser("")
	if err != nil {
		sr.Error = fmt.Sprintf("prompt error: %v", err)
		return sr
	}
	answer := strings.TrimSpace(strings.ToLower(response))
	if answer == "y" || answer == "yes" {
		sr.Passed = true
	} else {
		sr.Error = "user rejected"
	}
	return sr
}

func executeClose(s *store.Store, cfg *config.Config, itemID string, step config.RunStepDef) StepResult {
	sr := StepResult{Step: "close", Type: "close"}
	resolution := step.Resolution
	if resolution == "" {
		resolution = "completed"
	}
	code := Close(s, cfg, itemID, resolution, CloseOpts{})
	if code != 0 {
		sr.Error = fmt.Sprintf("st close exited %d", code)
		return sr
	}
	sr.Passed = true
	return sr
}

func executeCommand(cfg *config.Config, itemID, sprintID string, step config.RunStepDef, worktreeDir string) StepResult {
	sr := StepResult{Step: step.Name(), Type: "command"}
	cmd := expandTemplate(step.Command, itemID, sprintID, worktreeDir, cfg)
	output, exitCode, err := runCmdInDir(worktreeDir, cmd)
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

	// Check required suites
	var missing []string
	for name := range cfg.Testing.RequiredSuites {
		val := ""
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				val = s
			}
		}
		if !strings.HasPrefix(val, "pass") {
			missing = append(missing, name)
		}
	}

	// Check triggered scope suites
	for name := range cfg.Testing.ScopeSuites {
		val := ""
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				val = s
			}
		}
		if val == "required" {
			missing = append(missing, name+" (triggered, not run)")
		}
	}

	if len(missing) > 0 {
		sr.Error = fmt.Sprintf("missing test evidence: %s", strings.Join(missing, ", "))
		return sr
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

	// Already approved — skip
	if item.PlanApproved {
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

		fmt.Printf("\nAccept this plan for %s? [y/N]: ", itemID)
		response, err := engine.PromptUser("")
		if err != nil {
			sr.Error = fmt.Sprintf("prompt error: %v", err)
			return sr
		}
		answer := strings.TrimSpace(strings.ToLower(response))
		if answer != "y" && answer != "yes" {
			sr.Error = "plan proposal rejected"
			return sr
		}

		// Claude's output should have updated the item via st update commands
		// in the prompt. Reload to pick up changes.
		s2, _ := store.New(cfg)
		item, _ = s2.Get(itemID)

		// Verify fields were actually set
		if item.Summary == "" || len(item.AcceptanceCriteria) == 0 {
			sr.Error = fmt.Sprintf("Claude did not set required fields. Missing: %s. "+
				"Set manually with: st edit %s <field>",
				planMissingFields(item.Summary == "", len(item.AcceptanceCriteria) == 0), itemID)
			return sr
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

		fmt.Printf("\nApprove design for %s? [y/N]: ", itemID)
		response, err := engine.PromptUser("")
		if err != nil {
			sr.Error = fmt.Sprintf("prompt error: %v", err)
			return sr
		}
		answer := strings.TrimSpace(strings.ToLower(response))
		if answer != "y" && answer != "yes" {
			sr.Error = "design not approved"
			return sr
		}
	}

	// Record approval on item (reload in case claude updated it)
	s3, _ := store.New(cfg)
	item, _ = s3.Get(itemID)
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

	b.WriteString("\nYour task:\n")
	if needsSummary {
		b.WriteString(fmt.Sprintf("1. Write a clear summary and set it: st update %s summary --stdin\n", itemID))
	}
	if needsACs {
		b.WriteString(fmt.Sprintf("2. Write specific, testable acceptance criteria and set them: st update %s acceptance_criteria --stdin\n", itemID))
	}
	b.WriteString("\nAcceptance criteria must be:\n")
	b.WriteString("- Specific and testable (not vague)\n")
	b.WriteString("- Verifiable by automated tests or code inspection\n")
	b.WriteString("- Complete — cover the full scope of the work\n")
	b.WriteString("\nRead the codebase to understand the context before proposing.\n")
	b.WriteString("Print your proposed summary and acceptance criteria to stdout so the user can review them.\n")

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

	b.WriteString("## Delivery Instructions\n")
	b.WriteString("1. Implement the changes\n")
	b.WriteString("2. Run ALL required test suites (above) — they must pass BEFORE committing\n")
	b.WriteString("3. Self-review: run `git diff` and review all changes\n")
	b.WriteString("4. Commit and push your branch\n")
	b.WriteString("5. Create a pull request with `gh pr create`\n")
	b.WriteString(fmt.Sprintf("6. Record the PR: `st pr %s --repo <repo-name> --pr <number>`\n", itemID))
	b.WriteString("7. STOP here. Do NOT merge. Report your results.\n\n")

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

	// Output format
	args = append(args, "--output-format", "json")

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

	// No session persistence
	args = append(args, "--no-session-persistence")

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
	if strings.Contains(s, "{branch}") {
		branch := ""
		if worktreeDir != "" {
			out, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
			if err == nil {
				branch = strings.TrimSpace(string(out))
			}
		}
		s = strings.ReplaceAll(s, "{branch}", branch)
	}
	return s
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

func isEligible(s *store.Store, cfg *config.Config, itemID string) bool {
	item, ok := s.Get(itemID)
	if !ok {
		return false
	}
	if cfg.IsTerminalStatus(item.Type, item.Status) {
		return false
	}
	if item.ClaimedBy != "" {
		return false
	}
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

func defaultRunClaude(cwd string, args []string, env []string) ([]byte, int, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			err = nil // not a real error, just non-zero exit
		}
	}
	return output, exitCode, err
}

func defaultPromptUser(_ string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return reader.ReadString('\n')
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
	completed, failed, rejected := 0, 0, 0
	var totalCost float64

	for _, r := range results {
		totalCost += r.TotalCost
		if r.Success {
			completed++
		} else {
			// Check if failed due to gate rejection
			for _, sr := range r.Steps {
				if sr.Type == "gate" && !sr.Passed {
					rejected++
					break
				}
			}
			if rejected == 0 || failed+rejected < len(results)-completed {
				failed++
			}
		}
	}

	fmt.Printf("\n=== Sprint %s Complete ===\n", sprintID)
	fmt.Printf("  Completed: %d\n", completed)
	if failed > 0 {
		fmt.Printf("  Failed:    %d\n", failed)
	}
	if rejected > 0 {
		fmt.Printf("  Rejected:  %d\n", rejected)
	}
	fmt.Printf("  Cost:      $%.2f\n", totalCost)
	fmt.Printf("  Duration:  %s\n", totalDuration.Round(time.Second))
}
