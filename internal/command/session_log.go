package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/pricing"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SessionLogPayload is the per-turn metrics payload the Claude Code Stop hook
// (and rewired st run) sends to `st session log`. All token and duration fields
// are for the single turn; the command accrues them onto the resolved item.
type SessionLogPayload struct {
	SessionID       string `json:"session_id"`
	Model           string `json:"model"`
	ProcessMs       int64  `json:"process_ms"`
	AIMs            int64  `json:"ai_ms"`
	RegInputTokens  int    `json:"reg_input_tokens"`
	RegOutputTokens int    `json:"reg_output_tokens"`
	CacheInTokens   int    `json:"cache_in_tokens"`
	// CacheOutTokens is the 5-minute cache write bucket (1.25x input rate).
	// Existing producers that don't split by tier should send their total
	// here; it's treated as all-5m and priced at 1.25x.
	CacheOutTokens int `json:"cache_out_tokens"`
	// CacheOut1hTokens is the 1-hour cache write bucket (2x input rate).
	// When the producer can split by tier (Stop hook parses
	// ephemeral_5m/1h_input_tokens), populate this field; pricing applies
	// the 2x rate. Zero is safe — older producers still work.
	CacheOut1hTokens int     `json:"cache_out_1h_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"` // if 0, computed from tokens × pricing
	Turn             int     `json:"turn,omitempty"`     // ordinal within session; informational
	ItemID           string  `json:"item_id,omitempty"`  // if empty, resolved from stack top
	Step             string  `json:"step,omitempty"`     // default "interactive"

	// Optional per-turn file diff info (populated by Stop hook once live).
	// Recorded in the ai_turns line for provenance.
	Files        int `json:"files,omitempty"`
	LinesAdded   int `json:"lines_added,omitempty"`
	LinesRemoved int `json:"lines_removed,omitempty"`
}

// SessionLogCLI reads a JSON payload from stdin and applies it.
// Exit codes: 0 success (including orphan case), 1 error, 2 usage.
func SessionLogCLI(s *store.Store, cfg *config.Config, stdin io.Reader) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session log: reading stdin: %v\n", err)
		return 1
	}
	if len(raw) == 0 {
		fmt.Fprintln(os.Stderr, "session log: empty payload on stdin")
		return 2
	}
	var payload SessionLogPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Fprintf(os.Stderr, "session log: invalid JSON: %v\n", err)
		return 1
	}
	return SessionLog(s, cfg, payload)
}

