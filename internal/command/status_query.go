package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// filterSpec is one parsed `--filter key:value` flag instance.
//
// Recognized keys (rejected at parse time if not in this list):
//   - agent:<id>     matches assigned_to OR last_touched_by
//   - assigned:<id>  strict assigned_to match
//   - status:<s>     comma list of status values, e.g. "active,queued"
//   - type:<t>       "task" or "issue" (or comma list)
//   - tag:<name>
//   - priority:<n>   single or comma list, e.g. "1" or "1,2"
//   - epic:<id>
//   - sprint:<id>
type filterSpec struct {
	Key   string
	Value string
}

// sortSpec is the parsed `--sort field[,asc|desc]` flag.
//
// Recognized fields: cost, time, lines, last_touched, priority, id.
// Default direction is "desc" for cost/time/lines/last_touched (operators
// ask "biggest first"), "asc" for priority/id (operators ask "highest
// priority first" which is numerically lowest).
type sortSpec struct {
	Field string
	Desc  bool
}

var validFilterKeys = []string{
	"agent", "assigned", "status", "type", "tag", "priority", "epic", "sprint",
}

var validSortFields = []string{
	"cost", "time", "lines", "last_touched", "priority", "id",
}

// parseFilterSpecs parses a slice of "key:value" strings into filterSpec
// values. Returns an error listing the offending input + valid keys when
// any spec is malformed or uses an unknown key.
func parseFilterSpecs(raw []string) ([]filterSpec, error) {
	var out []filterSpec
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		idx := strings.Index(r, ":")
		if idx <= 0 || idx == len(r)-1 {
			return nil, fmt.Errorf("filter %q: expected 'key:value' (valid keys: %s)",
				r, strings.Join(validFilterKeys, ", "))
		}
		key := strings.ToLower(strings.TrimSpace(r[:idx]))
		val := strings.TrimSpace(r[idx+1:])
		if !contains(validFilterKeys, key) {
			return nil, fmt.Errorf("filter key %q unrecognized (valid: %s)",
				key, strings.Join(validFilterKeys, ", "))
		}
		out = append(out, filterSpec{Key: key, Value: val})
	}
	return out, nil
}

// parseSortSpec parses "field[,asc|desc]" into a sortSpec. Empty input
// returns a zero-value spec (caller treats as "no sort").
func parseSortSpec(raw string) (sortSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sortSpec{}, nil
	}
	parts := strings.Split(raw, ",")
	field := strings.ToLower(strings.TrimSpace(parts[0]))
	if !contains(validSortFields, field) {
		return sortSpec{}, fmt.Errorf("sort field %q unrecognized (valid: %s)",
			field, strings.Join(validSortFields, ", "))
	}
	// Default direction: desc for "biggest first" fields, asc for ids/priority.
	desc := false
	switch field {
	case "cost", "time", "lines", "last_touched":
		desc = true
	}
	if len(parts) > 1 {
		dir := strings.ToLower(strings.TrimSpace(parts[1]))
		switch dir {
		case "asc":
			desc = false
		case "desc":
			desc = true
		case "":
			// keep default
		default:
			return sortSpec{}, fmt.Errorf("sort direction %q unrecognized (want asc or desc)", dir)
		}
	}
	return sortSpec{Field: field, Desc: desc}, nil
}

// matchesFilters returns true when item passes ALL filters (composable AND).
// `since` (zero = no filter) drops items whose last_touched is older than the
// cutoff. Empty filter list + zero since = always true.
func matchesFilters(item *model.Item, filters []filterSpec, sinceCutoff time.Time) bool {
	if !sinceCutoff.IsZero() {
		if item.LastTouched.Before(sinceCutoff) {
			return false
		}
	}
	for _, f := range filters {
		if !matchOne(item, f) {
			return false
		}
	}
	return true
}

