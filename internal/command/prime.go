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
	Format  string // "markdown" (default) or "json"
	Compact bool   // compact output for hook injection (~50 lines)
}

type primeData struct {
	Active           []primeItem    `json:"active"`
	Ready            []primeItem    `json:"ready"`
	Issues           int            `json:"open_issues"`
	IssuesBySeverity map[string]int `json:"issues_by_severity"`
	Queued           int            `json:"queued_tasks"`
	Archive          int            `json:"archived"`
	Guidance         string         `json:"guidance,omitempty"`
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

	// Work queue
	queueEntries := LoadQueue(cfg)
	if len(queueEntries) > 0 {
		b.WriteString("## Queue\n")
		limit := 5
		if opts.Compact {
			limit = 3
		}
		shown := queueEntries
		if len(shown) > limit {
			shown = shown[:limit]
		}
		for i, e := range shown {
			item, ok := s.Get(e.ID)
			title := "(not found)"
			marker := ""
			if ok {
				title = truncate(item.Title, 45)
				if item.Status == "active" {
					marker = " ← ACTIVE"
				}
			}
			if !e.Approved {
				marker += " (pending approval)"
			}
			b.WriteString(fmt.Sprintf("  %d. %-8s %s%s\n", i+1, e.ID, title, marker))
		}
		if len(queueEntries) > limit {
			b.WriteString(fmt.Sprintf("  ... +%d more\n", len(queueEntries)-limit))
		}
		b.WriteString("\n")
	}

	// Next action directive
	if len(data.Active) > 0 {
		activeID := data.Active[0].ID
		action := NextAction(s, cfg, activeID)
		if action != "" {
			b.WriteString("## Next Action\n")
			b.WriteString(fmt.Sprintf("  Current: %s\n", activeID))
			b.WriteString(fmt.Sprintf("  → %s\n", action))
			b.WriteString("\n")
		}
	} else if len(queueEntries) > 0 {
		// No active work — suggest starting the first queue item
		nextID := ""
		for _, e := range queueEntries {
			if e.Approved {
				nextID = e.ID
				break
			}
		}
		if nextID != "" {
			b.WriteString("## Next Action\n")
			b.WriteString(fmt.Sprintf("  No active work. Next in queue: %s\n", nextID))
			b.WriteString(fmt.Sprintf("  → st start %s\n", nextID))
			b.WriteString("\n")
		}
	}

	// Ready queue
	readyLimit := 5
	if opts.Compact {
		readyLimit = 3
	}
	readyLabel := fmt.Sprintf("## Ready (top %d)\n", readyLimit)
	b.WriteString(readyLabel)
	shown := data.Ready
	if len(shown) > readyLimit {
		shown = shown[:readyLimit]
	}
	if len(shown) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range shown {
			b.WriteString(fmt.Sprintf("  %-8s p%d  %s\n", item.ID, item.Priority, item.Title))
		}
	}
	b.WriteString("\n")

	// Open issues by severity
	b.WriteString("## Open Issues\n")
	blocking := data.IssuesBySeverity["critical"] + data.IssuesBySeverity["high"]
	important := data.IssuesBySeverity["medium"]
	techDebt := data.IssuesBySeverity["low"]
	b.WriteString(fmt.Sprintf("  %d blocking  %d important  %d tech-debt\n\n", blocking, important, techDebt))

	// Summary
	b.WriteString("## Summary\n")
	b.WriteString(fmt.Sprintf("  %d open issues  %d queued tasks  %d archived\n\n", data.Issues, data.Queued, data.Archive))

	// Guidance
	if data.Guidance != "" {
		b.WriteString("## Guidance\n")
		b.WriteString(fmt.Sprintf("  %s\n\n", data.Guidance))
	}

	// Command reference (omit in compact mode)
	if !opts.Compact {
		b.WriteString("## Commands\n")
		b.WriteString("  as show <id>     — item detail\n")
		b.WriteString("  as start <id>    — claim and activate\n")
		b.WriteString("  as close <id> <resolution> — close with gates\n")
		b.WriteString("  as status        — dashboard\n")
		b.WriteString("  as ready         — unblocked items\n")
		b.WriteString("  as check         — validate all files\n")
	}

	fmt.Print(b.String())
	return 0
}

func buildPrimeData(s *store.Store, cfg *config.Config, g *deps.Graph) primeData {
	data := primeData{
		IssuesBySeverity: make(map[string]int),
	}

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

	// Ready queue (all of them — caller trims for display)
	ready := g.Ready()
	for _, item := range ready {
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

	// Counts and issue severity
	for _, item := range s.All() {
		if item.Type == "issue" && item.Status == "open" {
			data.Issues++
			sev := item.Severity
			if sev == "" {
				sev = "medium"
			}
			data.IssuesBySeverity[sev]++
		}
		if isStartStatus(item, cfg) {
			data.Queued++
		}
		if isTerminal(item, cfg) {
			data.Archive++
		}
	}

	// Guidance from config
	data.Guidance = cfg.Guidance

	return data
}
