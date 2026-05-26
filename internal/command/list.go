package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ListOpts holds flags for the list command.
type ListOpts struct {
	Type     string
	Status   string
	Tag      string
	Assigned string
	Goal     string
	Priority string // single value or comma-separated, e.g. "0" or "0,1"
	Sprint   string
	Epic     string
	Arc      string
}

func List(s *store.Store, cfg *config.Config, opts ListOpts) int {
	var filters []store.Filter
	if opts.Type != "" {
		filters = append(filters, store.TypeFilter(opts.Type))
	}
	if opts.Status != "" {
		filters = append(filters, store.StatusFilter(opts.Status))
	}
	if opts.Tag != "" {
		filters = append(filters, store.TagFilter(opts.Tag))
	}
	if opts.Assigned != "" {
		filters = append(filters, store.AssignedFilter(opts.Assigned))
	}
	if opts.Goal != "" {
		filters = append(filters, store.GoalFilter(opts.Goal))
	}
	if opts.Priority != "" {
		filters = append(filters, store.PriorityFilter(opts.Priority))
	}
	if opts.Sprint != "" {
		filters = append(filters, store.SprintFilter(opts.Sprint))
	}
	if opts.Epic != "" {
		filters = append(filters, store.EpicFilter(opts.Epic))
	}
	if opts.Arc != "" {
		filters = append(filters, store.ArcFilter(opts.Arc))
	}

	// Default: show non-terminal items
	if opts.Type == "" && opts.Status == "" && opts.Tag == "" && opts.Assigned == "" &&
		opts.Goal == "" && opts.Priority == "" && opts.Sprint == "" && opts.Epic == "" && opts.Arc == "" {
		filters = append(filters, store.NonTerminalFilter(cfg))
	}

	items := s.List(filters...)

	if len(items) == 0 {
		fmt.Println("No items found.")
		return 0
	}

	// I-444: render each row through the shared FormatItemRow helper so
	// st list aligns byte-for-byte with st status / st run status. The
	// (stage) and [assigned] suffixes stay list-specific and append after
	// the shared base, matching the indented-continuation pattern that
	// printQueuedTasks uses for ▶ blocks / ⊘ blocked by lines.
	g := deps.Build(s.All(), cfg)
	for _, item := range items {
		stage := ""
		if st, ok := item.Delivery["stage"]; ok {
			if str, ok := st.(string); ok && str != "" {
				stage = str
			}
		}
		assigned := ""
		if label := formatAssignment(item); label != "" {
			assigned = fmt.Sprintf(" [%s]", label)
		}

		fmt.Print(FormatItemRow(item, ItemRowOpts{
			TitleWidth:   45,
			Blocked:      g.IsBlocked(item.ID),
			PlanApproved: item.PlanApproved,
		}))
		if stage != "" {
			fmt.Printf("  (%s)", stage)
		}
		fmt.Print(assigned)
		fmt.Println()
	}

	fmt.Fprintf(os.Stderr, "\n%d items\n", len(items))
	return 0
}
