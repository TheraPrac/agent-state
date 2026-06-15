package command

import (
	"fmt"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// AgentEngine is the adapter interface for executing one pipeline step.
// ClaudeEngine and CodexEngine each implement it; selectAgentEngine picks
// the right one based on RunOpts.AgentEngine.
type AgentEngine interface {
	// Name returns the engine identifier ("claude" or "codex").
	Name() string
	// Run executes a single pipeline step and returns the result.
	Run(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, isResume bool) StepResult
}

// ClaudeEngine wraps the existing executeClaude path behind the AgentEngine
// interface so the pipeline dispatch in executeStepWithSession stays clean.
type ClaudeEngine struct{}

func (ClaudeEngine) Name() string { return "claude" }

func (ClaudeEngine) Run(s *store.Store, cfg *config.Config, itemID, sprintID string, step config.RunStepDef, opts RunOpts, engine RunEngine, worktreeDir, claudeSessionID string, isResume bool) StepResult {
	return executeClaude(s, cfg, itemID, sprintID, step, opts, engine, worktreeDir, claudeSessionID, isResume)
}

// selectAgentEngine returns the AgentEngine named by opts.AgentEngine.
// Empty or "claude" → ClaudeEngine; "codex" → CodexEngine.
// Callers should validate opts.AgentEngine with validateAgentEngine before use.
func selectAgentEngine(opts RunOpts, engine RunEngine) AgentEngine {
	if opts.AgentEngine == "codex" {
		return CodexEngine{engine: engine}
	}
	return ClaudeEngine{}
}

// ValidateAgentEngine returns an error if ae is not a supported engine name.
func ValidateAgentEngine(ae string) error {
	switch ae {
	case "", "claude", "codex":
		return nil
	}
	return fmt.Errorf("invalid --agent-engine %q: must be \"claude\" or \"codex\"", ae)
}

// execAgentRaw dispatches a raw subprocess launch to the appropriate engine.
// Used by prep's plan-generation path, which builds its own arg slice rather
// than going through a full Run() call.
func execAgentRaw(engine RunEngine, agentEngine, cwd string, args []string, env []string) ([]byte, int, error) {
	if agentEngine == "codex" {
		if engine.RunCodex != nil {
			return engine.RunCodex(cwd, args, env)
		}
		return defaultRunCodex(cwd, args, env)
	}
	return engine.RunClaude(cwd, args, env)
}
