package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/pricing"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SessionLogPayload is the per-turn metrics payload AI providers send to
// `st session log`. All token and duration fields are for the single turn; the
// command accrues them onto the resolved item.
//
// I-569 step 3: CostUSD and CostSource are accepted on the wire (older
// Claude Code producers still send them) but ignored. Cost is always
// recomputed from tokens × pricing inside SessionLog so the rate table is
// the single source of truth and migrations on price changes are
// unnecessary.
type SessionLogPayload struct {
	Provider        string `json:"provider,omitempty"`
	SessionID       string `json:"session_id"`
	ResponseID      string `json:"response_id,omitempty"`
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
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"` // if 0, computed from tokens × pricing
	CostSource       string  `json:"cost_source,omitempty"`
	Turn             int     `json:"turn,omitempty"`    // ordinal within session; informational
	ItemID           string  `json:"item_id,omitempty"` // if empty, resolved from stack top
	Step             string  `json:"step,omitempty"`    // default "interactive"

	// ProjectDir is the producer's CLAUDE_PROJECT_DIR. I-569 step 6's
	// reconcile-tokens needs this to derive the correct
	// `~/.claude/projects/<slug>/<sid>.jsonl` path back to ground truth
	// when the session ends and we want to reconcile drift. Optional —
	// pre-I-569 producers omit it and reconcile falls back to a
	// best-effort lookup.
	ProjectDir string `json:"project_dir,omitempty"`

	// Optional per-turn file diff info (populated by Stop hook once live).
	// Recorded in the ai_turns line for provenance.
	Files        int `json:"files,omitempty"`
	LinesAdded   int `json:"lines_added,omitempty"`
	LinesRemoved int `json:"lines_removed,omitempty"`

	// Optional sub-agent heritage. Set when the producing turn ran inside
	// a child agent spawned by a parent — preserves attribution so usage
	// rollups can credit the parent/root chain. Omitted when the executing
	// agent is the root.
	//
	// ParentID and RootID are AGENT-IDENTITY values (e.g. "agent-a"),
	// matching Identity.ParentID / Identity.RootID and Session.ParentAgentID
	// / Session.RootAgentID across the rest of the codebase. They appear in
	// the per-turn ai_turns line as "parent:<agent>" / "root:<agent>" for
	// provenance only — do NOT use them as a routing key. The routing target
	// is RollupItemID below.
	ParentID string `json:"parent_id,omitempty"`
	RootID   string `json:"root_id,omitempty"`
	Role     string `json:"role,omitempty"`

	// RollupItemID is the target item id this turn rolls up to (T-330). The
	// SubagentStop hook resolves it to the spawning agent's stack-top item
	// before shipping the payload, implementing the I-369 (Option C)
	// decision: subagent metrics accumulate on the parent's root item, with
	// per-turn provenance preserved via ParentID / RootID / Role above.
	// Distinct from RootID specifically to avoid overloading the agent-id
	// chain field with item-id semantics.
	RollupItemID string `json:"rollup_item_id,omitempty"`
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
//
// ItemID resolution priority:
//  1. Explicit payload.ItemID — caller knows exactly where this turn belongs.
//  2. payload.RollupItemID (T-330) — when set, a Task-tool subagent is
//     reporting and its work rolls up to the spawning agent's root item
//     per the I-369 (Option C) decision. Beats stack top because a
//     subagent firing after its parent has popped would otherwise orphan
//     the metrics; explicit rollup keeps them on the right item even
//     across stack churn.
//  3. Stack top — the working assumption for a parent agent's own turn.
//  4. Orphan log — never silently dropped.
//
// ParentID / RootID / Role on the payload are AGENT-CHAIN provenance
// (agent-id-typed), not routing keys. They surface in the ai_turns line
// for downstream drill-down (T-327's `st stats meta --by role`).
//
// This is the ONE accumulator both producers (Claude Code Stop hook,
// SubagentStop hook, st run's recordRunMetrics) call. Schema lives in the
// item's time_tracking block; per-turn provenance goes to
// work_tracking.ai_turns.
func SessionLog(s *store.Store, cfg *config.Config, payload SessionLogPayload) int {
	// Resolve target item
	itemID := payload.ItemID
	if itemID == "" {
		itemID = payload.RollupItemID
	}
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

	if _, ok := s.Get(itemID); !ok {
		// Item not found — treat as orphan rather than error; the alternative
		// is silent data loss.
		if err := writeOrphanLog(cfg, payload); err != nil {
			fmt.Fprintf(os.Stderr, "session log: writing orphan log: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "session log: item %s not found — logged to orphan.log\n", itemID)
		return 0
	}

	// I-569 step 3: synthetic cost is ALWAYS recomputed from tokens × the
	// pricing rate table. payload.CostUSD and payload.CostSource are
	// accepted on the wire (back-compat for older producers) but ignored
	// in logic — the only authoritative source is `pricing.ComputeCost`,
	// and unknown model just means cost stays at 0 for this turn (no
	// per-turn "unknown" bookkeeping; the absence is derivable from
	// non-zero tokens with zero cost).
	var cost float64
	if shouldComputeCost(payload) {
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

	// Capture computed values for the Mutate closure (cost computation is
	// done above — pure arithmetic, no I/O — so it's safe to run before
	// acquiring the lock).
	capturedCost := cost
	capturedNow := time.Now().Format(time.RFC3339)
	toucher := cfg.AgentID()
	if toucher == "" {
		toucher = "stop-hook"
	}
	capturedToucher := toucher
	capturedTurnLine := formatAITurnLine(payload, capturedCost, capturedNow)

	if err := s.Mutate(itemID, func(item *model.Item) error {
		// I-448: drop tuple-identical SUBAGENT turns within the last 60s.
		// The /code-review skill (and similar fan-out patterns) spawns
		// N parallel Sonnet/Haiku subagents whose SubagentStop hooks
		// fire per-agent, each producing ai_turns rows with byte-
		// identical (cache_in, reg_in, reg_out, cache_out_1h, role,
		// model) tuples but distinct session IDs. Without dedup, the
		// parent item's totals get inflated N× — that's the I-432 /
		// I-441 / I-443 24-billion-token / $15K bug. Scoped to subagent
		// payloads only so legitimate same-session same-token
		// accumulation (rare, but seen in unit tests) is unaffected.
		//
		// I-569: also require hasTokens(payload). A subagent payload
		// with all-zero tokens (e.g., a provenance-only marker, or any
		// future caller using step:subagent purely for attribution)
		// tuple-matches every other zero-token subagent payload and
		// would be silently dropped — a regression vector now that
		// I-569 has detached subagent provenance from the token sum.
		if payload.Step == "subagent" && hasTokens(payload) && isDuplicateRecentTurn(item, payload, capturedNow, 60) {
			fmt.Fprintf(os.Stderr,
				"session log: dropped duplicate subagent turn for %s (cache_in=%d reg_out=%d within 60s)\n",
				itemID, payload.CacheInTokens, payload.RegOutputTokens)
			return nil
		}

		// Accrue aggregate fields (re-reads from fresh disk copy inside lock)
		item.SetNested("time_tracking", "process_time_seconds",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "process_time_seconds")+int(payload.ProcessMs/1000)))
		item.SetNested("time_tracking", "ai_time_seconds",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "ai_time_seconds")+int(payload.AIMs/1000)))

		item.SetNested("time_tracking", "reg_input_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_input_tokens")+payload.RegInputTokens))
		item.SetNested("time_tracking", "reg_output_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_output_tokens")+payload.RegOutputTokens))
		if payload.ReasoningTokens > 0 || readIntField(item, "time_tracking", "reasoning_tokens") > 0 {
			item.SetNested("time_tracking", "reasoning_tokens",
				fmt.Sprintf("%d", readIntField(item, "time_tracking", "reasoning_tokens")+payload.ReasoningTokens))
		}
		item.SetNested("time_tracking", "cache_in_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_in_tokens")+payload.CacheInTokens))
		item.SetNested("time_tracking", "cache_out_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_out_tokens")+payload.CacheOutTokens))
		if payload.CacheOut1hTokens > 0 || readIntField(item, "time_tracking", "cache_out_1h_tokens") > 0 {
			item.SetNested("time_tracking", "cache_out_1h_tokens",
				fmt.Sprintf("%d", readIntField(item, "time_tracking", "cache_out_1h_tokens")+payload.CacheOut1hTokens))
		}

		// Derived totals — kept in the file so consumers don't need to compute
		item.SetNested("time_tracking", "total_input_tokens",
			fmt.Sprintf("%d",
				readIntField(item, "time_tracking", "reg_input_tokens")+
					readIntField(item, "time_tracking", "cache_in_tokens")+
					readIntField(item, "time_tracking", "cache_out_tokens")+
					readIntField(item, "time_tracking", "cache_out_1h_tokens")))
		item.SetNested("time_tracking", "total_output_tokens",
			fmt.Sprintf("%d", readIntField(item, "time_tracking", "reg_output_tokens")))
		if payload.TotalTokens > 0 || readIntField(item, "time_tracking", "total_tokens") > 0 {
			item.SetNested("time_tracking", "total_tokens",
				fmt.Sprintf("%d", readIntField(item, "time_tracking", "total_tokens")+payload.TotalTokens))
		}

		if capturedCost > 0 {
			// 6-decimal precision matches by_model cost precision so the two
			// aggregates don't drift apart across round-trips. See formatByModelLine.
			// I-569 step 5 will move this to render-time (computed from
			// real_tokens × current rates instead of accumulated).
			item.SetNested("time_tracking", "ai_cost_usd",
				fmt.Sprintf("%.6f", readFloatField(item, "time_tracking", "ai_cost_usd")+capturedCost))
		}

		item.SetNested("time_tracking", "turn_count",
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
			item.SetNested("time_tracking", "session_count",
				fmt.Sprintf("%d", len(seen)+1))
		}

		// Bookkeeping
		if payload.SessionID != "" {
			item.SetNested("time_tracking", "last_session", payload.SessionID)
		}
		if payload.Model != "" {
			item.SetNested("time_tracking", "last_model", payload.Model)
		}
		if payload.Provider != "" {
			item.SetNested("time_tracking", "last_provider", payload.Provider)
		}
		item.SetNested("time_tracking", "last_touched", capturedNow)
		item.SetNested("time_tracking", "last_touched_by", capturedToucher)

		// Append per-turn provenance line
		item.Doc.AppendToNestedList("time_tracking", "ai_turns", capturedTurnLine)

		// Upsert per-provider/model aggregate. Historical entries without Provider
		// keep their model-only key for backwards compatibility.
		if payload.Model != "" {
			upsertByModel(item, payload, capturedCost)
		}

		// I-569 step 2: canonical real_tokens block, plus per-step and
		// per-session rollups. These coexist with the legacy top-level
		// fields (reg_input_tokens, cache_*_tokens) until step 9's atomic
		// rename retires them. The new schema names match Anthropic SDK
		// exactly, so reconcile-tokens (step 6) can compare item state
		// against transcript JSONL `usage` blocks without translation.
		rt := realTokensFromPayload(payload)
		writeRealTokens(item, readRealTokens(item).add(rt))
		upsertByStep(item, payload.Step, rt, payload.ProcessMs)
		upsertBySession(item, payload.SessionID, payload.ProjectDir, capturedNow, rt)

		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "session log: writing %s: %v\n", itemID, err)
		return 1
	}
	return 0
}

