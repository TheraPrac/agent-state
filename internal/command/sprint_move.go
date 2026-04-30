package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintMove repositions a sprint within its parent epic. 1 = first.
// Renumbers siblings 1..N preserving their relative order; sprints in
// other epics are unaffected. Pairs with EpicMove (I-489) so the
// epic→sprint→item chain in `st queue show` matches the operator's
// strategic ordering.
func SprintMove(s *store.Store, cfg *config.Config, sprintID string, pos int) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}
	if err := r.MoveSprint(sprintID, pos); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}
	fmt.Printf("Moved sprint %s to position %d\n", sprintID, pos)
	autoSync(s, fmt.Sprintf("st sprint move: %s -> %d", sprintID, pos))
	return 0
}
