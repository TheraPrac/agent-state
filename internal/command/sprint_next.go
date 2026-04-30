package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintNext prints the next approved + unblocked + non-terminal queue
// entry whose item belongs to the given sprint. Mostly a wrapper around
// `st queue next --sprint <slug>`, with one extra guard: it validates
// the sprint slug against the registry and returns exit 1 on miss.
// `queue next --sprint` silently returns "no items" for an unknown
// slug, which masks typos; this command surfaces them.
func SprintNext(s *store.Store, cfg *config.Config, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}
	if _, err := r.SprintByID(sprintID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	return QueueNext(s, cfg, QueueNextOpts{Sprint: sprintID})
}
