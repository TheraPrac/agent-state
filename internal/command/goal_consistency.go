package command

import (
	"fmt"
	"io"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// GoalConsistencyCheck reports items that are in a goal's must_do but not
// in item.Goals (or vice-versa). Returns 0 if clean, 1 if any drift found.
func GoalConsistencyCheck(s *store.Store, cfg *config.Config) int {
	return goalConsistencyCheckTo(os.Stdout, s, cfg)
}

func goalConsistencyCheckTo(w io.Writer, s *store.Store, cfg *config.Config) int {
	goals := s.List(store.TypeFilter("goal"))

	driftFound := false

	for _, goal := range goals {
		if goal.Status != "active" {
			continue
		}

		// Build mustDoIDs: all item IDs in goal.MustDo (across all buckets).
		mustDoSet := make(map[string]bool)
		for _, ids := range goal.MustDo {
			for _, id := range ids {
				mustDoSet[id] = true
			}
		}

		// Build goalsIDs: all items from s.All() where item.Goals contains this goalID.
		goalsSet := make(map[string]bool)
		for _, item := range s.All() {
			for _, gid := range item.Goals {
				if gid == goal.ID {
					goalsSet[item.ID] = true
					break
				}
			}
		}

		// inMustDoNotGoals: in must_do but item.Goals doesn't reference this goal.
		var inMustDoNotGoals []string
		for id := range mustDoSet {
			if !goalsSet[id] {
				inMustDoNotGoals = append(inMustDoNotGoals, id)
			}
		}

		// inGoalsNotMustDo: item.Goals references this goal but not in must_do.
		var inGoalsNotMustDo []string
		for id := range goalsSet {
			if !mustDoSet[id] {
				inGoalsNotMustDo = append(inGoalsNotMustDo, id)
			}
		}

		if len(inMustDoNotGoals) == 0 && len(inGoalsNotMustDo) == 0 {
			continue
		}

		driftFound = true
		fmt.Fprintf(w, "DRIFT %s — %s:\n", goal.ID, goal.Title)
		for _, id := range inMustDoNotGoals {
			fmt.Fprintf(w, "  in must_do, missing from item.Goals: %s\n", id)
		}
		for _, id := range inGoalsNotMustDo {
			fmt.Fprintf(w, "  in item.Goals, missing from must_do:  %s\n", id)
		}
	}

	if !driftFound {
		fmt.Fprintln(w, "ok — no goal consistency drift")
		return 0
	}
	return 1
}