// SessionLog applies a per-turn metrics payload to the resolved item.
// If payload.ItemID is empty, the stack top is used. If the stack is empty,
// the payload is appended to the orphan log and 0 is returned (never dropped).
//
// This is the ONE accumulator both producers (Claude Code Stop hook and
// st run's recordRunMetrics) call. Schema lives in the item's time_tracking
// block; per-turn provenance goes to work_tracking.ai_turns.
func SessionLog(s *store.Store, cfg *config.Config, payload SessionLogPayload) int {
	// Resolve target item
	itemID := payload.ItemID
	if itemID == "" {
		entries := LoadStack(cfg)
		if len(entries) > 0 {
			itemID = entries[len(entries)-1].ID
		}
	}
	if itemID == "" {
		if err := writeOrphanLog(cfg, payload); err != nil {
			fmt.Fprintf(os.Stderr, "session log: writing orphan log: %v\n", err)
			return 1
		}
		return 0
	}

	item, ok := s.Get(itemID)
	if !ok {
		// Item not found — treat as orphan rather than error; the alternative
		// is silent data loss.
		if err := writeOrphanLog(cfg, payload); err != nil {
			fmt.Fprintf(os.Stderr, "session log: writing orphan log: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "session log: item %s not found — logged to orphan.log\n", itemID)
		return 0
	}

	// Compute cost if not provided. Unknown model is surfaced as a warning to
	// stderr; we still record token counts so operators can backfill later.
	cost := payload.CostUSD
	if cost == 0 && payload.Model != "" && hasTokens(payload) {
		computed, err := pricing.ComputeCost(
			payload.Model,
			payload.RegInputTokens, payload.RegOutputTokens,
			payload.CacheInTokens, payload.CacheOutTokens, payload.CacheOut1hTokens,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "session log: %v (tokens recorded without cost)\n", err)
		} else {
			cost = computed
		}
	}

	// Accrue aggregate fields
	setNestedField(item, "time_tracking", "process_time_seconds",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "process_time_seconds")+int(payload.ProcessMs/1000)))
	setNestedField(item, "time_tracking", "ai_time_seconds",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "ai_time_seconds")+int(payload.AIMs/1000)))

	setNestedField(item, "time_tracking", "reg_input_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_input_tokens")+payload.RegInputTokens))
	setNestedField(item, "time_tracking", "reg_output_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_output_tokens")+payload.RegOutputTokens))
	setNestedField(item, "time_tracking", "cache_in_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_in_tokens")+payload.CacheInTokens))
	setNestedField(item, "time_tracking", "cache_out_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_out_tokens")+payload.CacheOutTokens))
	if payload.CacheOut1hTokens > 0 || readIntField(item, "time_tracking", "cache_out_1h_tokens") > 0 {
		setNestedField(item, "time_tracking", "cache_out_1h_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_out_1h_tokens")+payload.CacheOut1hTokens))
	}

	// Derived totals — kept in the file so consumers don't need to compute
	setNestedField(item, "time_tracking", "total_input_tokens",
		fmt.Sprintf("%d",
			readIntField(item, "time_tracking", "reg_input_tokens")+
				readIntField(item, "time_tracking", "cache_in_tokens")+
				readIntField(item, "time_tracking", "cache_out_tokens")+
				readIntField(item, "time_tracking", "cache_out_1h_tokens")))
	setNestedField(item, "time_tracking", "total_output_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_output_tokens")))

	if cost > 0 {
		// 6-decimal precision matches by_model cost precision so the two
		// aggregates don't drift apart across round-trips. See formatByModelLine.
		setNestedField(item, "time_tracking", "ai_cost_usd",
			fmt.Sprintf("%.6f", readFloatField(item, "time_tracking", "ai_cost_usd")+cost))
	}

	setNestedField(item, "time_tracking", "turn_count",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "turn_count")+1))

	// session_count: recompute from distinct session ids in ai_turns (walk-based
	// for correctness; list is typically small). A new session_id triggers +1.
	// An empty SessionID is bucketed as "unknown" so the invariant
	// `session_count >= 1 whenever turn_count >= 1` always holds even if the
	// Claude envelope fails to provide a session_id.
	sid := payload.SessionID
	if sid == "" {
		sid = "unknown"
	}
	seen := seenSessionIDs(item)
	if !seen[sid] {
		setNestedField(item, "time_tracking", "session_count",
			fmt.Sprintf("%d", len(seen)+1))
	}

	// Bookkeeping
	if payload.SessionID != "" {
		setNestedField(item, "time_tracking", "last_session", payload.SessionID)
	}
	if payload.Model != "" {
		setNestedField(item, "time_tracking", "last_model", payload.Model)
	}
	now := time.Now().Format(time.RFC3339)
	setNestedField(item, "time_tracking", "last_touched", now)
	toucher := cfg.AgentID()
	if toucher == "" {
		toucher = "stop-hook"
	}
	setNestedField(item, "time_tracking", "last_touched_by", toucher)

	// Append per-turn provenance line
	appendListField(item, "time_tracking", "ai_turns", formatAITurnLine(payload, cost, now))

	// Upsert per-model aggregate (one line per model under work_tracking.by_model)
	if payload.Model != "" {
		upsertByModel(item, payload, cost)
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "session log: writing %s: %v\n", itemID, err)
		return 1
	}
	return 0
}

// byModelAggregate captures per-model running totals. Values are parsed from
// and written to the work_tracking.by_model line list, one line per model.
type byModelAggregate struct {
	Turns    int
	RegIn    int
	RegOut   int
	CacheIn  int
	CacheOut int
	Cost     float64
}