func matchOne(item *model.Item, f filterSpec) bool {
	switch f.Key {
	case "agent":
		// Match either assigned_to (current ownership) OR last_touched_by
		// (anyone who's recently touched it). Operator's "what's agent-b
		// involved in" question is loose by intent.
		want := stripAgentPrefix(f.Value)
		// Defensive: a malformed input like `agent:agent-` collapses to
		// empty after stripAgentPrefix, which would silently match every
		// item with empty assigned_to / last_touched_by. Reject that case
		// rather than acting as a wildcard.
		if want == "" {
			return false
		}
		assigned := stripAgentPrefix(itemField(item, "assigned_to"))
		toucher := stripAgentPrefix(item.LastTouchedBy)
		return assigned == want || toucher == want
	case "assigned":
		want := stripAgentPrefix(f.Value)
		if want == "" {
			return false
		}
		assigned := stripAgentPrefix(itemField(item, "assigned_to"))
		return assigned == want
	case "status":
		return matchInList(item.Status, f.Value)
	case "type":
		return matchInList(item.Type, f.Value)
	case "tag":
		for _, t := range item.Tags {
			if t == f.Value {
				return true
			}
		}
		return false
	case "priority":
		if item.Priority == nil {
			return false
		}
		return matchInList(strconv.Itoa(*item.Priority), f.Value)
	case "epic":
		return item.Epic == f.Value
	case "sprint":
		return item.Sprint == f.Value
	}
	return false
}

// matchInList: f.Value is "a,b,c"; returns true if value is in the list.
func matchInList(value, csv string) bool {
	for _, v := range strings.Split(csv, ",") {
		if strings.TrimSpace(v) == value {
			return true
		}
	}
	return false
}

// stripAgentPrefix normalizes "agent-b" / "b" to a common form so
// `--filter agent:b` matches both "agent-b" and "b".
func stripAgentPrefix(s string) string {
	s = strings.TrimSpace(s)
	return strings.TrimPrefix(s, "agent-")
}

// itemField pulls a string field from item.Doc when not exposed on the
// typed struct; used for `assigned_to` since that field isn't on
// model.Item directly in some code paths.
func itemField(item *model.Item, key string) string {
	if item == nil {
		return ""
	}
	if v, ok := item.Doc.GetField(key); ok {
		return v
	}
	return ""
}

// applyStatusQuery filters and sorts a list of items per the parsed
// specs. Deterministic over inputs but NOT pure: when sorting by metric
// fields (cost/time/lines), it routes through ExtractItemMetrics which
// reads the PR manifest from disk for net-LOC. Tests pass an empty
// manifestDir to keep the I/O off the unit-test path.
func applyStatusQuery(items []*model.Item, filters []filterSpec, ss sortSpec, sinceCutoff time.Time, cfg *config.Config, manifestDir string, now time.Time) []*model.Item {
	out := make([]*model.Item, 0, len(items))
	for _, it := range items {
		if matchesFilters(it, filters, sinceCutoff) {
			out = append(out, it)
		}
	}
	if ss.Field != "" {
		applySort(out, ss, cfg, manifestDir, now)
	}
	return out
}

func applySort(items []*model.Item, ss sortSpec, cfg *config.Config, manifestDir string, now time.Time) {
	// Cache metrics so we don't recompute per comparison call (sort.Slice
	// invokes less-than O(n log n) times).
	cache := make(map[string]ItemMetrics, len(items))
	for _, it := range items {
		isDone := false
		if cfg != nil {
			isDone = cfg.IsTerminalStatus(it.Type, it.Status)
		}
		cache[it.ID] = ExtractItemMetrics(it, manifestDir, now, isDone)
	}

	less := func(i, j int) bool {
		a, b := items[i], items[j]
		switch ss.Field {
		case "cost":
			return cache[a.ID].CostUSD < cache[b.ID].CostUSD
		case "time":
			return cache[a.ID].ProcessTime < cache[b.ID].ProcessTime
		case "lines":
			return cache[a.ID].NetLOC < cache[b.ID].NetLOC
		case "last_touched":
			return a.LastTouched.Before(b.LastTouched)
		case "priority":
			ap, bp := 999, 999
			if a.Priority != nil {
				ap = *a.Priority
			}
			if b.Priority != nil {
				bp = *b.Priority
			}
			if ap != bp {
				return ap < bp
			}
			return a.ID < b.ID
		case "id":
			return a.ID < b.ID
		}
		return false
	}
	if ss.Desc {
		base := less
		less = func(i, j int) bool { return base(j, i) }
	}
	sort.Slice(items, less)
}

