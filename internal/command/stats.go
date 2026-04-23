package command

import (
	"encoding/json"
	"fmt"
	"io"
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

	// Time/cost rollups (populated when opts.Time is set)
	Time *timeStats `json:"time,omitempty"`
}

type timeStats struct {
	ItemsWithMetrics  int                   `json:"items_with_metrics"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	TotalTurns        int                   `json:"total_turns"`
	TotalSessions     int                   `json:"total_sessions"`
	TotalProcessSecs  int                   `json:"total_process_seconds"`
	TotalAISecs       int                   `json:"total_ai_seconds"`
	TotalRegInput     int                   `json:"total_reg_input_tokens"`
	TotalRegOutput    int                   `json:"total_reg_output_tokens"`
	TotalCacheIn      int                   `json:"total_cache_in_tokens"`
	TotalCacheOut     int                   `json:"total_cache_out_tokens"`    // 5m writes
	TotalCacheOut1h   int                   `json:"total_cache_out_1h_tokens"` // 1h writes
	TotalLinesAdded   int                   `json:"total_lines_added"`
	TotalLinesRemoved int                   `json:"total_lines_removed"`
	TotalFilesChanged int                   `json:"total_files_changed"`
	ByModel           map[string]modelTotal `json:"by_model,omitempty"`
}

type modelTotal struct {
	Turns     int     `json:"turns"`
	RegInput  int     `json:"reg_input_tokens"`
	RegOutput int     `json:"reg_output_tokens"`
	CacheIn   int     `json:"cache_in_tokens"`
	CacheOut  int     `json:"cache_out_tokens"`
	CostUSD   float64 `json:"cost_usd"`
}

func Stats(s *store.Store, cfg *config.Config, opts StatsOpts) int {
	data := computeStats(s, cfg)
	if opts.Time {
		data.Time = computeTimeStats(s)
	}

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

	// Time/cost rollups
	if data.Time != nil {
		renderTimeStats(os.Stdout, data.Time)
	}

	return 0
}

// renderTimeStats prints the cross-item time/cost rollup. Writer is injectable
// so tests can capture output. Cost fields use %.6f to match storage precision
// in session_log.go — stops st stats readings from looking different than
// the raw time_tracking.ai_cost_usd field.
func renderTimeStats(w io.Writer, t *timeStats) {
	fmt.Fprintf(w, "\n\033[1m━━━ TIME & COST ━━━\033[0m\n")
	fmt.Fprintf(w, "  Items with metrics: %d\n", t.ItemsWithMetrics)
	if t.ItemsWithMetrics == 0 {
		return
	}
	fmt.Fprintf(w, "  Total cost:      $%.6f\n", t.TotalCostUSD)
	fmt.Fprintf(w, "  Total turns:     %d across %d distinct sessions\n", t.TotalTurns, t.TotalSessions)
	if t.TotalProcessSecs > 0 {
		fmt.Fprintf(w, "  Process time:    %s  (ai: %s)\n",
			formatDuration(time.Duration(t.TotalProcessSecs)*time.Second),
			formatDuration(time.Duration(t.TotalAISecs)*time.Second))
	}
	fmt.Fprintf(w, "  Tokens in:       reg %s  +  cache %s\n",
		formatTokens(t.TotalRegInput), formatTokens(t.TotalCacheIn))
	fmt.Fprintf(w, "  Tokens out:      %s  (cache writes: %s 5m + %s 1h)\n",
		formatTokens(t.TotalRegOutput),
		formatTokens(t.TotalCacheOut), formatTokens(t.TotalCacheOut1h))
	if t.TotalFilesChanged > 0 {
		fmt.Fprintf(w, "  Code:            %s (+%d / -%d across %d files)\n",
			formatLOC(t.TotalLinesAdded-t.TotalLinesRemoved),
			t.TotalLinesAdded, t.TotalLinesRemoved, t.TotalFilesChanged)
	}
	if len(t.ByModel) > 0 {
		fmt.Fprintln(w, "  By model:")
		// Stable order: sort by cost desc
		type kv struct {
			k string
			v modelTotal
		}
		pairs := make([]kv, 0, len(t.ByModel))
		for k, v := range t.ByModel {
			pairs = append(pairs, kv{k, v})
		}
		for i := 1; i < len(pairs); i++ {
			for j := i; j > 0 && pairs[j].v.CostUSD > pairs[j-1].v.CostUSD; j-- {
				pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
			}
		}
		for _, p := range pairs {
			fmt.Fprintf(w, "    %-22s %d turns, $%.6f, %s in / %s out\n",
				p.k, p.v.Turns, p.v.CostUSD,
				formatTokens(p.v.RegInput), formatTokens(p.v.RegOutput))
		}
	}
}

// computeTimeStats walks all items and sums the SessionLog time_tracking
// fields. Parses the per-model list (one line per model) from the Doc so the
// aggregation mirrors what's stored on each item.
func computeTimeStats(s *store.Store) *timeStats {
	out := &timeStats{ByModel: map[string]modelTotal{}}
	for _, item := range s.All() {
		if item.TimeTracking == nil {
			continue
		}
		turns := readIntField(item, "time_tracking", "turn_count")
		cost := readFloatField(item, "time_tracking", "ai_cost_usd")
		if turns == 0 && cost == 0 {
			continue
		}
		out.ItemsWithMetrics++
		out.TotalCostUSD += cost
		out.TotalTurns += turns
		out.TotalSessions += readIntField(item, "time_tracking", "session_count")
		out.TotalProcessSecs += readIntField(item, "time_tracking", "process_time_seconds")
		out.TotalAISecs += readIntField(item, "time_tracking", "ai_time_seconds")
		out.TotalRegInput += readIntField(item, "time_tracking", "reg_input_tokens")
		out.TotalRegOutput += readIntField(item, "time_tracking", "reg_output_tokens")
		out.TotalCacheIn += readIntField(item, "time_tracking", "cache_in_tokens")
		out.TotalCacheOut += readIntField(item, "time_tracking", "cache_out_tokens")
		out.TotalCacheOut1h += readIntField(item, "time_tracking", "cache_out_1h_tokens")
		out.TotalLinesAdded += readIntField(item, "time_tracking", "lines_added")
		out.TotalLinesRemoved += readIntField(item, "time_tracking", "lines_removed")
		out.TotalFilesChanged += readIntField(item, "time_tracking", "files_changed_count")

		// Per-model lines
		if item.Doc != nil {
			inBlock := false
			inTT := false
			for _, line := range item.Doc.Lines {
				if line.Indent == 0 && line.Key != "" {
					inTT = line.Key == "time_tracking"
					inBlock = false
					continue
				}
				if !inTT {
					continue
				}
				if line.Indent == 2 && line.Key == "by_model" {
					inBlock = true
					continue
				}
				if line.Indent <= 2 && line.Key != "" && line.Key != "by_model" {
					inBlock = false
					continue
				}
				if !inBlock {
					continue
				}
				raw := line.Raw
				raw = trimListPrefix(raw)
				colonIdx := indexColonInModelLine(raw)
				if colonIdx <= 0 {
					continue
				}
				modelID := raw[:colonIdx]
				agg := parseByModelLine(raw)
				prev := out.ByModel[modelID]
				prev.Turns += agg.Turns
				prev.RegInput += agg.RegIn
				prev.RegOutput += agg.RegOut
				prev.CacheIn += agg.CacheIn
				prev.CacheOut += agg.CacheOut
				prev.CostUSD += agg.Cost
				out.ByModel[modelID] = prev
			}
		}
	}
	return out
}

func trimListPrefix(raw string) string {
	r := raw
	// Strip leading whitespace
	for len(r) > 0 && (r[0] == ' ' || r[0] == '\t') {
		r = r[1:]
	}
	if len(r) >= 2 && r[0] == '-' && r[1] == ' ' {
		r = r[2:]
	}
	return r
}

func indexColonInModelLine(s string) int {
	for i, c := range s {
		if c == ':' {
			return i
		}
	}
	return -1
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