// upsertByModel finds the existing line for payload.Model under
// work_tracking.by_model (if any), adds the payload's deltas, and writes the
// updated line back. If no line exists, it appends a new one.
func upsertByModel(item *model.Item, p SessionLogPayload, cost float64) {
	existing := readByModel(item, p.Model)
	existing.Turns++
	existing.RegIn += p.RegInputTokens
	existing.RegOut += p.RegOutputTokens
	existing.CacheIn += p.CacheInTokens
	existing.CacheOut += p.CacheOutTokens + p.CacheOut1hTokens // aggregate total cache writes per model
	existing.Cost += cost

	line := formatByModelLine(p.Model, existing)

	// Try to update in place; if not found, append.
	if !updateListLine(item, "time_tracking", "by_model",
		func(raw string) bool { return byModelLineMatches(raw, p.Model) },
		line) {
		appendListField(item, "time_tracking", "by_model", line)
	}
}

// formatByModelLine produces a stable, grep-friendly representation.
// Format: "<model>: turns=N reg_in=N reg_out=N cache_in=N cache_out=N cost=$N.NNNNNN"
// 6-decimal cost precision keeps round-trip accumulation drift under $0.000001
// per turn — safe even across thousands of turns on cheap models.
func formatByModelLine(model string, a byModelAggregate) string {
	return fmt.Sprintf("%s: turns=%d reg_in=%d reg_out=%d cache_in=%d cache_out=%d cost=$%.6f",
		model, a.Turns, a.RegIn, a.RegOut, a.CacheIn, a.CacheOut, a.Cost)
}

// readByModel walks time_tracking.by_model and returns the aggregate for model,
// or the zero value if not present.
func readByModel(item *model.Item, modelID string) byModelAggregate {
	var out byModelAggregate
	if item == nil || item.Doc == nil {
		return out
	}
	inWT := false
	inBlock := false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inWT = line.Key == "time_tracking"
			inBlock = false
			continue
		}
		if !inWT {
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
		trimmed := strings.TrimSpace(line.Raw)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		entry := strings.TrimPrefix(trimmed, "- ")
		if !byModelLineMatches(entry, modelID) {
			continue
		}
		out = parseByModelLine(entry)
		return out
	}
	return out
}

// byModelLineMatches returns true if the by_model list entry (already stripped
// of the "- " prefix, or still with it) starts with "<model>:".
func byModelLineMatches(raw, model string) bool {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "- ")
	// Up to the first colon is the model id
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		return trimmed[:idx] == model
	}
	return false
}

// parseByModelLine parses a "model: turns=N reg_in=N ..." line back into an
// aggregate. Missing fields stay at zero.
func parseByModelLine(entry string) byModelAggregate {
	var a byModelAggregate
	colon := strings.Index(entry, ":")
	if colon < 0 {
		return a
	}
	rest := strings.TrimSpace(entry[colon+1:])
	for _, tok := range strings.Fields(rest) {
		eq := strings.Index(tok, "=")
		if eq < 0 {
			continue
		}
		key := tok[:eq]
		val := tok[eq+1:]
		switch key {
		case "turns":
			fmt.Sscanf(val, "%d", &a.Turns)
		case "reg_in":
			fmt.Sscanf(val, "%d", &a.RegIn)
		case "reg_out":
			fmt.Sscanf(val, "%d", &a.RegOut)
		case "cache_in":
			fmt.Sscanf(val, "%d", &a.CacheIn)
		case "cache_out":
			fmt.Sscanf(val, "%d", &a.CacheOut)
		case "cost":
			v := strings.TrimPrefix(val, "$")
			fmt.Sscanf(v, "%f", &a.Cost)
		}
	}
	return a
}

// updateListLine finds the first list entry under parent.key whose raw payload
// (after the "- " prefix) is matched by `match`, and replaces it with newVal.
// Returns true if a line was updated. Format written: "  - <newVal>".
func updateListLine(item *model.Item, parent, key string, match func(raw string) bool, newVal string) bool {
	if item == nil || item.Doc == nil {
		return false
	}
	parentIdx := -1
	keyIdx := -1
	for i, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key == parent {
			parentIdx = i
			continue
		}
		if parentIdx < 0 {
			continue
		}
		if line.Indent == 0 && line.Key != "" && line.Key != parent {
			break
		}
		if line.Indent == 2 && line.Key == key {
			keyIdx = i
			continue
		}
		if keyIdx < 0 {
			continue
		}
		if line.Indent < 4 && line.Key != "" && line.Key != key {
			break
		}
		trimmed := strings.TrimSpace(line.Raw)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		payload := strings.TrimPrefix(trimmed, "- ")
		if match(payload) {
			item.Doc.Lines[i].Raw = fmt.Sprintf("  - %s", newVal)
			return true
		}
	}
	return false
}

