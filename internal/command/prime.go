package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/store"
)

// PrimeOpts holds flags for the prime command.
type PrimeOpts struct {
	Format string // "markdown" (default) or "json"
}

type primeData struct {
	Active  []primeItem `json:"active"`
	Ready   []primeItem `json:"ready"`
	Issues  int         `json:"open_issues"`
	Queued  int         `json:"queued_tasks"`
	Archive int         `json:"archived"`
}

type primeItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Stage    string `json:"stage,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Assigned string `json:"assigned,omitempty"`
}

func Prime(s *store.Store, cfg *config.Config, opts PrimeOpts) int {
	g := deps.Build(s.All(), cfg)
	data := buildPrimeData(s, cfg, g)

	if opts.Format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(data)
		return 0
	}

	// Markdown output
	var b strings.Builder
	b.WriteString("=== AS CONTEXT ===\n\n")

	// Active work
	b.WriteString("## Active Work\n")
	if len(data.Active) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range data.Active {
			line := fmt.Sprintf("  %-8s %s", item.ID, item.Title)
			if item.Stage != "" {
				line += fmt.Sprintf("  stage: %s", item.Stage)
			}
			if item.Assigned != "" {
				line += fmt.Sprintf("  [%s]", item.Assigned)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n")

	// Ready queue (top 5)
	b.WriteString("## Ready (top 5)\n")
	if len(data.Ready) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range data.Ready {
			b.WriteString(fmt.Sprintf("  %-8s p%d  %s\n", item.ID, item.Priority, item.Title))
		}
	}
	b.WriteString("\n")

	// Summary
	b.WriteString("## Summary\n")
	b.WriteString(fmt.Sprintf("  %d open issues  %d queued tasks  %d archived\n\n", data.Issues, data.Queued, data.Archive))

	// Command reference
	b.WriteString("## Commands\n")
	b.WriteString("  as show <id>     — item detail\n")
	b.WriteString("  as start <id>    — claim and activate\n")
	b.WriteString("  as close <id> <resolution> — close with gates\n")
	b.WriteString("  as status        — dashboard\n")
	b.WriteString("  as ready         — unblocked items\n")
	b.WriteString("  as check         — validate all files\n")

	fmt.Print(b.String())
	return 0
}

func buildPrimeData(s *store.Store, cfg *config.Config, g *deps.Graph) primeData {
	data := primeData{}

	// Active work
	active := s.List(store.StatusFilter("active"))
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	for _, item := range active {
		data.Active = append(data.Active, primeItem{
			ID:       item.ID,
			Title:    item.Title,
			Stage:    deliveryStage(item),
			Assigned: item.AssignedTo,
		})
	}

	// Ready queue (top 5)
	ready := g.Ready()
	limit := 5
	if len(ready) < limit {
		limit = len(ready)
	}
	for _, item := range ready[:limit] {
		p := 2
		if item.Priority != nil {
			p = *item.Priority
		}
		data.Ready = append(data.Ready, primeItem{
			ID:       item.ID,
			Title:    item.Title,
			Priority: p,
		})
	}

	// Counts
	for _, item := range s.All() {
		if item.Type == "issue" && item.Status == "open" {
			data.Issues++
		}
		if isStartStatus(item, cfg) {
			data.Queued++
		}
		if isTerminal(item, cfg) {
			data.Archive++
		}
	}

	return data
}
