package command

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

func TestSessionLog_BasicAccrual(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID:       "sess-1",
		Model:           "claude-opus-4-7",
		ProcessMs:       42_000,
		AIMs:            38_000,
		RegInputTokens:  1000,
		RegOutputTokens: 500,
		CacheInTokens:   10_000,
		CacheOutTokens:  200,
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
	assertInt(t, item, "time_tracking", "total_input_tokens", 1000+10_000+200)
	assertInt(t, item, "time_tracking", "total_output_tokens", 500)
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
}

func TestSessionLog_MissingItemWritesOrphanLog(t *testing.T) {
	env := testutil.NewEnv(t)
	// Stack points to an item that doesn't exist
	SaveStack(env.Cfg, []StackEntry{{ID: "T-999"}})

	p := SessionLogPayload{SessionID: "x", Model: "claude-opus-4-7", RegInputTokens: 10}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("expected 0 (soft-fail to orphan), got %d", code)
	}
	orphanPath := filepath.Join(env.Cfg.SessionsDir(), "orphan.log")
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("orphan.log expected: %v", err)
	}
}

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
	// Cost must remain zero (not silently computed at unknown rate)
	cost := readFloatField(item, "time_tracking", "ai_cost_usd")
	if cost != 0 {
		t.Errorf("unknown model cost should be 0, got %.4f", cost)
	}
}

func TestSessionLog_PayloadCostOverridesComputed(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	p := SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		RegInputTokens: 1000, RegOutputTokens: 500,
		CostUSD: 0.9999, // trusted over computed
	}
	SessionLog(env.S, env.Cfg, p)
	env.Reload(t)
	item, _ := env.S.Get("T-003")
	got := readFloatField(item, "time_tracking", "ai_cost_usd")
	if abs(got-0.9999) > 1e-6 {
		t.Errorf("expected provided cost 0.9999, got %.6f", got)
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
			CacheInTokens: 100, CacheOutTokens: 50,
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
