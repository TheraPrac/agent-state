package command

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// codexThreadStarted is the JSONL event emitted at the start of a codex exec
// invocation. It carries the thread_id used as the session identifier.
type codexThreadStarted struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
}

// codexTurnCompletedUsage is the usage object inside a turn.completed event.
type codexTurnCompletedUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// codexTurnCompleted is the JSONL event emitted after each agent turn.
type codexTurnCompleted struct {
	Type  string                  `json:"type"`
	Usage codexTurnCompletedUsage `json:"usage"`
}

// codexTokenCounts holds token counts summed across all turn.completed events
// in a single codex exec invocation.
type codexTokenCounts struct {
	Input           int
	CachedInput     int
	Output          int
	ReasoningOutput int
}

// Total returns input + output (codex --json has no explicit total field).
func (c codexTokenCounts) Total() int { return c.Input + c.Output }

// parseCodexThreadID extracts the thread_id from the thread.started JSONL event
// in a codex --json output stream. Returns "" if not found.
func parseCodexThreadID(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `"thread.started"`) && !strings.Contains(line, `thread_id`) {
			continue
		}
		var ev codexThreadStarted
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
	}
	return ""
}

// parseCodexSessionID is an alias for parseCodexThreadID used by CodexEngine
// to match the naming convention in the rest of the pipeline.
func parseCodexSessionID(output []byte) string {
	return parseCodexThreadID(output)
}

// parseCodexTurnUsage sums token counts across all turn.completed JSONL events
// in a codex --json output stream. If no events are found, returns zero counts.
func parseCodexTurnUsage(output []byte) codexTokenCounts {
	var totals codexTokenCounts
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `"turn.completed"`) {
			continue
		}
		var ev codexTurnCompleted
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "turn.completed" {
			continue
		}
		totals.Input += ev.Usage.InputTokens
		totals.CachedInput += ev.Usage.CachedInputTokens
		totals.Output += ev.Usage.OutputTokens
		totals.ReasoningOutput += ev.Usage.ReasoningOutputTokens
	}
	return totals
}

// parseCodexExitSummary is a backward-compatibility alias for parseCodexTurnUsage
// used by tests and internal callers that predate the JSONL schema discovery.
func parseCodexExitSummary(output []byte) codexTokenCounts {
	return parseCodexTurnUsage(output)
}

// codexCursorKey returns the delivery sub-key for the last-seen cumulative
// counter for a given Codex thread and field. One cursor per thread ensures
// that resume delta accounting is correct across multiple st run invocations
// if codex exec resume re-emits prior-turn usage.
func codexCursorKey(sessionID, field string) string {
	return fmt.Sprintf("codex_usage_%s_%s", sanitizeCodexSessionID(sessionID), field)
}

// sanitizeCodexSessionID replaces characters not valid in YAML map keys with
// underscores so the cursor fields can be stored in delivery.* safely.
func sanitizeCodexSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// computeCodexUsageDelta reads last-seen cumulative counters from the item's
// delivery fields, computes deltas against cur, persists the new cursors, and
// returns the delta as an AIUsage ready for SessionLog. This prevents
// double-counting if codex exec resume re-emits prior-turn usage totals.
func computeCodexUsageDelta(s *store.Store, cfg *config.Config, itemID, model_, sessionID string, cur codexTokenCounts) (AIUsage, error) {
	item, ok := s.Get(itemID)
	if !ok {
		return AIUsage{}, fmt.Errorf("codex usage: item %s not found", itemID)
	}

	readCursor := func(field string) int {
		v, _ := getNestedField(item, "delivery", codexCursorKey(sessionID, field))
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}

	prevInput  := readCursor("input")
	prevCached := readCursor("cached_input")
	prevOutput := readCursor("output")
	prevReason := readCursor("reasoning_output")

	deltaInput  := clampZero(cur.Input - prevInput)
	deltaCached := clampZero(cur.CachedInput - prevCached)
	deltaOutput := clampZero(cur.Output - prevOutput)
	deltaReason := clampZero(cur.ReasoningOutput - prevReason)

	// Persist new cursors so the next resume uses the updated baseline.
	if err := s.Mutate(itemID, func(item *model.Item) error {
		set := func(field string, val int) {
			item.SetNested("delivery", codexCursorKey(sessionID, field), strconv.Itoa(val))
		}
		set("input", cur.Input)
		set("cached_input", cur.CachedInput)
		set("output", cur.Output)
		set("reasoning_output", cur.ReasoningOutput)
		return nil
	}); err != nil {
		return AIUsage{}, fmt.Errorf("codex usage: persisting cursors for %s: %w", itemID, err)
	}

	regInput := clampZero(deltaInput - deltaCached)
	total := deltaInput + deltaOutput
	return AIUsage{
		Provider:        AIProviderOpenAI,
		Model:           model_,
		SessionID:       sessionID,
		RegInputTokens:  regInput,
		CachedInTokens:  deltaCached,
		RegOutputTokens: deltaOutput,
		ReasoningTokens: deltaReason,
		TotalTokens:     total,
		CostSource:      CostSourceEstimated,
	}, nil
}

// recordCodexUsage calls SessionLog with the per-turn Codex usage delta.
func recordCodexUsage(s *store.Store, cfg *config.Config, itemID, step string, usage AIUsage) {
	payload := sessionLogPayloadFromUsage(usage, itemID)
	payload.Step = step
	SessionLog(s, cfg, payload)
}

func clampZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
