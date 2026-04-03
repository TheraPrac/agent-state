package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Revert restores an item to its state before the most recent snapshot for the given step.
// If step is empty, reverts to the most recent snapshot of any step.
func Revert(s *store.Store, cfg *config.Config, id, step string, dryRun bool) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// Find the most recent snapshot
	entries, err := changelog.Read(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading changelog: %v\n", err)
		return 1
	}

	var snapshot *changelog.Entry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Op != "snapshot" {
			continue
		}
		if step != "" && e.Field != step {
			continue
		}
		snapshot = &entries[i]
		break
	}

	if snapshot == nil {
		if step != "" {
			fmt.Fprintf(os.Stderr, "no snapshot found for step %q on %s\n", step, id)
		} else {
			fmt.Fprintf(os.Stderr, "no snapshots found for %s\n", id)
		}
		return 1
	}

	currentContent := item.Doc.String()
	if currentContent == snapshot.NewValue {
		fmt.Printf("Item %s already matches the snapshot from %s (%s) — nothing to revert.\n",
			id, snapshot.Field, snapshot.Timestamp)
		return 0
	}

	// Show diff
	diff := changelog.DiffSnapshot(snapshot.NewValue, currentContent)
	fmt.Printf("Reverting %s to snapshot from step %q (%s):\n", id, snapshot.Field, snapshot.Timestamp)
	fmt.Printf("Changes that will be undone:\n%s\n", diff)

	if dryRun {
		fmt.Println("(dry run — no changes made)")
		return 0
	}

	// Write the snapshot content directly to the item file
	itemPath, pathOk := s.Path(id)
	if !pathOk {
		fmt.Fprintf(os.Stderr, "cannot find file path for %s\n", id)
		return 1
	}
	if err := os.WriteFile(itemPath, []byte(snapshot.NewValue+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", itemPath, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op:     "revert",
		Field:  snapshot.Field,
		Reason: fmt.Sprintf("reverted to pre-%s snapshot from %s", snapshot.Field, snapshot.Timestamp),
	})

	fmt.Printf("✓ Reverted %s to pre-%s state\n", id, snapshot.Field)
	return 0
}