// formatAITurnLine produces the provenance line appended to time_tracking.ai_turns.
// Format is space-separated key:value pairs — grep-friendly and stable enough to
// be parsed by downstream reporting without a dedicated parser.
func formatAITurnLine(p SessionLogPayload, cost float64, at string) string {
	step := p.Step
	if step == "" {
		step = "interactive"
	}
	// Keep session id parseable — an empty string would produce "session: model:..."
	// which breaks the walker. Bucket as "unknown" (mirrors SessionLog's session_count).
	sid := p.SessionID
	if sid == "" {
		sid = "unknown"
	}
	var sb strings.Builder
	if p.Turn > 0 {
		sb.WriteString(fmt.Sprintf("turn:%d ", p.Turn))
	}
	sb.WriteString(fmt.Sprintf("session:%s model:%s cost:$%.6f process:%ds ai:%ds reg_in:%d reg_out:%d cache_in:%d cache_out:%d",
		sid, p.Model, cost,
		p.ProcessMs/1000, p.AIMs/1000,
		p.RegInputTokens, p.RegOutputTokens,
		p.CacheInTokens, p.CacheOutTokens))
	if p.Files > 0 || p.LinesAdded > 0 || p.LinesRemoved > 0 {
		sb.WriteString(fmt.Sprintf(" files:%d +%d -%d net:%d",
			p.Files, p.LinesAdded, p.LinesRemoved, p.LinesAdded-p.LinesRemoved))
	}
	sb.WriteString(fmt.Sprintf(" step:%s at:%s", step, at))
	return sb.String()
}

// seenSessionIDs walks time_tracking.ai_turns entries in the item's Doc and
// returns the set of distinct session ids observed. Used to decide whether
// session_count should increment on a new payload.
func seenSessionIDs(item *model.Item) map[string]bool {
	seen := map[string]bool{}
	if item == nil || item.Doc == nil {
		return seen
	}
	inTimeTracking := false
	inAITurns := false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inTimeTracking = line.Key == "time_tracking"
			inAITurns = false
			continue
		}
		if !inTimeTracking {
			continue
		}
		if line.Indent == 2 && line.Key == "ai_turns" {
			inAITurns = true
			continue
		}
		if line.Indent <= 2 && line.Key != "" && line.Key != "ai_turns" {
			inAITurns = false
			continue
		}
		if !inAITurns {
			continue
		}
		trimmed := strings.TrimSpace(line.Raw)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		entry := strings.TrimPrefix(trimmed, "- ")
		if idx := strings.Index(entry, "session:"); idx >= 0 {
			rest := entry[idx+len("session:"):]
			if sp := strings.IndexByte(rest, ' '); sp > 0 {
				rest = rest[:sp]
			}
			if rest != "" {
				seen[rest] = true
			}
		}
	}
	return seen
}

// writeOrphanLog appends a single JSON line to cfg.SessionsDir()/orphan.log.
// Never silently drops metrics.
func writeOrphanLog(cfg *config.Config, p SessionLogPayload) error {
	dir := cfg.SessionsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "orphan.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Inject orphan timestamp before marshaling
	type orphanRecord struct {
		At      string            `json:"at"`
		Reason  string            `json:"reason"`
		Payload SessionLogPayload `json:"payload"`
	}
	rec := orphanRecord{
		At:      time.Now().Format(time.RFC3339),
		Reason:  "no_item_on_stack_or_item_missing",
		Payload: p,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func hasTokens(p SessionLogPayload) bool {
	return p.RegInputTokens > 0 || p.RegOutputTokens > 0 ||
		p.CacheInTokens > 0 || p.CacheOutTokens > 0 || p.CacheOut1hTokens > 0
}