// byModelAggregate captures provider/model running totals. Values are parsed
// from and written to the time_tracking.by_model line list.
type byModelAggregate struct {
	Turns            int
	RegIn            int
	RegOut           int
	CacheIn          int
	CacheOut         int
	Cost             float64
	UnknownCostTurns int
}

// upsertByModel finds the existing provider/model line under
// time_tracking.by_model (if any), adds the payload's deltas, and writes the
// updated line back. If no line exists, it appends a new one.
//
// I-569 step 3: no longer takes a cost source. Per-turn unknown_cost_turns
// tracking has been retired — a turn with non-zero tokens and zero cost is
// implicitly "unknown" and stats can derive that on demand. Existing
// unknown_cost_turns values on legacy by_model lines remain readable
// (parseByModelLine still accepts them) but are not incremented going
// forward.
func upsertByModel(item *model.Item, p SessionLogPayload, cost float64) {
	key := providerModelKey(p)
	existing := readByModel(item, key)
	existing.Turns++
	existing.RegIn += p.RegInputTokens
	existing.RegOut += p.RegOutputTokens
	existing.CacheIn += p.CacheInTokens
	existing.CacheOut += p.CacheOutTokens + p.CacheOut1hTokens // aggregate total cache writes per model
	existing.Cost += cost

	line := formatByModelLine(key, existing)

	// Try to update in place; if not found, append.
	if !updateListLine(item, "time_tracking", "by_model",
		func(raw string) bool { return byModelLineMatches(raw, key) },
		line) {
		item.Doc.AppendToNestedList("time_tracking", "by_model", line)
	}
}

