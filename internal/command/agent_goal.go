package command

import (
	"fmt"
	"io"
	"os"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// AgentGoalSet pins the calling agent to goalID so st next / st recommend
// only surface candidates linked to that goal. Validates that the goal
// exists, has type "goal", and is active before writing.
func AgentGoalSet(s *store.Store, cfg *config.Config, goalID string) int {
	return agentGoalSetTo(os.Stdout, os.Stderr, s, cfg, goalID)
}

func agentGoalSetTo(w, werr io.Writer, s *store.Store, cfg *config.Config, goalID string) int {
	agentID := cfg.Identity().ID
	if agentID == "" {
		fmt.Fprintln(werr, "agent goal set: no agent identity resolved (set AS_AGENT_ID or run from an agent workspace)")
		return 1
	}

	goal, ok := s.Get(goalID)
	if !ok {
		fmt.Fprintf(werr, "agent goal set: %s not found — run `st goal list` to see active goals\n", goalID)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(werr, "agent goal set: %s is type %q, not \"goal\"\n", goalID, goal.Type)
		return 1
	}
	if goal.Status != "active" {
		fmt.Fprintf(werr, "agent goal set: %s has status %q — only active goals are valid focus targets\n  run `st goal list` to see active goals\n", goalID, goal.Status)
		return 1
	}

	if err := agent.SetGoalFocus(cfg, agentID, goalID); err != nil {
		fmt.Fprintf(werr, "agent goal set: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "goal focus set: %s — %s\n  st next and st recommend will now filter to this goal\n", goalID, goal.Title)
	return 0
}

// AgentGoalClear removes the goal focus for the calling agent, restoring
// global ranking in st next / st recommend.
func AgentGoalClear(cfg *config.Config) int {
	return agentGoalClearTo(os.Stdout, os.Stderr, cfg)
}

func agentGoalClearTo(w, werr io.Writer, cfg *config.Config) int {
	agentID := cfg.Identity().ID
	if agentID == "" {
		fmt.Fprintln(werr, "agent goal clear: no agent identity resolved")
		return 1
	}
	if err := agent.ClearGoalFocus(cfg, agentID); err != nil {
		fmt.Fprintf(werr, "agent goal clear: %v\n", err)
		return 1
	}
	fmt.Fprintln(w, "goal focus cleared — st next and st recommend use global ranking")
	return 0
}

// AgentGoalShow prints the current goal focus for the calling agent,
// or "(none)" when unset.
func AgentGoalShow(s *store.Store, cfg *config.Config) int {
	return agentGoalShowTo(os.Stdout, s, cfg)
}

func agentGoalShowTo(w io.Writer, s *store.Store, cfg *config.Config) int {
	agentID := cfg.Identity().ID
	focus := agent.GetGoalFocus(cfg, agentID)
	if focus == "" {
		fmt.Fprintln(w, "goal focus: (none)")
		fmt.Fprintln(w, "  set with: st agent goal set <goal-id>")
		return 0
	}
	title := focus
	if goal, ok := s.Get(focus); ok {
		title = goal.Title
	}
	fmt.Fprintf(w, "goal focus: %s — %s\n", focus, title)
	return 0
}
