package command

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// StatsMetaOpts holds flags for `st stats meta`.
type StatsMetaOpts struct {
	Agent string // filter to one agent id; "self" resolves via cfg.AgentID()
	Since string // duration window like "7d", "24h"; empty = no filter
	By    string // "agent" (default) or "reason"
	JSON  bool
}

// orphanLogEntry mirrors the JSON line shape written by writeOrphanLog
// (session_log.go). Each line of .as/sessions/orphan.log is one record.
type orphanLogEntry struct {
	At      string            `json:"at"`
	AgentID string            `json:"agent_id,omitempty"`
	Reason  string            `json:"reason"`
	Payload SessionLogPayload `json:"payload"`
}

// metaRow is one row of the rendered output. Aggregates across one bucket
// (agent or reason).
type metaRow struct {
	Key            string  `json:"key"`             // agent id or reason
	Turns          int     `json:"turns"`
	ProcessSeconds int64   `json:"process_seconds"`
	AISeconds      int64   `json:"ai_seconds"`
	CostUSD        float64 `json:"cost_usd"`
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	UnknownCost    int     `json:"unknown_cost_turns,omitempty"`
}

// metaReport is the full machine-readable output (--json).
type metaReport struct {
	GroupBy   string    `json:"group_by"`
	Since     string    `json:"since,omitempty"`
	AgentFilter string  `json:"agent_filter,omitempty"`
	Total     metaRow   `json:"total"`
	Rows      []metaRow `json:"rows"`
}

// StatsMeta reads .as/sessions/orphan.log, applies filters from opts,
// aggregates by agent (default) or reason (--by reason), and prints the
// result. Missing log file is not an error — emits "no meta-work
// recorded" (text) or an empty report (JSON).
//
// Per the I-369 (Option C) decision, when subagent metrics are
// RollupItemID-targeted to a parent item that EXISTS, they don't land
// here — they accrue on the parent's time_tracking. They DO land here
// when (a) no item id is resolvable at all, or (b) the resolved item id
// can't be found in the store (item-not-found fallback). So the
// "meta-work" surface is between-item deliberation plus orphaned
// subagent turns whose target item has since been archived/renamed.
func StatsMeta(cfg *config.Config, opts StatsMetaOpts) int {
	// Validate flags up front so a bad --since/--by surfaces a usage error
	// even when there's no orphan.log to read yet.
	cutoff, err := resolveSinceCutoff(opts.Since, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats meta: invalid --since %q: %v\n", opts.Since, err)
		return 2
	}
	groupBy := strings.ToLower(strings.TrimSpace(opts.By))
	if groupBy == "" {
		groupBy = "agent"
	}
	if groupBy != "agent" && groupBy != "reason" {
		fmt.Fprintf(os.Stderr, "stats meta: invalid --by %q (want 'agent' or 'reason')\n", opts.By)
		return 2
	}
	agentFilter := resolveAgentFilter(opts.Agent, cfg)

	logPath := filepath.Join(cfg.SessionsDir(), "orphan.log")
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyReport(opts, groupBy)
		}
		fmt.Fprintf(os.Stderr, "stats meta: opening %s: %v\n", logPath, err)
		return 1
	}
	defer f.Close()

	entries, err := parseOrphanLog(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats meta: %v\n", err)
		return 1
	}

	report := buildMetaReport(entries, cutoff, agentFilter, groupBy)
	report.Since = opts.Since
	report.AgentFilter = agentFilter

	if opts.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}

	if report.Total.Turns == 0 {
		fmt.Println("no meta-work recorded")
		return 0
	}

	renderMetaText(report, os.Stdout, opts.Since)
	return 0
}

// emptyReport prints the "no meta-work recorded" surface (text) or an
// empty report (JSON). The caller passes the resolved groupBy so the
// JSON shape stays consistent with what a populated run would emit
// (e.g. --by reason --json on an empty log returns group_by:"reason",
// not the default).
func emptyReport(opts StatsMetaOpts, groupBy string) int {
	if opts.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(metaReport{GroupBy: groupBy, Rows: []metaRow{}})
		return 0
	}
	fmt.Println("no meta-work recorded")
	return 0
}

// resolveAgentFilter expands "self" to cfg.AgentID(); empty/blank stays
// empty (no filter).
func resolveAgentFilter(raw string, cfg *config.Config) string {
	a := strings.TrimSpace(raw)
	if a == "" {
		return ""
	}
	if a == "self" {
		return cfg.AgentID()
	}
	return a
}

// resolveSinceCutoff parses "7d", "24h", "30m" etc. into an absolute
// cutoff. Empty input returns zero-value time (no filter).
func resolveSinceCutoff(raw string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	d, err := parseDurationFlexible(s)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-d), nil
}