// formatByModelLine produces a stable, grep-friendly representation.
// Format: "<model>: turns=N reg_in=N reg_out=N cache_in=N cache_out=N cost=$N.NNNNNN"
// 6-decimal cost precision keeps round-trip accumulation drift under $0.000001
// per turn — safe even across thousands of turns on cheap models.
func formatByModelLine(model string, a byModelAggregate) string {
	line := fmt.Sprintf("%s: turns=%d reg_in=%d reg_out=%d cache_in=%d cache_out=%d cost=$%.6f",
		model, a.Turns, a.RegIn, a.RegOut, a.CacheIn, a.CacheOut, a.Cost)
	if a.UnknownCostTurns > 0 {
		line += fmt.Sprintf(" unknown_cost_turns=%d", a.UnknownCostTurns)
	}
	return line
}

// readByModel walks time_tracking.by_model and returns the aggregate for a
// model-only or provider/model key, or the zero value if not present.
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
		case "unknown_cost_turns":
			fmt.Sscanf(val, "%d", &a.UnknownCostTurns)
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
//
// I-569 step 3: cost source is no longer tracked. cost: always renders as
// $%.6f (zero when not computable). The implicit "unknown" signal — non-zero
// tokens with zero cost — is derivable by any consumer that cares.
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
	if p.Provider != "" {
		sb.WriteString(fmt.Sprintf("provider:%s ", p.Provider))
	}
	if p.ResponseID != "" {
		sb.WriteString(fmt.Sprintf("response:%s ", p.ResponseID))
	}
	sb.WriteString(fmt.Sprintf("session:%s model:%s cost:$%.6f", sid, p.Model, cost))
	sb.WriteString(fmt.Sprintf(" process:%ds ai:%ds reg_in:%d reg_out:%d cache_in:%d cache_out:%d",
		p.ProcessMs/1000, p.AIMs/1000,
		p.RegInputTokens, p.RegOutputTokens,
		p.CacheInTokens, p.CacheOutTokens))
	if p.CacheOut1hTokens > 0 {
		// Only emit the 1h field when non-zero — keeps legacy grep patterns
		// that don't expect cache_out_1h from breaking.
		sb.WriteString(fmt.Sprintf(" cache_out_1h:%d", p.CacheOut1hTokens))
	}
	if p.ReasoningTokens > 0 {
		sb.WriteString(fmt.Sprintf(" reasoning:%d", p.ReasoningTokens))
	}
	if p.TotalTokens > 0 {
		sb.WriteString(fmt.Sprintf(" total:%d", p.TotalTokens))
	}
	if p.Files > 0 || p.LinesAdded > 0 || p.LinesRemoved > 0 {
		sb.WriteString(fmt.Sprintf(" files:%d +%d -%d net:%d",
			p.Files, p.LinesAdded, p.LinesRemoved, p.LinesAdded-p.LinesRemoved))
	}
	if p.ParentID != "" {
		sb.WriteString(fmt.Sprintf(" parent:%s", p.ParentID))
	}
	if p.RootID != "" {
		sb.WriteString(fmt.Sprintf(" root:%s", p.RootID))
	}
	if p.Role != "" {
		sb.WriteString(fmt.Sprintf(" role:%s", p.Role))
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

// isDuplicateRecentTurn returns true when the item's existing ai_turns
// already contains a byte-identical-tuple turn within `withinSec`
// seconds of `nowStr`. The tuple is (cache_in, cache_out, reg_in,
// reg_out, cache_out_1h, role, model) — the SubagentStop fan-out
// pattern that produced 5× duplication on I-432 / I-441 / I-443 had
// byte-identical values across all of these. Different agents
// producing legitimately-identical token counts within 60s is
// vanishingly unlikely; the time window is the primary guard rail.
//
// Scans every ai_turn (no early-exit on document order) so clock skew
// between parallel agents writing to the same item — which can produce
// out-of-order timestamps — doesn't silently let a duplicate through.
// Lists are small in practice; the linear scan cost is irrelevant
// against the disk write.
func isDuplicateRecentTurn(item *model.Item, p SessionLogPayload, nowStr string, withinSec int) bool {
	if item == nil || item.Doc == nil {
		return false
	}
	now, err := time.Parse(time.RFC3339, nowStr)
	if err != nil {
		return false
	}
	cutoff := now.Add(-time.Duration(withinSec) * time.Second)

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
		atStr := extractField(entry, "at:")
		if atStr == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			continue
		}
		if at.Before(cutoff) {
			continue
		}
		if extractIntField(entry, "cache_in:") != p.CacheInTokens {
			continue
		}
		if extractIntField(entry, "cache_out:") != p.CacheOutTokens {
			continue
		}
		if extractIntField(entry, "reg_in:") != p.RegInputTokens {
			continue
		}
		if extractIntField(entry, "reg_out:") != p.RegOutputTokens {
			continue
		}
		if extractIntField(entry, "cache_out_1h:") != p.CacheOut1hTokens {
			continue
		}
		if extractField(entry, "model:") != p.Model {
			continue
		}
		if extractField(entry, "role:") != p.Role {
			continue
		}
		return true
	}
	return false
}

