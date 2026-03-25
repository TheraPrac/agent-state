package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Start(s *store.Store, cfg *config.Config, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: as start <id>")
		return 2
	}

	id := args[0]
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// Check: must be in start status
	tc, ok := cfg.Types[item.Type]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", item.Type)
		return 1
	}
	if item.Status != tc.StartStatus {
		fmt.Fprintf(os.Stderr, "%s is %s, not %s — cannot start\n", id, item.Status, tc.StartStatus)
		return 1
	}

	// Check: not assigned to another agent
	agentID := cfg.AgentID()
	if item.AssignedTo != "" && item.AssignedTo != agentID {
		fmt.Fprintf(os.Stderr, "%s is assigned to %s — use `as release %s` first\n", id, item.AssignedTo, id)
		return 1
	}

	// Check: dependencies resolved
	g := deps.Build(s.All(), cfg)
	if g.IsBlocked(id) {
		unresolved := g.UnresolvedDeps(id)
		fmt.Fprintf(os.Stderr, "%s is blocked by: %v\n", id, unresolved)
		return 1
	}

	// Transition
	item.Doc.SetField("status", tc.ActiveStatus)
	item.Status = tc.ActiveStatus

	now := time.Now().Format(time.RFC3339)
	item.Doc.SetField("last_touched", now)

	if agentID != "" {
		item.Doc.SetField("assigned_to", agentID)
		item.AssignedTo = agentID
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("Started %s — %s\n", id, item.Title)
	if agentID != "" {
		fmt.Printf("  Assigned to: %s\n", agentID)
	}
	return 0
}
