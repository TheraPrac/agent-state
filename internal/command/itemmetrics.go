package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
)

// ItemMetrics is the rolled-up per-item view of time/cost/tokens/LOC used
// for the one-line summary surfaces — st status's active-work/recent rows,
// statusSingle's "Metrics:" line, and the per-item rows of st run status.
// Both surfaces extract through ExtractItemMetrics so the summary lines
// can't drift. (st show's renderTimeTracking is a deliberately richer
// detailed-block view that surfaces fields ItemMetrics doesn't carry —
// turn_count, by_provider_model, files_changed_count, etc. — and stays
// independent on purpose.) Fields stay zero when the underlying data is
// absent; callers use HasMetrics to decide whether to render anything.
type ItemMetrics struct {
	Wall         time.Duration
	ProcessTime  time.Duration
	AITime       time.Duration
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	NetLOC       int
}

// ExtractItemMetrics reads time_tracking + the PR manifest and returns a
// rolled-up ItemMetrics. `now` is used to compute wall time on open items
// (closed items use completed_at - started_at). `manifestDir` is consulted
// for net LOC; pass an empty string to skip manifest reads.
//
// Tolerant of typed fields (TimeTracking map[string]interface{}) and the
// stringly-typed nested form readIntField/readFloatField walk; matches the
// historical RunStatus extraction behavior.
func ExtractItemMetrics(item *model.Item, manifestDir string, now time.Time, isDone bool) ItemMetrics {
	if item == nil {
		return ItemMetrics{}
	}
	var m ItemMetrics

	// Wall time — completed - started for closed items, now - started for open
	if tt := item.TimeTracking; tt != nil {
		startedStr := stringField(tt, "started_at")
		if startedStr != "" {
			if started, err := time.Parse(time.RFC3339, startedStr); err == nil {
				if isDone {
					if completedStr := stringField(tt, "completed_at"); completedStr != "" {
						if completed, err := time.Parse(time.RFC3339, completedStr); err == nil {
							m.Wall = completed.Sub(started)
						}
					}
				} else {
					m.Wall = now.Sub(started)
				}
			}
		}
	}

	// ProcessTime: prefer process_time_seconds; fall back to legacy run_wall_seconds
	if secs := readSecondsField(item.TimeTracking, "process_time_seconds", "run_wall_seconds"); secs > 0 {
		m.ProcessTime = time.Duration(secs * float64(time.Second))
	}

	// AITime: prefer ai_time_seconds; fall back to ai_duration_seconds
	if secs := readSecondsField(item.TimeTracking, "ai_time_seconds", "ai_duration_seconds"); secs > 0 {
		m.AITime = time.Duration(secs * float64(time.Second))
	}

	// Cost — typed map first; fall through to doc walker for partially-
	// populated items (matches the same dual-path pattern as tokens below).
	if tt := item.TimeTracking; tt != nil {
		m.CostUSD = floatField(tt, "ai_cost_usd")
	}
	if m.CostUSD == 0 {
		m.CostUSD = readFloatField(item, "time_tracking", "ai_cost_usd")
	}

	// Tokens — typed TimeTracking map is the canonical source after parse;
	// fall back to the doc walker (readIntField) for partially-populated
	// items, then the legacy input_tokens/output_tokens fields.
	m.InputTokens = intField(item.TimeTracking, "total_input_tokens")
	if m.InputTokens == 0 {
		m.InputTokens = readIntField(item, "time_tracking", "total_input_tokens")
	}
	if m.InputTokens == 0 {
		m.InputTokens = intField(item.TimeTracking, "reg_input_tokens") +
			intField(item.TimeTracking, "cache_in_tokens") +
			intField(item.TimeTracking, "cache_out_tokens") +
			intField(item.TimeTracking, "cache_out_1h_tokens")
	}
	if m.InputTokens == 0 {
		m.InputTokens = readIntField(item, "time_tracking", "reg_input_tokens") +
			readIntField(item, "time_tracking", "cache_in_tokens") +
			readIntField(item, "time_tracking", "cache_out_tokens") +
			readIntField(item, "time_tracking", "cache_out_1h_tokens")
	}
	if m.InputTokens == 0 {
		m.InputTokens = intField(item.TimeTracking, "input_tokens")
	}
	m.OutputTokens = intField(item.TimeTracking, "total_output_tokens")
	if m.OutputTokens == 0 {
		m.OutputTokens = readIntField(item, "time_tracking", "total_output_tokens")
	}
	if m.OutputTokens == 0 {
		m.OutputTokens = intField(item.TimeTracking, "reg_output_tokens")
	}
	if m.OutputTokens == 0 {
		m.OutputTokens = readIntField(item, "time_tracking", "reg_output_tokens")
	}
	if m.OutputTokens == 0 {
		m.OutputTokens = intField(item.TimeTracking, "output_tokens")
	}

	// Net LOC from PR manifest
	if manifestDir != "" {
		if mf, err := manifest.Load(manifestDir, item.ID); err == nil {
			for _, pr := range mf.PRs {
				m.NetLOC += pr.CodeStats.Insertions - pr.CodeStats.Deletions
			}
		}
	}

	return m
}

