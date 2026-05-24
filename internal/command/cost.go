package command

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// CostOpts controls the cost rollup.
type CostOpts struct {
	// Since filters by item.LastTouched ≥ Since (zero value = no filter).
	Since time.Time
	// ItemID restricts the rollup to a single item.
	ItemID string
	// Agent restricts to items assigned to this agent (e.g. "agent-g").
	Agent string
	// All includes archived items in the rollup (default: open only).
	All bool
}

// Cost prints a per-item cost summary plus a total.
//
// Cost per item is computed from the item's accumulated time_tracking.real_tokens
// × current pricing rates via EstimateItemCostUSD. Items with no real_tokens
// (never logged a turn) are skipped. The output is informational; the
// underlying number is a SYNTHETIC ESTIMATE (see SyntheticCostLabel) — on Max
// plan there is no per-call billing to compare against.
func Cost(s *store.Store, cfg *config.Config, opts CostOpts, out io.Writer) int {
	if cfg == nil {
		fmt.Fprintln(out, "cost: no config")
		return 1
	}

	items := s.All()
	rows := make([]costRow, 0, len(items))
	var total float64
	for _, item := range items {
		if !includeItem(item, opts) {
			continue
		}
		usd := EstimateItemCostUSD(item)
		if usd == 0 {
			continue
		}
		rows = append(rows, costRow{
			ID:       item.ID,
			Title:    item.Title,
			Model:    readItemString(item, "time_tracking.last_model"),
			Assigned: readItemString(item, "assigned_to"),
			Status:   item.Status,
			USD:      usd,
		})
		total += usd
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "cost: no items with logged token usage in scope")
		return 0
	}

	// Sort descending by cost — biggest spenders first.
	sort.Slice(rows, func(i, j int) bool { return rows[i].USD > rows[j].USD })

	fmt.Fprintf(out, "%-8s  %-9s  %-22s  %-10s  %s\n", "ITEM", "USD", "LAST MODEL", "ASSIGNED", "TITLE")
	for _, r := range rows {
		fmt.Fprintf(out, "%-8s  $%8.4f  %-22s  %-10s  %s\n",
			r.ID, r.USD, truncateRow(r.Model, 22), truncateRow(r.Assigned, 10), truncateRow(r.Title, 60))
	}
	fmt.Fprintln(out, strings.Repeat("─", 80))
	fmt.Fprintf(out, "TOTAL: $%.4f across %d item(s) — %s\n", total, len(rows), SyntheticCostLabel)
	return 0
}

type costRow struct {
	ID       string
	Title    string
	Model    string
	Assigned string
	Status   string
	USD      float64
}

func includeItem(item *model.Item, opts CostOpts) bool {
	if item == nil {
		return false
	}
	if opts.ItemID != "" && item.ID != opts.ItemID {
		return false
	}
	if opts.Agent != "" && readItemString(item, "assigned_to") != opts.Agent {
		return false
	}
	if !opts.All && item.Status == "archived" {
		return false
	}
	if !opts.Since.IsZero() {
		// item.LastTouched may be zero on legacy items; in that case keep them
		// (fail toward showing).
		if !item.LastTouched.IsZero() && item.LastTouched.Before(opts.Since) {
			return false
		}
	}
	return true
}

// truncateRow caps a display column width to n bytes while staying valid
// UTF-8. Item titles routinely contain em-dashes (3 bytes) and other
// multi-byte runes — a naive byte cut mid-codepoint produces invalid UTF-8
// (terminal junk, JSON encoder panics).
func truncateRow(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	cut := n - 1
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
