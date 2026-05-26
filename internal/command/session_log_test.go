package command

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

func TestSessionLog_BasicAccrual(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID:                  "sess-1",
		Model:                      "claude-opus-4-7",
		ProcessMs:                  42_000,
		AIMs:                       38_000,
		RegInputTokens:             1000,
		RegOutputTokens:            500,
		CacheReadInputTokens:       10_000,
		CacheCreation5mInputTokens: 200,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	assertInt(t, item, "time_tracking", "process_time_seconds", 42)
	assertInt(t, item, "time_tracking", "ai_time_seconds", 38)
	assertInt(t, item, "time_tracking", "reg_input_tokens", 1000)
	assertInt(t, item, "time_tracking", "reg_output_tokens", 500)
	assertInt(t, item, "time_tracking", "cache_in_tokens", 10_000)
	assertInt(t, item, "time_tracking", "cache_out_tokens", 200)
	// I-569 finding-3: total_input_tokens / total_output_tokens are no
	// longer written (migrate-strip-cost retired them; SessionLog stopped
	// resurrecting them on the next turn). real_tokens has the same data.
	rt := readRealTokens(item)
	if got := rt.Input + rt.CacheRead + rt.CacheCreation5m; got != 1000+10_000+200 {
		t.Errorf("real_tokens input+cache total = %d, want %d", got, 1000+10_000+200)
	}
	if rt.Output != 500 {
		t.Errorf("real_tokens.output = %d, want 500", rt.Output)
	}
	assertInt(t, item, "time_tracking", "turn_count", 1)
	assertInt(t, item, "time_tracking", "session_count", 1)
	assertString(t, item, "time_tracking", "last_session", "sess-1")
	assertString(t, item, "time_tracking", "last_model", "claude-opus-4-7")

	// Cost computed: 1000*5/1M + 500*25/1M + 10000*0.5/1M + 200*6.25/1M
	// = 0.005 + 0.0125 + 0.005 + 0.00125 = 0.02375
	got := readFloatField(item, "time_tracking", "ai_cost_usd")
	want := 0.02375
	if diff := abs(got - want); diff > 1e-4 {
		t.Errorf("ai_cost_usd = %.6f, want %.6f (diff %.6f)", got, want, diff)
	}
}

func TestSessionLog_AccumulatesAcrossTurns(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "sess-1", Model: "claude-haiku-4-5",
		ProcessMs: 10_000, AIMs: 8_000,
		RegInputTokens: 100, RegOutputTokens: 50,
	}
	SessionLog(env.S, env.Cfg, p)
	SessionLog(env.S, env.Cfg, p)
	SessionLog(env.S, env.Cfg, p)

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	assertInt(t, item, "time_tracking", "process_time_seconds", 30)
	assertInt(t, item, "time_tracking", "ai_time_seconds", 24)
	assertInt(t, item, "time_tracking", "reg_input_tokens", 300)
	assertInt(t, item, "time_tracking", "reg_output_tokens", 150)
	assertInt(t, item, "time_tracking", "turn_count", 3)
	// Single session across all three turns
	assertInt(t, item, "time_tracking", "session_count", 1)
}

