package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Release clears the agent assignment on an item, allowing another agent to claim it.
func Release(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.AssignedTo == "" {
		fmt.Fprintf(os.Stderr, "%s is not assigned to anyone\n", id)
		return 1
	}

	oldAgent := item.AssignedTo
	item.AssignedTo = ""
	item.Doc.SetField("assigned_to", "")

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "release", Field: "assigned_to", OldValue: oldAgent,
	})

	fmt.Printf("Released %s — was assigned to %s\n", id, oldAgent)
	return 0
}