// HasMetrics reports whether at least one tracked field is non-zero.
// Callers use it to skip the metric line on items that haven't accrued
// any tracked work yet.
func (m ItemMetrics) HasMetrics() bool {
	return m.Wall > 0 || m.ProcessTime > 0 || m.AITime > 0 ||
		m.CostUSD > 0 || m.InputTokens > 0 || m.OutputTokens > 0 || m.NetLOC != 0
}

// FormatLine produces the inline single-line representation used by
// st status's per-item rows and the PIPELINE section. Format:
//
//	"<wall> | $<cost> | <files-or-tokens> <±LOC>"
//
// Only non-zero fields appear; the line is empty when HasMetrics() is false.
// LOC is shown when NetLOC != 0; tokens are shown when LOC is absent and
// either token bucket is non-zero.
func (m ItemMetrics) FormatLine() string {
	if !m.HasMetrics() {
		return ""
	}
	parts := make([]string, 0, 4)
	if m.Wall > 0 {
		parts = append(parts, formatDuration(m.Wall))
	} else if m.AITime > 0 {
		parts = append(parts, formatDuration(m.AITime))
	}
	if m.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", m.CostUSD))
	}
	switch {
	case m.NetLOC != 0:
		parts = append(parts, formatLOC(m.NetLOC))
	case m.InputTokens > 0 || m.OutputTokens > 0:
		parts = append(parts, fmt.Sprintf("%s/%s tok",
			formatTokens(m.InputTokens), formatTokens(m.OutputTokens)))
	}
	return strings.Join(parts, " | ")
}

// FormatColumns produces the tabular column strings used by RunStatus.
// Each field is rendered to its own string (or empty) so callers can pad
// to fixed widths. Order: wall, processTime, aiTime, cost, tokens (I/O/T),
// loc.
type MetricColumns struct {
	Wall        string
	ProcessTime string
	AITime      string
	Cost        string
	Tokens      string
	LOC         string
}

func (m ItemMetrics) FormatColumns() MetricColumns {
	out := MetricColumns{}
	if m.Wall > 0 {
		out.Wall = formatDuration(m.Wall)
	}
	if m.ProcessTime > 0 {
		out.ProcessTime = formatDuration(m.ProcessTime)
	}
	if m.AITime > 0 {
		out.AITime = formatDuration(m.AITime)
	}
	if m.CostUSD > 0 {
		out.Cost = fmt.Sprintf("$%.2f", m.CostUSD)
	}
	if m.InputTokens > 0 || m.OutputTokens > 0 {
		out.Tokens = fmt.Sprintf("%s/%s/%s",
			formatTokens(m.InputTokens),
			formatTokens(m.OutputTokens),
			formatTokens(m.InputTokens+m.OutputTokens))
	}
	if m.NetLOC != 0 {
		out.LOC = formatLOC(m.NetLOC)
	}
	return out
}

// Add merges other into m and returns the sum. Used by RunStatus to
// accumulate sprint and epic totals from per-item metrics.
func (m ItemMetrics) Add(other ItemMetrics) ItemMetrics {
	return ItemMetrics{
		Wall:         m.Wall + other.Wall,
		ProcessTime:  m.ProcessTime + other.ProcessTime,
		AITime:       m.AITime + other.AITime,
		CostUSD:      m.CostUSD + other.CostUSD,
		InputTokens:  m.InputTokens + other.InputTokens,
		OutputTokens: m.OutputTokens + other.OutputTokens,
		NetLOC:       m.NetLOC + other.NetLOC,
	}
}

// readSecondsField reads a numeric duration-in-seconds field from a typed
// time_tracking map, trying primary then fallback keys. Tolerates float64,
// int, and string-as-number representations (legacy items).
func readSecondsField(tt map[string]interface{}, primary, fallback string) float64 {
	if tt == nil {
		return 0
	}
	for _, key := range []string{primary, fallback} {
		raw, ok := tt[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case string:
			var f float64
			fmt.Sscanf(v, "%f", &f)
			return f
		}
	}
	return 0
}

func stringField(tt map[string]interface{}, key string) string {
	if tt == nil {
		return ""
	}
	if raw, ok := tt[key]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

func floatField(tt map[string]interface{}, key string) float64 {
	if tt == nil {
		return 0
	}
	raw, ok := tt[key]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	}
	return 0
}

func intField(tt map[string]interface{}, key string) int {
	if tt == nil {
		return 0
	}
	raw, ok := tt[key]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var i int
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}
