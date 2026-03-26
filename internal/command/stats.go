package command

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// StatsOpts holds flags for the stats command.
type StatsOpts struct {
	JSON bool
	Time bool
}

type statsData struct {
	ByType     map[string]map[string]int `json:"by_type"`
	BySeverity map[string]int            `json:"by_severity,omitempty"`
	ByPriority map[int]int               `json:"by_priority,omitempty"`
	Total      int                       `json:"total"`
	ThisWeek   struct {
		Created int `json:"created"`
		Closed  int `json:"closed"`
	} `json:"this_week"`
}

func Stats(s *store.Store, cfg *config.Config, opts StatsOpts) int {
	data := computeStats(s, cfg)

	if opts.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(data)
		return 0
	}

	// Text output
	fmt.Println("\033[1m\033[37m━━━ STATS ━━━\033[0m")

	// By type and status
	for typeName, tc := range cfg.Types {
		counts := data.ByType[typeName]
		if counts == nil {
			continue
		}
		parts := []string{}
		for _, status := range tc.Statuses {
			n := counts[status]
			if n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, status))
			}
		}
		if len(parts) > 0 {
			fmt.Printf("  %-8s %s\n", capitalize(typeName)+"s:", joinParts(parts))
		}
	}
	fmt.Printf("  Total:   %d\n\n", data.Total)

	// Severity distribution
	if len(data.BySeverity) > 0 {
		fmt.Println("  By severity (open issues):")
		for _, sev := range []string{"critical", "high", "medium", "low"} {
			if n, ok := data.BySeverity[sev]; ok && n > 0 {
				fmt.Printf("    %s: %d\n", sev, n)
			}
		}
		fmt.Println()
	}

	// Priority distribution
	if len(data.ByPriority) > 0 {
		fmt.Println("  By priority (queued tasks):")
		for p := 0; p <= 4; p++ {
			if n, ok := data.ByPriority[p]; ok && n > 0 {
				fmt.Printf("    p%d: %d\n", p, n)
			}
		}
		fmt.Println()
	}

	// This week
	fmt.Printf("  This week: %d created, %d closed\n", data.ThisWeek.Created, data.ThisWeek.Closed)

	return 0
}

func computeStats(s *store.Store, cfg *config.Config) statsData {
	data := statsData{
		ByType:     make(map[string]map[string]int),
		BySeverity: make(map[string]int),
		ByPriority: make(map[int]int),
	}

	weekAgo := time.Now().AddDate(0, 0, -7)

	for _, item := range s.All() {
		data.Total++

		// By type + status
		if data.ByType[item.Type] == nil {
			data.ByType[item.Type] = make(map[string]int)
		}
		data.ByType[item.Type][item.Status]++

		// Severity (open issues)
		if item.Type == "issue" && item.Status == "open" {
			sev := item.Severity
			if sev == "" {
				sev = "medium"
			}
			data.BySeverity[sev]++
		}

		// Priority (queued tasks)
		if item.Type == "task" && isQueuedTask(item, cfg) {
			p := 2
			if item.Priority != nil {
				p = *item.Priority
			}
			data.ByPriority[p]++
		}

		// This week
		if item.Created.After(weekAgo) {
			data.ThisWeek.Created++
		}
		if item.Completed != nil && item.Completed.After(weekAgo) {
			data.ThisWeek.Closed++
		}
	}

	return data
}

func isQueuedTask(item *model.Item, cfg *config.Config) bool {
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	return item.Status == tc.StartStatus
}

func joinParts(parts []string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += "  "
		}
		result += p
	}
	return result
}