func TestSessionLog_SessionCountTracksDistinctIDs(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	base := SessionLogPayload{Model: "claude-haiku-4-5", RegInputTokens: 100}

	for _, sid := range []string{"sess-A", "sess-A", "sess-B", "sess-A", "sess-C"} {
		p := base
		p.SessionID = sid
		SessionLog(env.S, env.Cfg, p)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	// 3 distinct session ids: A, B, C
	assertInt(t, item, "time_tracking", "session_count", 3)
	assertInt(t, item, "time_tracking", "turn_count", 5)
}

func TestSessionLog_EmptyStackWritesOrphanLog(t *testing.T) {
	// I-414: pin the agent id explicitly so the orphan-log attribution
	// assertion below has a known non-empty value to check against.
	t.Setenv("AS_AGENT_ID", "agent-test")
	env := testutil.NewEnv(t)
	// No stack entries

	p := SessionLogPayload{
		SessionID: "sess-orphan", Model: "claude-opus-4-7",
		RegInputTokens: 1000, RegOutputTokens: 500,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("expected exit 0 on orphan, got %d", code)
	}

	orphanPath := filepath.Join(env.Cfg.SessionsDir(), "orphan.log")
	data, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("orphan.log not written: %v", err)
	}
	if !strings.Contains(string(data), "sess-orphan") {
		t.Errorf("orphan.log missing session id: %s", string(data))
	}
	if !strings.Contains(string(data), "no_item_on_stack") {
		t.Errorf("orphan.log missing reason: %s", string(data))
	}
	// I-414: every orphan entry must be tagged with the agent that
	// produced it so meta-work bucketizes per-agent for stats queries.
	if !strings.Contains(string(data), `"agent_id":"agent-test"`) {
		t.Errorf("orphan.log missing agent_id=agent-test attribution: %s", string(data))
	}
}