// parseDurationFlexible accepts time.ParseDuration's grammar (ns/us/ms/s/m/h)
// plus a "<n>d" prefix for days, since CLI users expect "7d" and "1d12h".
// "1d12h" is rewritten to "36h" before handing to time.ParseDuration so
// mixed days+hours expressions parse cleanly.
func parseDurationFlexible(s string) (time.Duration, error) {
	// Find a "<digits>d" prefix at the start of the string. If found,
	// convert it to hours and prepend the rest.
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end > 0 && end < len(s) && s[end] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s[:end], "%d", &days); err != nil || days < 0 {
			return 0, fmt.Errorf("could not parse leading %q as Nd days", s[:end+1])
		}
		rest := s[end+1:]
		expanded := fmt.Sprintf("%dh%s", days*24, rest)
		return time.ParseDuration(expanded)
	}
	return time.ParseDuration(s)
}

// parseOrphanLog reads JSON lines from r. Malformed lines are skipped
// with a warning to stderr (one bad line shouldn't poison the whole
// query).
func parseOrphanLog(r io.Reader) ([]orphanLogEntry, error) {
	var out []orphanLogEntry
	sc := bufio.NewScanner(r)
	// Realistic orphan-log entries (a SessionLogPayload + a few headers)
	// are well under 64KB. Bump the buffer to 1MB anyway as a defensive
	// guardrail in case SessionLogPayload grows new fields or a producer
	// concatenates output unexpectedly — at the limit we still skip the
	// over-long line cleanly via sc.Err() below rather than aborting.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var e orphanLogEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			fmt.Fprintf(os.Stderr, "stats meta: skipping malformed line %d: %v\n", lineNo, err)
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning orphan.log: %w", err)
	}
	return out, nil
}

// buildMetaReport applies filters and aggregates entries into the report
// bucket-by-bucket. Pure function over entries — easy to unit test.
func buildMetaReport(entries []orphanLogEntry, cutoff time.Time, agentFilter, groupBy string) metaReport {
	buckets := map[string]*metaRow{}
	var total metaRow

	for _, e := range entries {
		if !cutoff.IsZero() {
			at, err := time.Parse(time.RFC3339, e.At)
			if err != nil || at.Before(cutoff) {
				continue
			}
		}
		if agentFilter != "" && e.AgentID != agentFilter {
			continue
		}
		key := e.AgentID
		if groupBy == "reason" {
			key = e.Reason
		}
		if key == "" {
			key = "unknown"
		}

		row, ok := buckets[key]
		if !ok {
			row = &metaRow{Key: key}
			buckets[key] = row
		}
		inTok := e.Payload.RegInputTokens + e.Payload.CacheInTokens +
			e.Payload.CacheOutTokens + e.Payload.CacheOut1hTokens
		outTok := e.Payload.RegOutputTokens

		row.Turns++
		row.ProcessSeconds += e.Payload.ProcessMs / 1000
		row.AISeconds += e.Payload.AIMs / 1000
		row.CostUSD += e.Payload.CostUSD
		row.InputTokens += inTok
		row.OutputTokens += outTok
		if e.Payload.CostSource == CostSourceUnknown {
			row.UnknownCost++
		}

		total.Turns++
		total.ProcessSeconds += e.Payload.ProcessMs / 1000
		total.AISeconds += e.Payload.AIMs / 1000
		total.CostUSD += e.Payload.CostUSD
		total.InputTokens += inTok
		total.OutputTokens += outTok
		if e.Payload.CostSource == CostSourceUnknown {
			total.UnknownCost++
		}
	}
	total.Key = "total"

	rows := make([]metaRow, 0, len(buckets))
	for _, r := range buckets {
		rows = append(rows, *r)
	}
	// Stable order: highest cost first, then key for tie-break.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].Key < rows[j].Key
	})

	return metaReport{
		GroupBy: groupBy,
		Total:   total,
		Rows:    rows,
	}
}

// renderMetaText writes the human-readable form to w.
//
//	Meta-work, last 7d (by agent):
//	  agent-a: 4h 12m | $1.83 | 87 turns
//	  agent-b: 2h 48m | $1.21 | 56 turns
func renderMetaText(r metaReport, w io.Writer, since string) {
	header := "Meta-work"
	if since != "" {
		header += ", last " + since
	}
	header += fmt.Sprintf(" (by %s):", r.GroupBy)
	fmt.Fprintln(w, header)

	// Column-friendly: pad the key to the longest seen.
	width := len("total")
	for _, row := range r.Rows {
		if len(row.Key) > width {
			width = len(row.Key)
		}
	}
	for _, row := range r.Rows {
		fmt.Fprintf(w, "  %-*s  %s | %s | %d turns",
			width, row.Key,
			formatDuration(time.Duration(row.ProcessSeconds)*time.Second),
			fmt.Sprintf("$%.2f", row.CostUSD),
			row.Turns)
		if row.UnknownCost > 0 {
			fmt.Fprintf(w, "  (%d missing cost)", row.UnknownCost)
		}
		fmt.Fprintln(w)
	}
	if len(r.Rows) > 1 {
		fmt.Fprintf(w, "  %-*s  %s | %s | %d turns\n",
			width, "total",
			formatDuration(time.Duration(r.Total.ProcessSeconds)*time.Second),
			fmt.Sprintf("$%.2f", r.Total.CostUSD),
			r.Total.Turns)
	}
}
