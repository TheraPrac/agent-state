package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// StatsOpts holds flags for the stats command.
type StatsOpts struct {
	JSON bool
	Time bool
}

type statsData struct {
	ByType     map[string]map[string]int `json:"by_type"`
	// I-406: BySeverity removed; ByPriority is the unified bucket.
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
	TotalReasoning    int                   `json:"total_reasoning_tokens"`
	TotalTokens       int                   `json:"total_tokens"`
	TotalCacheIn      int                   `json:"total_cache_in_tokens"`
	TotalCacheOut     int                   `json:"total_cache_out_tokens"`    // 5m writes
	TotalCacheOut1h   int                   `json:"total_cache_out_1h_tokens"` // 1h writes
	UnknownCostTurns  int                   `json:"unknown_cost_turns"`
	TotalLinesAdded   int                   `json:"total_lines_added"`
	TotalLinesRemoved int                   `json:"total_lines_removed"`
	TotalFilesChanged int                   `json:"total_files_changed"`
	ByModel           map[string]modelTotal `json:"by_model,omitempty"`
	// I-591: estimate calibration — only for closed items with both fields.
	EstimatedItems     int     `json:"estimated_items,omitempty"`
	TotalEstimatedHrs  float64 `json:"total_estimated_hours,omitempty"`
	TotalActualHrs     float64 `json:"total_actual_hours,omitempty"`
}

type modelTotal struct {
	Turns            int     `json:"turns"`
	RegInput         int     `json:"reg_input_tokens"`
	RegOutput        int     `json:"reg_output_tokens"`
	CacheIn          int     `json:"cache_in_tokens"`
	CacheOut         int     `json:"cache_out_tokens"`
	UnknownCostTurns int     `json:"unknown_cost_turns,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
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

	// I-406: severity buckets removed — priority is the unified bucket
	// for both open issues and queued tasks.

	// Priority distribution
	if len(data.ByPriority) > 0 {
		fmt.Println("  By priority (open issues + queued tasks):")
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
// so tests can capture output.
//
// I-569 step 5: cost rendered as a "synthetic API cost estimate" — computed
// at render time from real_tokens × current pricing rates. The number is
// not a real billing line (Max plan has no per-call billing) and shifts
// when the rate table changes, so the label calls that out.
func renderTimeStats(w io.Writer, t *timeStats) {
	fmt.Fprintf(w, "\n\033[1m━━━ TIME & COST ━━━\033[0m\n")
	fmt.Fprintf(w, "  Items with metrics: %d\n", t.ItemsWithMetrics)
	if t.ItemsWithMetrics == 0 {
		return
	}
	fmt.Fprintf(w, "  %s: $%.6f\n", SyntheticCostLabel, t.TotalCostUSD)
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
	if t.TotalReasoning > 0 || t.TotalTokens > 0 {
		fmt.Fprintf(w, "  Reasoning/total: %s reasoning / %s total\n",
			formatTokens(t.TotalReasoning), formatTokens(t.TotalTokens))
	}
	if t.TotalFilesChanged > 0 {
		fmt.Fprintf(w, "  Code:            %s (+%d / -%d across %d files)\n",
			formatLOC(t.TotalLinesAdded-t.TotalLinesRemoved),
			t.TotalLinesAdded, t.TotalLinesRemoved, t.TotalFilesChanged)
	}
	if len(t.ByModel) > 0 {
		fmt.Fprintln(w, "  By provider/model:")
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
			costText := fmt.Sprintf("$%.6f", p.v.CostUSD)
			if p.v.UnknownCostTurns > 0 {
				costText = fmt.Sprintf("%s (%d unknown-cost)", costText, p.v.UnknownCostTurns)
			}
			fmt.Fprintf(w, "    %-22s %d turns, %s, %s in / %s out\n",
				p.k, p.v.Turns, costText,
				formatTokens(p.v.RegInput), formatTokens(p.v.RegOutput))
		}
	}
	// I-591: estimate calibration section.
	if t.EstimatedItems > 0 {
		avgEst := t.TotalEstimatedHrs / float64(t.EstimatedItems)
		avgAct := t.TotalActualHrs / float64(t.EstimatedItems)
		fmt.Fprintf(w, "  Estimate calibration: %d items  avg estimate %.1fh  avg actual %.1fh\n",
			t.EstimatedItems, avgEst, avgAct)
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
		// I-569 step 5: synthetic cost computed on demand from real_tokens.
		// Falls back to legacy ai_cost_usd for items not yet migrated by
		// step 8 — once the migration runs, that fallback returns 0.
		cost := EstimateItemCostUSD(item)
		if cost == 0 {
			cost = readFloatField(item, "time_tracking", "ai_cost_usd")
		}
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
		out.TotalReasoning += readIntField(item, "time_tracking", "reasoning_tokens")
		out.TotalTokens += readIntField(item, "time_tracking", "total_tokens")
		out.TotalCacheIn += readIntField(item, "time_tracking", "cache_in_tokens")
		out.TotalCacheOut += readIntField(item, "time_tracking", "cache_out_tokens")
		out.TotalCacheOut1h += readIntField(item, "time_tracking", "cache_out_1h_tokens")
		out.UnknownCostTurns += readIntField(item, "time_tracking", "unknown_cost_turns")
		out.TotalLinesAdded += readIntField(item, "time_tracking", "lines_added")
		out.TotalLinesRemoved += readIntField(item, "time_tracking", "lines_removed")
		out.TotalFilesChanged += readIntField(item, "time_tracking", "files_changed_count")

		// I-591: calibration — closed items with both estimated_hours and total_duration_seconds.
		if item.Status == "done" || item.Status == "archived" {
			estHrs := readFloatField(item, "time_tracking", "estimated_hours")
			totalSec := readIntField(item, "time_tracking", "total_duration_seconds")
			if estHrs > 0 && totalSec > 0 {
				out.EstimatedItems++
				out.TotalEstimatedHrs += estHrs
				out.TotalActualHrs += float64(totalSec) / 3600
			}
		}

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
				prev.UnknownCostTurns += agg.UnknownCostTurns
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
		// I-406: severity bucket removed.
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

		// I-406: priority is the unified urgency signal across both
		// types. Bucket open issues + queued tasks by priority (p0-p4).
		// Items missing priority bucket as p2 (medium).
		if (item.Type == "issue" && item.Status == "queued") ||
			(item.Type == "task" && isQueuedTask(item, cfg)) {
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