func TestSessionLog_MissingItemWritesOrphanLog(t *testing.T) {
	// I-414: this is the second path that writes orphan.log (item-not-found,
	// distinct from the empty-stack path). Pin the agent id so the orphan
	// attribution is verified here too.
	t.Setenv("AS_AGENT_ID", "agent-test")
	env := testutil.NewEnv(t)
	// Stack points to an item that doesn't exist
	SaveStack(env.Cfg, []StackEntry{{ID: "T-999"}})

	p := SessionLogPayload{SessionID: "x", Model: "claude-opus-4-7", RegInputTokens: 10}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("expected 0 (soft-fail to orphan), got %d", code)
	}
	orphanPath := filepath.Join(env.Cfg.SessionsDir(), "orphan.log")
	data, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("orphan.log expected: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-test"`) {
		t.Errorf("orphan.log missing agent_id attribution on missing-item path: %s", string(data))
	}
}

// I-569 step 3: per-turn cost source tracking (last_cost_source,
// unknown_cost_turns) was retired. The "I couldn't compute cost" signal
// is now implicit — tokens > 0 with cost == 0. Test confirms tokens
// still record, cost stays at 0, and no source bookkeeping is written.
func TestSessionLog_UnknownModelRecordsTokensNoCost(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "s", Model: "claude-future-99-0",
		RegInputTokens: 1000, RegOutputTokens: 500,
	}
	// Should not error — tokens always recorded, cost surfaces a warning
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	assertInt(t, item, "time_tracking", "reg_input_tokens", 1000)
	cost := readFloatField(item, "time_tracking", "ai_cost_usd")
	if cost != 0 {
		t.Errorf("unknown model cost should be 0, got %.4f", cost)
	}
	// last_cost_source / unknown_cost_turns are retired — no longer written.
	if got, _ := getNestedField(item, "time_tracking", "last_cost_source"); got != "" {
		t.Errorf("last_cost_source should not be set, got %q", got)
	}
	if got := readIntField(item, "time_tracking", "unknown_cost_turns"); got != 0 {
		t.Errorf("unknown_cost_turns should not be incremented, got %d", got)
	}
}

func TestSessionLog_OpenAIUnknownCostIsExplicit(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := sessionLogPayloadFromUsage(AIUsage{
		Provider:        AIProviderOpenAI,
		SessionID:       "codex-session",
		ResponseID:      "resp_123",
		Model:           "gpt-5.2",
		Step:            "implement",
		ProcessMs:       11_000,
		AIMs:            10_000,
		RegInputTokens:  800,
		CachedInTokens:  400,
		RegOutputTokens: 300,
		ReasoningTokens: 75,
		TotalTokens:     1500,
		CostSource:      CostSourceUnknown,
	}, "")
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	assertString(t, item, "time_tracking", "last_provider", AIProviderOpenAI)
	assertString(t, item, "time_tracking", "last_model", "gpt-5.2")
	assertInt(t, item, "time_tracking", "reg_input_tokens", 800)
	assertInt(t, item, "time_tracking", "cache_in_tokens", 400)
	assertInt(t, item, "time_tracking", "reasoning_tokens", 75)
	assertInt(t, item, "time_tracking", "total_tokens", 1500)
	// I-569 step 3: no last_cost_source / unknown_cost_turns / cost_source:
	// emission. Cost renders as $0.000000 (OpenAI pricing isn't in the
	// table; pricing returns ErrUnknownModel).
	if got, _ := getNestedField(item, "time_tracking", "last_cost_source"); got != "" {
		t.Errorf("last_cost_source should not be set, got %q", got)
	}
	if got := readIntField(item, "time_tracking", "unknown_cost_turns"); got != 0 {
		t.Errorf("unknown_cost_turns should not be incremented, got %d", got)
	}

	raw, err := os.ReadFile(filepath.Join(env.Root, "tasks", "T-003-active.md"))
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	s := string(raw)
	for _, needle := range []string{
		"provider:openai",
		"response:resp_123",
		"model:gpt-5.2",
		"cost:$0.000000",
		"reasoning:75",
		"total:1500",
		"openai/gpt-5.2:",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("OpenAI session log missing %q. File:\n%s", needle, s)
		}
	}
	for _, banned := range []string{"cost_source:", "cost:unknown", "unknown_cost_turns="} {
		if strings.Contains(s, banned) {
			t.Errorf("OpenAI session log should not contain %q (I-569 step 3 retired it). File:\n%s", banned, s)
		}
	}
}

// I-569 step 3: payload.CostUSD is no longer trusted — pricing × tokens
// is the single source of truth. A producer trying to override (legacy
// pre-step-3 producers still set CostUSD) gets ignored; the recorded
// cost matches what the rate table would compute, not what the wire
// said.
func TestSessionLog_PayloadCostIgnoredAlwaysRecomputes(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		RegInputTokens: 1000, RegOutputTokens: 500,
		CostUSD: 0.9999, // ignored; computed cost wins
	}
	SessionLog(env.S, env.Cfg, p)
	env.Reload(t)
	item, _ := env.S.Get("T-003")
	got := readFloatField(item, "time_tracking", "ai_cost_usd")
	// Opus 4.7: 1000×$5/M + 500×$25/M = $0.005 + $0.0125 = $0.0175
	want := 0.0175
	if abs(got-want) > 1e-6 {
		t.Errorf("expected recomputed cost %.6f, got %.6f (CostUSD on payload should be ignored)", want, got)
	}
}

func TestSessionLog_AppendsAITurnLine(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "sess-xyz", Model: "claude-opus-4-7",
		ProcessMs: 5000, AIMs: 4000,
		RegInputTokens: 100, RegOutputTokens: 50,
		Turn: 7, Step: "interactive",
	}
	SessionLog(env.S, env.Cfg, p)

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	raw, err := os.ReadFile(filepath.Join(env.Root, "tasks", "T-003-active.md"))
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	s := string(raw)
	for _, needle := range []string{"ai_turns:", "session:sess-xyz", "model:claude-opus-4-7",
		"turn:7", "step:interactive", "process:5s", "ai:4s", "reg_in:100", "reg_out:50"} {
		if !strings.Contains(s, needle) {
			t.Errorf("ai_turns line missing %q. File:\n%s", needle, s)
		}
	}
	_ = item
}

func TestSessionLog_ByModel_SingleModel(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	for i := 0; i < 3; i++ {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			SessionID: "s", Model: "claude-opus-4-7",
			RegInputTokens: 1000, RegOutputTokens: 500,
			CacheReadInputTokens: 100, CacheCreation5mInputTokens: 50,
		})
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	agg := readByModel(item, "claude-opus-4-7")
	if agg.Turns != 3 {
		t.Errorf("opus turns = %d, want 3", agg.Turns)
	}
	if agg.RegIn != 3000 {
		t.Errorf("opus reg_in = %d, want 3000", agg.RegIn)
	}
	if agg.RegOut != 1500 {
		t.Errorf("opus reg_out = %d, want 1500", agg.RegOut)
	}
	if agg.CacheIn != 300 {
		t.Errorf("opus cache_in = %d, want 300", agg.CacheIn)
	}
	if agg.CacheOut != 150 {
		t.Errorf("opus cache_out = %d, want 150", agg.CacheOut)
	}
	// 1000*5 + 500*25 + 100*0.5 + 50*6.25 = 5000 + 12500 + 50 + 312.5 = 17862.5
	// per MTok, so /1M = 0.0178625, then × 3 turns = 0.0535875
	wantCost := 0.0535875
	if diff := abs(agg.Cost - wantCost); diff > 1e-4 {
		t.Errorf("opus cost = %.6f, want %.6f", agg.Cost, wantCost)
	}
}

func TestSessionLog_ByModel_MultipleModels(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		RegInputTokens: 1000, RegOutputTokens: 500,
	})
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-haiku-4-5",
		RegInputTokens: 10_000, RegOutputTokens: 5_000,
	})
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		RegInputTokens: 2000, RegOutputTokens: 1000,
	})

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	opus := readByModel(item, "claude-opus-4-7")
	haiku := readByModel(item, "claude-haiku-4-5")

	if opus.Turns != 2 {
		t.Errorf("opus turns = %d, want 2", opus.Turns)
	}
	if opus.RegIn != 3000 {
		t.Errorf("opus reg_in = %d, want 3000", opus.RegIn)
	}
	if haiku.Turns != 1 {
		t.Errorf("haiku turns = %d, want 1", haiku.Turns)
	}
	if haiku.RegIn != 10_000 {
		t.Errorf("haiku reg_in = %d, want 10000", haiku.RegIn)
	}

	// Aggregate turn_count on time_tracking equals sum of by_model turns
	totalTurns := readIntField(item, "time_tracking", "turn_count")
	if totalTurns != opus.Turns+haiku.Turns {
		t.Errorf("turn_count %d != opus(%d) + haiku(%d)", totalTurns, opus.Turns, haiku.Turns)
	}

	// Sanity: item's cumulative cost equals sum of per-model costs
	totalCost := readFloatField(item, "time_tracking", "ai_cost_usd")
	if abs(totalCost-(opus.Cost+haiku.Cost)) > 1e-4 {
		t.Errorf("ai_cost_usd %.6f != opus(%.6f) + haiku(%.6f)",
			totalCost, opus.Cost, haiku.Cost)
	}
}

func TestSessionLog_ByModel_UnknownModelRecorded(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-future-99-0",
		RegInputTokens: 500, RegOutputTokens: 100,
	})

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	agg := readByModel(item, "claude-future-99-0")
	// Tokens tracked even though cost couldn't be computed
	if agg.Turns != 1 || agg.RegIn != 500 || agg.RegOut != 100 {
		t.Errorf("unknown model aggregate missing tokens: %+v", agg)
	}
	if agg.Cost != 0 {
		t.Errorf("unknown model cost should be 0, got %.4f", agg.Cost)
	}
}

func TestSessionLog_1hCacheTierAccruedAndPriced(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// Opus 4.7 rates: cache_5m=$6.25/MTok (1.25x input=$5), cache_1h=$10/MTok (2x).
	// 100k 5m + 50k 1h = 100,000 * 6.25/M + 50,000 * 10/M = 0.625 + 0.5 = 1.125
	p := SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		CacheCreation5mInputTokens: 100_000, // 5m
		CacheCreation1hInputTokens: 50_000,  // 1h
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	env.Reload(t)
	item, _ := env.S.Get("T-003")

	assertInt(t, item, "time_tracking", "cache_out_tokens", 100_000)
	assertInt(t, item, "time_tracking", "cache_out_1h_tokens", 50_000)
	// I-569 finding-3: total_input_tokens retired in favor of real_tokens.
	rt := readRealTokens(item)
	if rt.CacheCreation5m != 100_000 || rt.CacheCreation1h != 50_000 {
		t.Errorf("real_tokens cache split = (5m:%d, 1h:%d), want (100000, 50000)",
			rt.CacheCreation5m, rt.CacheCreation1h)
	}

	cost := readFloatField(item, "time_tracking", "ai_cost_usd")
	want := 1.125
	if abs(cost-want) > 1e-4 {
		t.Errorf("ai_cost_usd = %.6f, want %.6f (5m@1.25x + 1h@2x)", cost, want)
	}
}

func TestSessionLog_EmptySessionIDBucketsAsUnknown(t *testing.T) {
	// Regression: turn_count must not exceed session_count. When a payload
	// arrives with no SessionID we bucket it under "unknown" so the
	// invariant (session_count >= 1 whenever turn_count >= 1) holds.
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	for i := 0; i < 3; i++ {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			// no SessionID
			Model:          "claude-haiku-4-5",
			RegInputTokens: 100,
		})
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	turnCount := readIntField(item, "time_tracking", "turn_count")
	sessionCount := readIntField(item, "time_tracking", "session_count")
	if turnCount != 3 {
		t.Errorf("turn_count = %d, want 3", turnCount)
	}
	if sessionCount != 1 {
		t.Errorf("session_count = %d, want 1 (unknown bucket)", sessionCount)
	}
}

func TestSessionLog_NestingInvariant(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// 5 turns with ai_ms ≤ process_ms each
	for i := 0; i < 5; i++ {
		SessionLog(env.S, env.Cfg, SessionLogPayload{
			SessionID: "s", Model: "claude-haiku-4-5",
			ProcessMs: 10_000, AIMs: 8_000,
			RegInputTokens: 100,
		})
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	ai := readIntField(item, "time_tracking", "ai_time_seconds")
	proc := readIntField(item, "time_tracking", "process_time_seconds")
	if ai > proc {
		t.Errorf("invariant violated: ai_time(%d) > process_time(%d)", ai, proc)
	}
}

func TestSessionLogCLI_ReadsStdin(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "cli-test", Model: "claude-haiku-4-5",
		RegInputTokens: 500, RegOutputTokens: 100,
	}
	b, _ := json.Marshal(p)

	code := SessionLogCLI(env.S, env.Cfg, bytes.NewReader(b))
	if code != 0 {
		t.Fatalf("SessionLogCLI exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")
	assertInt(t, item, "time_tracking", "reg_input_tokens", 500)
}

func TestSessionLogCLI_EmptyStdinIsUsageError(t *testing.T) {
	env := testutil.NewEnv(t)
	if code := SessionLogCLI(env.S, env.Cfg, bytes.NewReader(nil)); code != 2 {
		t.Errorf("expected exit 2 for empty stdin, got %d", code)
	}
}

func TestSessionLogCLI_InvalidJSONIsError(t *testing.T) {
	env := testutil.NewEnv(t)
	if code := SessionLogCLI(env.S, env.Cfg, strings.NewReader("not json")); code != 1 {
		t.Errorf("expected exit 1 for invalid json, got %d", code)
	}
}

// T-330: SubagentStop hook populates RollupItemID on every subagent
// payload per the I-369 (Option C) decision so cost rolls up to the
// spawning agent's root item even when the parent's stack has shifted by
// the time the subagent finishes. The accumulator's ItemID resolution
// honors RollupItemID before falling through to stack-top.
//
// ParentID/RootID/Role remain AGENT-CHAIN provenance (agent-id-typed),
// emitted into the ai_turns line for T-327's `st stats meta --by role`
// drill-down — separate namespace from the item-id routing key.
func TestSessionLog_RoutesViaRollupItemID(t *testing.T) {
	env := testutil.NewEnv(t)
	// Stack top is T-001, but the subagent payload says RollupItemID=T-003.
	// Metrics MUST land on T-003 — the spawning agent's root item.
	SaveStack(env.Cfg, []StackEntry{{ID: "T-001"}})

	p := SessionLogPayload{
		SessionID:       "subagent-sess",
		ParentID:        "agent-a", // agent-id, provenance only
		RootID:          "agent-a", // agent-id, provenance only
		Role:            "code-reviewer",
		RollupItemID:    "T-003", // item-id, the routing target
		Model:           "claude-haiku-4-5",
		RegInputTokens:  2000,
		RegOutputTokens: 400,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}

	env.Reload(t)

	// Metrics on T-003 (the RollupItemID target).
	target, _ := env.S.Get("T-003")
	assertInt(t, target, "time_tracking", "reg_input_tokens", 2000)
	assertInt(t, target, "time_tracking", "reg_output_tokens", 400)
	assertInt(t, target, "time_tracking", "turn_count", 1)

	// Stack-top item T-001 must be untouched.
	other, _ := env.S.Get("T-001")
	if got := readIntField(other, "time_tracking", "reg_input_tokens"); got != 0 {
		t.Errorf("T-001 reg_input_tokens = %d, want 0 (RollupItemID should override stack-top)", got)
	}

	// The per-turn ai_turns line preserves heritage so drill-down (T-327)
	// can group by role / parent agent / root agent.
	raw, err := os.ReadFile(filepath.Join(env.Root, "tasks", "T-003-active.md"))
	if err != nil {
		t.Fatalf("read T-003: %v", err)
	}
	for _, want := range []string{"role:code-reviewer", "parent:agent-a", "root:agent-a"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("ai_turns line missing %q. File:\n%s", want, string(raw))
		}
	}
}

// Sanity: regular parent-agent turns (no RollupItemID) still route via
// stack top — the non-subagent path is unchanged.
func TestSessionLog_RollupItemIDFallthroughWhenEmpty(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		// No ParentID/RootID/Role/RollupItemID — regular parent-agent turn.
		SessionID:      "regular-sess",
		Model:          "claude-opus-4-7",
		RegInputTokens: 100,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}
	env.Reload(t)
	item, _ := env.S.Get("T-003")
	assertInt(t, item, "time_tracking", "reg_input_tokens", 100)
}

// Explicit ItemID beats RollupItemID — caller-known attribution always wins.
func TestSessionLog_ExplicitItemIDBeatsRollupItemID(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-001"}})

	p := SessionLogPayload{
		ItemID:         "T-002",
		RollupItemID:   "T-003",
		SessionID:      "sess",
		Model:          "claude-haiku-4-5",
		RegInputTokens: 50,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}
	env.Reload(t)

	want, _ := env.S.Get("T-002")
	assertInt(t, want, "time_tracking", "reg_input_tokens", 50)

	for _, otherID := range []string{"T-001", "T-003"} {
		other, _ := env.S.Get(otherID)
		if other == nil {
			continue
		}
		if got := readIntField(other, "time_tracking", "reg_input_tokens"); got != 0 {
			t.Errorf("%s should be untouched, got reg_input_tokens=%d", otherID, got)
		}
	}
}

// Coverage for the orphan fallthrough on a missing rollup target — the
// existing item-not-found soft-fail must still fire when a subagent's
// RollupItemID points to a phantom item.
func TestSessionLog_RollupItemIDMissingItemWritesOrphanLog(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-test")
	env := testutil.NewEnv(t)
	// Empty stack; payload's RollupItemID points to a phantom T-999.
	p := SessionLogPayload{
		SessionID:      "subagent-sess",
		Role:           "Explore",
		RollupItemID:   "T-999",
		Model:          "claude-haiku-4-5",
		RegInputTokens: 50,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("expected soft-fail orphan, got exit=%d", code)
	}
	orphanPath := filepath.Join(env.Cfg.SessionsDir(), "orphan.log")
	data, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("orphan.log expected: %v", err)
	}
	if !strings.Contains(string(data), `"agent_id":"agent-test"`) {
		t.Errorf("orphan.log missing agent_id: %s", string(data))
	}
	if !strings.Contains(string(data), `"rollup_item_id":"T-999"`) {
		t.Errorf("orphan.log missing rollup_item_id field: %s", string(data))
	}
}

// I-448: parallel subagents in a fan-out (e.g. /code-review's 5
// reviewers) produce ai_turns rows with byte-identical token tuples
// but distinct session IDs. Without dedup, each rolls up into the
// parent's totals, inflating cache_in/total_input_tokens N×. Verify
// that:
//   - the FIRST subagent payload accrues normally
//   - a second subagent payload with identical (cache_in, reg_in,
//     reg_out, cache_out_1h, role, model) tuple within 60s is
//     dropped — turn_count stays at 1, totals don't double
//   - an interactive payload with the same tuple still accrues
//     (dedup is scoped to step:subagent only)
func TestSessionLog_DropsDuplicateSubagentTurnsWithin60s(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	tuple := SessionLogPayload{
		Step:                       "subagent",
		Role:                       "general-purpose",
		Model:                      "claude-opus-4-7",
		RegInputTokens:             45676,
		RegOutputTokens:            1050962,
		CacheReadInputTokens:       601116722,
		CacheCreation1hInputTokens: 5849169,
		// I-569 step 10 soft cap: 601M cache_read on a 0ms turn would be
		// rejected as physically impossible. Set ProcessMs above the
		// 60s minimum so the dedup logic gets a chance to run.
		ProcessMs: 120_000,
	}

	// First subagent — should accrue.
	first := tuple
	first.SessionID = "agent-aaa"
	if code := SessionLog(env.S, env.Cfg, first); code != 0 {
		t.Fatalf("first subagent SessionLog exit=%d", code)
	}

	// Second subagent — different agent ID, byte-identical tuple within
	// 60s — must be dropped.
	second := tuple
	second.SessionID = "agent-bbb"
	if code := SessionLog(env.S, env.Cfg, second); code != 0 {
		t.Fatalf("second subagent SessionLog exit=%d", code)
	}

	// Third subagent, same shape as the first two.
	third := tuple
	third.SessionID = "agent-ccc"
	if code := SessionLog(env.S, env.Cfg, third); code != 0 {
		t.Fatalf("third subagent SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	if got := readIntField(item, "time_tracking", "turn_count"); got != 1 {
		t.Errorf("turn_count = %d, want 1 (dups dropped)", got)
	}
	if got := readIntField(item, "time_tracking", "cache_in_tokens"); got != 601116722 {
		t.Errorf("cache_in_tokens = %d, want 601116722 (single tuple, dups dropped)", got)
	}

	// Interactive payload with the same tuple must STILL accrue —
	// dedup is scoped to step:subagent only.
	interactive := tuple
	interactive.SessionID = "agent-interactive"
	interactive.Step = "interactive"
	if code := SessionLog(env.S, env.Cfg, interactive); code != 0 {
		t.Fatalf("interactive SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ = env.S.Get("T-003")

	if got := readIntField(item, "time_tracking", "turn_count"); got != 2 {
		t.Errorf("turn_count = %d, want 2 (interactive must accrue past dedup)", got)
	}
	if got := readIntField(item, "time_tracking", "cache_in_tokens"); got != 2*601116722 {
		t.Errorf("cache_in_tokens = %d, want %d (1 subagent + 1 interactive)", got, 2*601116722)
	}
}

// I-569: provenance-only subagent payloads (no tokens, step:subagent)
// must NOT be silently dropped by the I-448 dedup. With all-zero token
// tuples, every provenance marker tuple-matches every other one — the
// pre-fix dedup would treat the second, third, ... markers as
// duplicates and drop them, losing per-subagent attribution. The fix
// guards the dedup behind hasTokens(payload) so zero-token markers
// always accrue.
func TestSessionLog_ProvenanceOnlySubagentNotDeduped(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	marker := SessionLogPayload{
		Step: "subagent",
		Role: "Explore",
		// No model, no tokens — pure provenance signal.
	}

	first := marker
	first.SessionID = "agent-aaa"
	if code := SessionLog(env.S, env.Cfg, first); code != 0 {
		t.Fatalf("first marker SessionLog exit=%d", code)
	}

	second := marker
	second.SessionID = "agent-bbb"
	if code := SessionLog(env.S, env.Cfg, second); code != 0 {
		t.Fatalf("second marker SessionLog exit=%d", code)
	}

	third := marker
	third.SessionID = "agent-ccc"
	if code := SessionLog(env.S, env.Cfg, third); code != 0 {
		t.Fatalf("third marker SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-003")

	// All three markers must have accrued — turn_count == 3, not 1.
	if got := readIntField(item, "time_tracking", "turn_count"); got != 3 {
		t.Errorf("turn_count = %d, want 3 (provenance-only markers must not dedup)", got)
	}
}

// --- helpers ---

func assertInt(t *testing.T, item *model.Item, parent, key string, want int) {
	t.Helper()
	got := readIntField(item, parent, key)
	if got != want {
		t.Errorf("%s.%s = %d, want %d", parent, key, got, want)
	}
}

func assertString(t *testing.T, item *model.Item, parent, key, want string) {
	t.Helper()
	got, _ := getNestedField(item, parent, key)
	if got != want {
		t.Errorf("%s.%s = %q, want %q", parent, key, got, want)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestSessionLogGitSyncsAfterMutate(t *testing.T) {
	env := testutil.NewGitEnv(t)
	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	root := env.Cfg.Root()

	// Push T-003 onto the stack so SessionLog resolves it as the target item.
	if err := SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}}); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}
	// Commit stack file so the next GitSync creates a distinguishable new commit.
	gitRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun("add", "-A")
	gitRun("commit", "-m", "push T-003 stack")

	p := SessionLogPayload{
		SessionID:       "test-sid-001",
		Model:           "claude-sonnet-4-6",
		ProcessMs:       5000,
		AIMs:            3000,
		RegInputTokens:  100,
		RegOutputTokens: 50,
	}
	if rc := SessionLog(env.S, env.Cfg, p); rc != 0 {
		t.Fatalf("SessionLog rc=%d", rc)
	}

	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "as session log: T-003") {
		t.Errorf("HEAD commit = %q, want 'as session log: T-003'", strings.TrimSpace(string(out)))
	}
}

func TestSessionLogLeavesCleanWorkingTree(t *testing.T) {
	env := testutil.NewGitEnv(t)
	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	root := env.Cfg.Root()

	if err := SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}}); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}
	exec.Command("git", "-C", root, "add", "-A").Run()
	exec.Command("git", "-C", root, "commit", "-m", "push T-003 stack").Run()

	p := SessionLogPayload{
		SessionID:       "test-sid-002",
		Model:           "claude-sonnet-4-6",
		ProcessMs:       5000,
		AIMs:            3000,
		RegInputTokens:  200,
		RegOutputTokens: 80,
	}
	SessionLog(env.S, env.Cfg, p)

	if trackedDirty(t, root) {
		t.Error("tracked files dirty after SessionLog — GitSync must commit all modifications")
	}
}

func TestSessionLogSubagentStepDoesNotSync(t *testing.T) {
	// Subagent-step payloads must NOT trigger GitSync — they fire rapidly in
	// fan-out scenarios and would cause git lock contention across concurrent
	// agents. The metric lands on disk; the next interactive-step GitSync
	// commits it.
	env := testutil.NewGitEnv(t)
	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	root := env.Cfg.Root()

	if err := SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}}); err != nil {
		t.Fatalf("SaveStack: %v", err)
	}
	exec.Command("git", "-C", root, "add", "-A").Run()
	exec.Command("git", "-C", root, "commit", "-m", "setup").Run()

	shaBefore, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()

	p := SessionLogPayload{
		Step:            "subagent",
		SessionID:       "subagent-sid",
		Model:           "claude-sonnet-4-6",
		ProcessMs:       3000,
		RegInputTokens:  50,
		RegOutputTokens: 20,
	}
	if rc := SessionLog(env.S, env.Cfg, p); rc != 0 {
		t.Fatalf("SessionLog rc=%d", rc)
	}

	shaAfter, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(shaBefore)) != strings.TrimSpace(string(shaAfter)) {
		t.Errorf("subagent-step SessionLog must not create a new commit: before=%s after=%s",
			strings.TrimSpace(string(shaBefore)), strings.TrimSpace(string(shaAfter)))
	}
}
