package command

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// MetricsOpts holds flags for the metrics command.
type MetricsOpts struct {
	Type   string // filter by item type (issue|task|goal)
	Tag    string // filter by tag (substring match)
	Goal   string // filter by goal ID
	Sprint string // filter by sprint name
	Since  string // only items with completed_at >= since (YYYY-MM-DD or RFC3339)
	Sort   string // "cost" | "loc" | "duration" | "tokens" — default "cost"
	Top    int    // limit rows, 0 = no limit
	Format string // "" (table) | "json" | "csv"
}

// metricsRow holds the per-item data for rendering.
type metricsRow struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
	Status      string  `json:"status"`
	CostUSD     float64 `json:"cost_usd"`
	Tokens      int     `json:"total_tokens"`
	NetLOC      int     `json:"net_loc"`
	Files       int     `json:"files_changed"`
	Duration    string  `json:"duration"`
	DurationSec int64   `json:"duration_seconds"`
	Turns       int     `json:"turns"`
}

// Metrics renders a per-item table of cost, LOC, tokens, and duration.
func Metrics(s *store.Store, _ *config.Config, opts MetricsOpts) int {
	if opts.Sort == "" {
		opts.Sort = "cost"
	}

	// Parse --since threshold. YYYY-MM-DD is interpreted as local midnight so that
	// items completed on that calendar day in the local timezone are included.
	var sinceTime time.Time
	if opts.Since != "" {
		var err error
		sinceTime, err = time.Parse(time.RFC3339, opts.Since)
		if err != nil {
			sinceTime, err = time.ParseInLocation("2006-01-02", opts.Since, time.Local)
			if err != nil {
				fmt.Fprintf(os.Stderr, "metrics: invalid --since %q (use YYYY-MM-DD or RFC3339)\n", opts.Since)
				return 1
			}
		}
	}

	now := time.Now()
	var rows []metricsRow

	for _, item := range s.All() {
		// Type filter
		if opts.Type != "" && item.Type != opts.Type {
			continue
		}
		// Tag filter (substring match over item.Tags)
		if opts.Tag != "" {
			found := false
			for _, t := range item.Tags {
				if strings.Contains(strings.ToLower(t), strings.ToLower(opts.Tag)) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		// Goal filter
		if opts.Goal != "" {
			found := false
			for _, g := range item.Goals {
				if strings.EqualFold(g, opts.Goal) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		// Sprint filter
		if opts.Sprint != "" && !strings.EqualFold(item.Sprint, opts.Sprint) {
			continue
		}
		// --since filter: only items with a completed_at >= sinceTime; open items are excluded.
		if !sinceTime.IsZero() {
			if item.Completed == nil {
				continue
			}
			if item.Completed.Before(sinceTime) {
				continue
			}
		}

		// Skip items with no time_tracking data at all.
		if item.TimeTracking == nil {
			continue
		}

		isDone := item.Status == "done" || item.Status == "archived" || item.Completed != nil
		m := ExtractItemMetrics(item, "", now, isDone)
		if !m.HasMetrics() {
			continue
		}

		// Extra fields not in ItemMetrics.
		files := readIntField(item, "time_tracking", "files_changed_count")
		turns := readIntField(item, "time_tracking", "turn_count")
		totalTokens := m.InputTokens + m.OutputTokens

		// NetLOC from manifest (empty dir above) falls back to time_tracking fields.
		netLOC := m.NetLOC
		if netLOC == 0 {
			added := readIntField(item, "time_tracking", "lines_added")
			removed := readIntField(item, "time_tracking", "lines_removed")
			if added > 0 || removed > 0 {
				netLOC = added - removed
			}
		}

		// Raw seconds for --sort duration; prefer ProcessTime > AITime > Wall.
		var durSec int64
		var durStr string
		if m.ProcessTime > 0 {
			durSec = int64(m.ProcessTime.Seconds())
			durStr = formatDuration(m.ProcessTime)
		} else if m.AITime > 0 {
			durSec = int64(m.AITime.Seconds())
			durStr = formatDuration(m.AITime)
		} else if m.Wall > 0 {
			durSec = int64(m.Wall.Seconds())
			durStr = formatDuration(m.Wall)
		}

		rows = append(rows, metricsRow{
			ID:          item.ID,
			Title:       truncateTitle(item.Title, 40),
			Type:        item.Type,
			Status:      item.Status,
			CostUSD:     m.CostUSD,
			Tokens:      totalTokens,
			NetLOC:      netLOC,
			Files:       files,
			Duration:    durStr,
			DurationSec: durSec,
			Turns:       turns,
		})
	}

	// Sort descending by chosen field.
	sort.Slice(rows, func(i, j int) bool {
		switch opts.Sort {
		case "loc":
			ai, aj := rows[i].NetLOC, rows[j].NetLOC
			if ai < 0 {
				ai = -ai
			}
			if aj < 0 {
				aj = -aj
			}
			return ai > aj
		case "duration":
			return rows[i].DurationSec > rows[j].DurationSec
		case "tokens":
			return rows[i].Tokens > rows[j].Tokens
		default: // "cost"
			return rows[i].CostUSD > rows[j].CostUSD
		}
	})

	// Apply --top limit.
	if opts.Top > 0 && len(rows) > opts.Top {
		rows = rows[:opts.Top]
	}

	switch opts.Format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "metrics: encode error: %v\n", err)
			return 1
		}
	case "csv":
		if err := renderMetricsCSV(rows); err != nil {
			fmt.Fprintf(os.Stderr, "metrics: csv write error: %v\n", err)
			return 1
		}
	default:
		renderMetricsTable(rows)
	}
	return 0
}

func renderMetricsTable(rows []metricsRow) {
	if len(rows) == 0 {
		fmt.Println("\033[2mno items with metrics\033[0m")
		return
	}
	fmt.Printf("\033[1m\033[37m%-10s %-42s %-7s %9s %7s %8s %6s %8s %5s\033[0m\n",
		"ID", "Title", "Type", "Cost $", "Tokens", "Net LOC", "Files", "Duration", "Turns")
	fmt.Println(strings.Repeat("─", 107))
	for _, r := range rows {
		costStr := ""
		if r.CostUSD > 0 {
			costStr = fmt.Sprintf("$%.4f", r.CostUSD)
		}
		tokStr := ""
		if r.Tokens > 0 {
			tokStr = formatTokens(r.Tokens)
		}
		locStr := ""
		if r.NetLOC != 0 {
			locStr = formatLOC(r.NetLOC)
		}
		filesStr := ""
		if r.Files > 0 {
			filesStr = fmt.Sprintf("%d", r.Files)
		}
		turnsStr := ""
		if r.Turns > 0 {
			turnsStr = fmt.Sprintf("%d", r.Turns)
		}
		fmt.Printf("%-10s %-42s %-7s %9s %7s %8s %6s %8s %5s\n",
			r.ID, r.Title, r.Type, costStr, tokStr, locStr, filesStr, r.Duration, turnsStr)
	}
	fmt.Printf("\033[2m%d item(s)\033[0m\n", len(rows))
}

func renderMetricsCSV(rows []metricsRow) error {
	w := csv.NewWriter(os.Stdout)
	w.Write([]string{"id", "title", "type", "status", "cost_usd", "total_tokens", "net_loc", "files_changed", "duration", "duration_seconds", "turns"})
	for _, r := range rows {
		w.Write([]string{
			r.ID,
			r.Title,
			r.Type,
			r.Status,
			fmt.Sprintf("%.6f", r.CostUSD),
			fmt.Sprintf("%d", r.Tokens),
			fmt.Sprintf("%d", r.NetLOC),
			fmt.Sprintf("%d", r.Files),
			r.Duration,
			fmt.Sprintf("%d", r.DurationSec),
			fmt.Sprintf("%d", r.Turns),
		})
	}
	w.Flush()
	return w.Error()
}

// truncateTitle shortens s to max runes, appending "…" if truncated.
func truncateTitle(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