// statusJSONItem is the per-item shape emitted by `st status --json`.
// Carries every metric the text view shows so jq pipelines have parity
// with what an operator sees in the terminal.
type statusJSONItem struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Status        string         `json:"status"`
	Title         string         `json:"title"`
	AssignedTo    string         `json:"assigned_to,omitempty"`
	LastTouched   string         `json:"last_touched,omitempty"`
	LastTouchedBy string         `json:"last_touched_by,omitempty"`
	Priority      *int           `json:"priority,omitempty"`
	// I-406: Severity removed; Priority is the unified urgency signal.
	Tags          []string       `json:"tags,omitempty"`
	Epic          string         `json:"epic,omitempty"`
	Sprint        string         `json:"sprint,omitempty"`
	Stage         string         `json:"delivery_stage,omitempty"`
	Metrics       statusJSONMetrics `json:"metrics"`
}

type statusJSONMetrics struct {
	WallSeconds         int64   `json:"wall_seconds"`
	ProcessTimeSeconds  int64   `json:"process_time_seconds"`
	AITimeSeconds       int64   `json:"ai_time_seconds"`
	CostUSD             float64 `json:"cost_usd"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheWriteTokens    int     `json:"cache_write_tokens"`
	Model               string  `json:"model,omitempty"`
	NetLOC              int     `json:"net_loc"`
}

// statusJSON emits a flat list of items matching the query, with metrics
// inlined per item. JSON consumers (jq, scripts) get the same data the
// text dashboard renders, no separate metric dump needed.
//
// Default scope matches the text dashboard's active-first bias: when the
// caller doesn't pass a `status:` filter, terminal statuses (done,
// abandoned, archived per the I-433 unified vocabulary) are excluded so
// a casual `st status --json` doesn't dump hundreds of archive entries.
// Operators who want the archive can opt in via `--filter status:archived`
// or equivalent.
func statusJSON(s storeForQuery, cfg *config.Config, filters []filterSpec, ss sortSpec, sinceCutoff time.Time) int {
	allMap := s.All()
	all := make([]*model.Item, 0, len(allMap))
	for _, item := range allMap {
		all = append(all, item)
	}

	// Apply the default non-terminal narrowing only when the caller
	// didn't already specify a status filter.
	if !hasFilterKey(filters, "status") {
		nonTerminal := make([]*model.Item, 0, len(all))
		for _, it := range all {
			if !cfg.IsTerminalStatus(it.Type, it.Status) {
				nonTerminal = append(nonTerminal, it)
			}
		}
		all = nonTerminal
	}

	now := time.Now()
	filtered := applyStatusQuery(all, filters, ss, sinceCutoff, cfg, cfg.ManifestDir(), now)

	out := make([]statusJSONItem, 0, len(filtered))
	for _, item := range filtered {
		isDone := cfg.IsTerminalStatus(item.Type, item.Status)
		m := ExtractItemMetrics(item, cfg.ManifestDir(), now, isDone)
		stage, _ := getNestedField(item, "delivery", "stage")
		row := statusJSONItem{
			ID:            item.ID,
			Type:          item.Type,
			Status:        item.Status,
			Title:         item.Title,
			AssignedTo:    itemField(item, "assigned_to"),
			LastTouched:   item.LastTouched.Format(time.RFC3339),
			LastTouchedBy: item.LastTouchedBy,
			Priority:      item.Priority,
			Tags:          item.Tags,
			Epic:          item.Epic,
			Sprint:        item.Sprint,
			Stage:         stage,
			Metrics: statusJSONMetrics{
				WallSeconds:        int64(m.Wall.Seconds()),
				ProcessTimeSeconds: int64(m.ProcessTime.Seconds()),
				AITimeSeconds:      int64(m.AITime.Seconds()),
				CostUSD:            m.CostUSD,
				InputTokens:        m.InputTokens,
				OutputTokens:       m.OutputTokens,
				CacheReadTokens:    m.CacheReadTokens,
				CacheWriteTokens:   m.CacheWriteTokens,
				Model:              m.Model,
				NetLOC:             m.NetLOC,
			},
		}
		out = append(out, row)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "status: json encode: %v\n", err)
		return 1
	}
	return 0
}

// storeForQuery is the narrow interface statusJSON needs from store.Store
// (just All()). store.Store.All() returns map[string]*model.Item; the
// caller flattens to a slice before passing to applyStatusQuery.
type storeForQuery interface {
	All() map[string]*model.Item
}

// hasFilterKey reports whether any filter spec has the given key. Used
// to decide whether to apply default narrowing (e.g. exclude terminal
// statuses when no `status:` filter is set).
func hasFilterKey(filters []filterSpec, key string) bool {
	for _, f := range filters {
		if f.Key == key {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
