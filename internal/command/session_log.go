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
	SessionID       string  `json:"session_id"`
	Model           string  `json:"model"`
	ProcessMs       int64   `json:"process_ms"`
	AIMs            int64   `json:"ai_ms"`
	RegInputTokens  int     `json:"reg_input_tokens"`
	RegOutputTokens int     `json:"reg_output_tokens"`
	CacheInTokens   int     `json:"cache_in_tokens"`
	CacheOutTokens  int     `json:"cache_out_tokens"`
	CostUSD         float64 `json:"cost_usd,omitempty"` // if 0, computed from tokens × pricing
	Turn            int     `json:"turn,omitempty"`     // ordinal within session; informational
	ItemID          string  `json:"item_id,omitempty"`  // if empty, resolved from stack top
	Step            string  `json:"step,omitempty"`     // default "interactive"

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
			payload.CacheInTokens, payload.CacheOutTokens,
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

	// Derived totals — kept in the file so consumers don't need to compute
	setNestedField(item, "time_tracking", "total_input_tokens",
		fmt.Sprintf("%d",
			readIntField(item, "time_tracking", "reg_input_tokens")+
				readIntField(item, "time_tracking", "cache_in_tokens")+
				readIntField(item, "time_tracking", "cache_out_tokens")))
	setNestedField(item, "time_tracking", "total_output_tokens",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_output_tokens")))

	if cost > 0 {
		setNestedField(item, "time_tracking", "ai_cost_usd",
			fmt.Sprintf("%.4f", readFloatField(item, "time_tracking", "ai_cost_usd")+cost))
	}

	setNestedField(item, "time_tracking", "turn_count",
		fmt.Sprintf("%d", readIntField(item, "time_tracking", "turn_count")+1))

	// session_count: recompute from distinct session ids in ai_turns (walk-based
	// for correctness; list is typically small). A new session_id triggers +1.
	if payload.SessionID != "" {
		seen := seenSessionIDs(item)
		if !seen[payload.SessionID] {
			setNestedField(item, "time_tracking", "session_count",
				fmt.Sprintf("%d", len(seen)+1))
		}
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
	appendListField(item, "work_tracking", "ai_turns", formatAITurnLine(payload, cost, now))

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "session log: writing %s: %v\n", itemID, err)
		return 1
	}
	return 0
}

// formatAITurnLine produces the provenance line appended to work_tracking.ai_turns.
// Format is space-separated key:value pairs — grep-friendly and stable enough to
// be parsed by downstream reporting without a dedicated parser.
func formatAITurnLine(p SessionLogPayload, cost float64, at string) string {
	step := p.Step
	if step == "" {
		step = "interactive"
	}
	var sb strings.Builder
	if p.Turn > 0 {
		sb.WriteString(fmt.Sprintf("turn:%d ", p.Turn))
	}
	sb.WriteString(fmt.Sprintf("session:%s model:%s cost:$%.4f process:%ds ai:%ds reg_in:%d reg_out:%d cache_in:%d cache_out:%d",
		p.SessionID, p.Model, cost,
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

// seenSessionIDs walks work_tracking.ai_turns entries in the item's Doc and
// returns the set of distinct session ids observed. Used to decide whether
// session_count should increment on a new payload.
func seenSessionIDs(item *model.Item) map[string]bool {
	seen := map[string]bool{}
	if item == nil || item.Doc == nil {
		return seen
	}
	inWorkTracking := false
	inAITurns := false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inWorkTracking = line.Key == "work_tracking"
			inAITurns = false
			continue
		}
		if !inWorkTracking {
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
		p.CacheInTokens > 0 || p.CacheOutTokens > 0
}
