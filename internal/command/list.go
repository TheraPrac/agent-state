package command

import (
	"flag"
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

func List(s *store.Store, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	typeF := fs.String("type", "", "filter by type")
	statusF := fs.String("status", "", "filter by status")
	tagF := fs.String("tag", "", "filter by tag")
	assignedF := fs.String("assigned", "", "filter by assignment")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var filters []store.Filter
	if *typeF != "" {
		filters = append(filters, store.TypeFilter(*typeF))
	}
	if *statusF != "" {
		filters = append(filters, store.StatusFilter(*statusF))
	}
	if *tagF != "" {
		filters = append(filters, store.TagFilter(*tagF))
	}
	if *assignedF != "" {
		filters = append(filters, store.AssignedFilter(*assignedF))
	}

	// Default: show non-terminal items
	if *typeF == "" && *statusF == "" && *tagF == "" && *assignedF == "" {
		filters = append(filters, store.NonTerminalFilter(cfg))
	}

	items := s.List(filters...)

	if len(items) == 0 {
		fmt.Println("No items found.")
		return 0
	}

	for _, item := range items {
		stage := ""
		if st, ok := item.Delivery["stage"]; ok {
			if str, ok := st.(string); ok && str != "" {
				stage = str
			}
		}
		assigned := ""
		if item.AssignedTo != "" {
			assigned = fmt.Sprintf(" [%s]", item.AssignedTo)
		}

		fmt.Printf("%-8s %-10s %s", item.ID, item.Status, item.Title)
		if stage != "" {
			fmt.Printf("  (%s)", stage)
		}
		fmt.Print(assigned)
		fmt.Println()
	}

	fmt.Fprintf(os.Stderr, "\n%d items\n", len(items))
	return 0
}
