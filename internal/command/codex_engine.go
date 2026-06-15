package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/pricing"
	"github.com/jfinlinson/agent-state/internal/store"
)

// defaultCodexModel is the OpenAI model ID used for Codex pricing when the
// caller does not supply an explicit --ae-model. It must be present in the
// OpenAI pricing table (pricing.EstimateOpenAICostUSD).
const defaultCodexModel = "codex-mini-latest"

// CodexEngine implements AgentEngine using the Codex CLI:
//
//	codex exec [--json --skip-git-repo-check --sandbox read-only] <prompt>
//	codex exec resume <thread_id> [--json --skip-git-repo-check --sandbox read-only] <prompt>
//
// It reads JSONL events from stdout: extracts the thread_id from the
// thread.started event, sums per-turn token usage from turn.completed events,
// persists last-seen cursors keyed by thread_id (for resume dedup), computes
// estimated cost from the OpenAI pricing table, and calls SessionLog.
type CodexEngine struct {
	engine RunEngine
}

func (e CodexEngine) Name() string { return "codex" }

func (e CodexEngine) Run(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, codexSessionID string, isResume bool) StepResult {
	sr := StepResult{Step: step.Name(), Type: "claude"} // type=claude kept for pipeline compat

	// Build prompt (same helpers as Claude path)
	prompt := step.Prompt
	if prompt == "" {
		prompt = buildDefaultPrompt(s, cfg, itemID, sprintID)
	} else {
		prompt = expandTemplate(prompt, itemID, sprintID, worktreeDir, cfg)
	}
	prompt += buildItemContext(s, cfg, itemID, worktreeDir)
	if !opts.NoCoordination {
		prompt += buildCoordinationBlock(s, cfg, cfg.Identity().ID, itemID)
	}

	// Standard flags for non-interactive --json mode. The prompt is passed
	// POSITIONALLY (not via -p, which in codex selects a config profile).
	stdFlags := []string{"--json", "--skip-git-repo-check", "--sandbox", "read-only"}

	var args []string
	if isResume && codexSessionID != "" {
		// resume: codex exec resume <thread_id> [flags] <prompt>
		args = append([]string{"exec", "resume", codexSessionID}, stdFlags...)
		args = append(args, prompt)
	} else {
		// fresh: codex exec [flags] <prompt>
		args = append([]string{"exec"}, stdFlags...)
		args = append(args, prompt)
	}

	env := []string{
		"ST_RUN_ITEM=" + itemID,
		"ST_RUN_STEP=" + step.Name(),
	}
	if agentID := cfg.AgentID(); agentID != "" {
		env = append(env, "AS_AGENT_ID="+agentID)
	}
	env = append(env, step.ExtraEnv...)

	runFn := engine.RunCodex
	if runFn == nil {
		runFn = defaultRunCodex
	}
	output, exitCode, err := runFn(worktreeDir, args, env)
	if err != nil {
		sr.Error = fmt.Sprintf("codex exec error: %v", err)
		return sr
	}

	// Extract thread_id from the thread.started JSONL event on first run.
	discoveredID := parseCodexSessionID(output)
	if discoveredID != "" && !isResume {
		_ = s.Mutate(itemID, func(item *model.Item) error {
			item.SetNested("delivery", "codex_session_id", discoveredID)
			return nil
		})
		codexSessionID = discoveredID
	}

	// Sum per-turn token usage from all turn.completed events in this run.
	cur := parseCodexTurnUsage(output)

	// Resolve the OpenAI model ID for pricing. opts.CodexModel is set by
	// --ae-model; fall back to the hardcoded default. Never use the Claude
	// model id from resolveStepModel — it is absent from the OpenAI table.
	pricingModel := opts.CodexModel
	if pricingModel == "" {
		pricingModel = defaultCodexModel
	}

	sr.Output = truncate(strings.TrimSpace(string(output)), 500)
	sr.FullOutput = strings.TrimSpace(string(output))
	if exitCode != 0 {
		sr.Error = fmt.Sprintf("codex exited %d", exitCode)
		return sr
	}

	// Compute per-turn delta to avoid double-counting across resumes if codex
	// exec resume re-emits prior-turn usage. Keyed by thread_id.
	// NOTE: delta advance happens AFTER the exit-code check so a failed run
	// does not consume the cursor — the next resume will re-count correctly.
	if cur.Input > 0 || cur.Output > 0 {
		if codexSessionID == "" {
			fmt.Fprintf(os.Stderr, "[%s] codex: token usage non-zero but no thread_id — usage not recorded\n", itemID)
		} else {
			usage, deltaErr := computeCodexUsageDelta(s, cfg, itemID, pricingModel, codexSessionID, cur)
			if deltaErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] codex usage delta: %v\n", itemID, deltaErr)
			} else {
				cost, costErr := pricing.EstimateOpenAICostUSD(pricingModel, usage.RegInputTokens, usage.RegOutputTokens, usage.CachedInTokens)
				if costErr != nil {
					fmt.Fprintf(os.Stderr, "[%s] codex cost estimate: %v (recorded without cost)\n", itemID, costErr)
				} else {
					usage.CostUSD = cost
				}
				applyUsageToStepResult(&sr, usage)
				recordCodexUsage(s, cfg, itemID, step.Name(), usage)
			}
		}
	}
	sr.Passed = true
	return sr
}
