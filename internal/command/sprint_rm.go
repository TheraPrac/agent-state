package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintRm removes an item from a sprint and clears the item's sprint field.
func SprintRm(s *store.Store, cfg *config.Config, sprintID, itemID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	// Validate sprint exists and item is in it
	if err := r.SprintRemoveItem(sprintID, itemID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Clear the item's sprint field
	if _, ok := s.Get(itemID); ok {
		if err := s.Mutate(itemID, func(item *model.Item) error {
			item.Doc.SetField("sprint", "")
			item.Sprint = ""
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", itemID, err)
			return 1
		}
	}

	// Save registry
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Removed %s from sprint %s\n", itemID, sprintID)
	autoSync(s, fmt.Sprintf("st sprint rm: %s -= %s", sprintID, itemID))
	return 0
}
