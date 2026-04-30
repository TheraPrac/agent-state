package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintAdd adds items to a sprint and updates each item's sprint field.
func SprintAdd(s *store.Store, cfg *config.Config, sprintID string, itemIDs []string) int {
	if len(itemIDs) == 0 {
		fmt.Fprintln(os.Stderr, "no item IDs provided")
		return 2
	}

	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	// Validate sprint exists
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Validate all item IDs exist
	for _, id := range itemIDs {
		if _, ok := s.Get(id); !ok {
			fmt.Fprintf(os.Stderr, "item not found: %s\n", id)
			return 1
		}
	}

	// Add items to sprint (deduplicates)
	if err := r.SprintAddItems(sprintID, itemIDs); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Update each item's sprint field
	for _, id := range itemIDs {
		item, _ := s.Get(id)
		if item.Sprint == sprintID {
			continue // already set
		}
		capturedSprintID := sprintID
		capturedEpicID := sp.Epic
		if err := s.Mutate(id, func(item *model.Item) error {
			item.Doc.SetField("sprint", capturedSprintID)
			item.Sprint = capturedSprintID
			// Also set epic if not already set
			if item.Epic == "" && capturedEpicID != "" {
				item.Doc.SetField("epic", capturedEpicID)
				item.Epic = capturedEpicID
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
			return 1
		}
	}

	// Save registry
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	// Auto-queue each item with Approved=false, Source=sprint. Items already
	// in the queue (manually queued by the operator) are left alone — see
	// upsertQueueSprintEntry comment.
	queued := 0
	for _, id := range itemIDs {
		added, err := upsertQueueSprintEntry(cfg, id, sprintID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "queueing %s: %v\n", id, err)
			return 1
		}
		if added {
			queued++
		}
	}

	if queued > 0 {
		fmt.Printf("Added %d item(s) to sprint %s (queued %d as pending)\n", len(itemIDs), sprintID, queued)
	} else {
		fmt.Printf("Added %d item(s) to sprint %s\n", len(itemIDs), sprintID)
	}
	autoSync(s, fmt.Sprintf("st sprint add: %s += %d item(s)", sprintID, len(itemIDs)))
	return 0
}
