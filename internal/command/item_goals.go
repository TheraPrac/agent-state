package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ItemGoalsAdd adds goalIDs to item's goals list. Returns exit code.
func ItemGoalsAdd(s *store.Store, cfg *config.Config, itemID string, goalIDs []string) int {
	item, ok := s.Get(itemID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", itemID)
		return 1
	}

	seen := make(map[string]bool, len(goalIDs))
	for _, goalID := range goalIDs {
		goal, exists := s.Get(goalID)
		if !exists {
			fmt.Fprintf(os.Stderr, "goals add: goal not found: %s\n", goalID)
			return 1
		}
		if goal.Type != "goal" {
			fmt.Fprintf(os.Stderr, "goals add: %s is not a goal (type=%s)\n", goalID, goal.Type)
			return 1
		}
		if goalID == itemID {
			fmt.Fprintf(os.Stderr, "goals add: %s cannot be added to its own goals list\n", goalID)
			return 1
		}
		for _, existing := range item.Goals {
			if existing == goalID {
				fmt.Fprintf(os.Stderr, "goals add: %s already in goals of %s\n", goalID, itemID)
				return 1
			}
		}
		if seen[goalID] {
			fmt.Fprintf(os.Stderr, "goals add: %s appears more than once in the request\n", goalID)
			return 1
		}
		seen[goalID] = true
	}

	if err := s.Mutate(itemID, func(it *model.Item) error {
		it.Goals = append(it.Goals, goalIDs...)
		if it.Doc != nil {
			formatted := make([]string, len(it.Goals))
			for i, g := range it.Goals {
				formatted[i] = "- " + g
			}
			it.Doc.ReplaceList("goals", formatted)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "goals add %s: %v\n", itemID, err)
		return 1
	}

	for _, goalID := range goalIDs {
		_ = changelog.Append(cfg, itemID, changelog.Entry{
			Op:       "goals_add",
			Field:    "goals",
			NewValue: goalID,
			Agent:    cfg.Identity().ID,
		})
		fmt.Printf("Added %s to %s goals\n", goalID, itemID)
	}
	return 0
}

// ItemGoalsRemove removes goalIDs from item's goals list. Returns exit code.
func ItemGoalsRemove(s *store.Store, cfg *config.Config, itemID string, goalIDs []string) int {
	item, ok := s.Get(itemID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", itemID)
		return 1
	}

	seen := make(map[string]bool, len(goalIDs))
	for _, goalID := range goalIDs {
		found := false
		for _, existing := range item.Goals {
			if existing == goalID {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "goals remove: %s not in goals of %s\n", goalID, itemID)
			return 1
		}
		if seen[goalID] {
			fmt.Fprintf(os.Stderr, "goals remove: %s appears more than once in the request\n", goalID)
			return 1
		}
		seen[goalID] = true
	}

	if err := s.Mutate(itemID, func(it *model.Item) error {
		for _, goalID := range goalIDs {
			it.Goals = removeStringFromSlice(it.Goals, goalID)
		}
		if it.Doc != nil {
			formatted := make([]string, len(it.Goals))
			for i, g := range it.Goals {
				formatted[i] = "- " + g
			}
			it.Doc.ReplaceList("goals", formatted)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "goals remove %s: %v\n", itemID, err)
		return 1
	}

	for _, goalID := range goalIDs {
		_ = changelog.Append(cfg, itemID, changelog.Entry{
			Op:       "goals_remove",
			Field:    "goals",
			OldValue: goalID,
			Agent:    cfg.Identity().ID,
		})
		fmt.Printf("Removed %s from %s goals\n", goalID, itemID)
	}
	return 0
}

// removeStringFromSlice returns a new slice with all occurrences of val removed.
func removeStringFromSlice(slice []string, val string) []string {
	out := slice[:0:0]
	for _, s := range slice {
		if s != val {
			out = append(out, s)
		}
	}
	return out
}
