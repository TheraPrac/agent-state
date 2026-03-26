package command

import (
	"fmt"
	"os"
	"sort"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// LogOpts holds flags for the log command.
type LogOpts struct {
	Limit int // max entries to show (0 = all)
}

// Log displays the changelog for an item or all items.
func Log(s *store.Store, cfg *config.Config, id string, opts LogOpts) int {
	if id != "" {
		return logSingle(s, cfg, id, opts)
	}
	return logAll(cfg, opts)
}

func logSingle(s *store.Store, cfg *config.Config, id string, opts LogOpts) int {
	// Verify item exists
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	entries, err := changelog.Read(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading changelog: %v\n", err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Printf("%s — %s (no changelog entries)\n", id, item.Title)
		return 0
	}

	fmt.Printf("%s — %s\n", id, item.Title)
	printEntries(entries, opts.Limit)
	return 0
}

func logAll(cfg *config.Config, opts LogOpts) int {
	all, err := changelog.ReadAll(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading changelogs: %v\n", err)
		return 1
	}

	if len(all) == 0 {
		fmt.Println("No changelog entries.")
		return 0
	}

	// Sort item IDs for stable output
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		entries := all[id]
		fmt.Printf("── %s (%d entries) ──\n", id, len(entries))
		printEntries(entries, opts.Limit)
		fmt.Println()
	}

	return 0
}

func printEntries(entries []changelog.Entry, limit int) {
	count := len(entries)
	if limit > 0 && limit < count {
		entries = entries[count-limit:]
		fmt.Printf("  (showing last %d of %d)\n", limit, count)
	}
	for _, e := range entries {
		fmt.Printf("  %s\n", e.Format())
	}
}