// extractField pulls the value of a "key:value" pair from an ai_turns
// line; tokens are space-delimited so the value runs to the next space
// or end of string.
func extractField(line, key string) string {
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}

// extractIntField parses a "key:N" int field. Returns 0 on absent /
// malformed — matches the SessionLogPayload zero-value semantics so
// "missing field" and "explicit 0" tuple-match the same way.
func extractIntField(line, key string) int {
	v := extractField(line, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// writeOrphanLog appends a single JSON line to cfg.SessionsDir()/orphan.log.
// Never silently drops metrics.
//
// I-414: each entry is tagged with the agent id resolved at write time,
// so meta-work (between-item deliberation, exploration, anything that
// orphans because no item is on the stack) bucketizes per agent rather
// than flattening across the whole workspace. Stats queries like
// "how much overhead did agent-b accumulate this week" can group by
// the agent_id field. Sub-agent heritage (ParentID/RootID/Role) is
// preserved on the embedded payload for downstream rollup.
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

	// Inject orphan timestamp + agent attribution before marshaling
	type orphanRecord struct {
		At      string            `json:"at"`
		AgentID string            `json:"agent_id,omitempty"`
		Reason  string            `json:"reason"`
		Payload SessionLogPayload `json:"payload"`
	}
	rec := orphanRecord{
		At:      time.Now().Format(time.RFC3339),
		AgentID: cfg.AgentID(),
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
		p.CacheInTokens > 0 || p.CacheOutTokens > 0 || p.CacheOut1hTokens > 0 ||
		p.ReasoningTokens > 0 || p.TotalTokens > 0
}

func shouldComputeCost(p SessionLogPayload) bool {
	provider := strings.TrimSpace(strings.ToLower(p.Provider))
	return (provider == "" || provider == AIProviderClaude) && p.Model != "" && hasTokens(p)
}

func providerModelKey(p SessionLogPayload) string {
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		return p.Model
	}
	return provider + "/" + p.Model
}
