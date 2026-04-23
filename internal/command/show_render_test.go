package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/testutil"
)

func TestRenderTimeTracking_QuietWhenNoMetrics(t *testing.T) {
	env := testutil.NewEnv(t)
	item, _ := env.S.Get("T-003") // no metrics seeded
	buf := &bytes.Buffer{}
	renderTimeTracking(buf, item)
	if buf.Len() != 0 {
		t.Errorf("expected empty output for metric-less item; got:\n%s", buf.String())
	}
}

func TestRenderTimeTracking_ShowsAll1hCacheBuckets(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// Mixed 5m + 1h writes so the split path renders both
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		ProcessMs: 10_000, AIMs: 8_000,
		RegInputTokens: 1_000, RegOutputTokens: 500,
		CacheInTokens: 50_000, CacheOutTokens: 2_000, CacheOut1hTokens: 1_000,
	})
	env.Reload(t)
	item, _ := env.S.Get("T-003")

	buf := &bytes.Buffer{}
	renderTimeTracking(buf, item)
	out := buf.String()

	for _, want := range []string{
		"time_tracking:",
		"process: ",
		"cost: $",
		"tokens:",
		"cache: ",
		// Both split buckets must appear in the display
		"5m",
		"1h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTimeTracking missing %q; full output:\n%s", want, out)
		}
	}

	// cost uses %.6f — should contain 6 decimal digits after the dollar sign
	if !strings.Contains(out, "cost: $") {
		t.Fatalf("expected cost line, got:\n%s", out)
	}
	costLine := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "cost: $") {
			costLine = ln
			break
		}
	}
	// Find "$" then count digits after decimal
	dollarIdx := strings.Index(costLine, "$")
	dotIdx := strings.Index(costLine[dollarIdx:], ".")
	if dotIdx < 0 {
		t.Fatalf("cost missing decimal: %s", costLine)
	}
	// Count digits after the dot until non-digit
	tail := costLine[dollarIdx+dotIdx+1:]
	digits := 0
	for _, c := range tail {
		if c < '0' || c > '9' {
			break
		}
		digits++
	}
	if digits != 6 {
		t.Errorf("expected 6 decimal digits in cost, got %d in %q", digits, costLine)
	}
}

func TestRenderTimeTracking_5mOnlyOmits1hFromDisplay(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})

	// No 1h writes — keeps the legacy display form
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "s", Model: "claude-haiku-4-5",
		RegInputTokens: 100, CacheOutTokens: 50,
	})
	env.Reload(t)
	item, _ := env.S.Get("T-003")

	buf := &bytes.Buffer{}
	renderTimeTracking(buf, item)
	out := buf.String()

	// The "= N 5m + N 1h" split annotation should NOT appear when only 5m was written
	if strings.Contains(out, "1h") {
		t.Errorf("should not show 1h split when only 5m was recorded; got:\n%s", out)
	}
	if !strings.Contains(out, "cache: ") {
		t.Errorf("expected legacy cache display; got:\n%s", out)
	}
}

func TestRenderTimeStats_TotalsIncludeBothCacheTiers(t *testing.T) {
	// Seed two items: one with 5m+1h, one with only 5m
	env := testutil.NewEnv(t)

	SaveStack(env.Cfg, []StackEntry{{ID: "T-003"}})
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "a", Model: "claude-opus-4-7",
		RegInputTokens: 1000, RegOutputTokens: 500,
		CacheOutTokens: 1_000, CacheOut1hTokens: 2_000,
	})

	SaveStack(env.Cfg, []StackEntry{{ID: "T-001"}})
	SessionLog(env.S, env.Cfg, SessionLogPayload{
		SessionID: "b", Model: "claude-haiku-4-5",
		RegInputTokens: 200, RegOutputTokens: 100,
		CacheOutTokens: 500, // no 1h
	})

	env.Reload(t)
	t2 := computeTimeStats(env.S)

	if t2.ItemsWithMetrics != 2 {
		t.Errorf("items_with_metrics = %d, want 2", t2.ItemsWithMetrics)
	}
	// 5m total: 1000 + 500 = 1500
	if t2.TotalCacheOut != 1500 {
		t.Errorf("TotalCacheOut = %d, want 1500", t2.TotalCacheOut)
	}
	// 1h total: only the opus item contributed → 2000
	if t2.TotalCacheOut1h != 2000 {
		t.Errorf("TotalCacheOut1h = %d, want 2000", t2.TotalCacheOut1h)
	}

	buf := &bytes.Buffer{}
	renderTimeStats(buf, t2)
	out := buf.String()

	// Must show the split annotation in the cache-writes line
	if !strings.Contains(out, "5m") || !strings.Contains(out, "1h") {
		t.Errorf("renderTimeStats missing 5m/1h annotation; got:\n%s", out)
	}
	// Per-model section present for two distinct models
	if !strings.Contains(out, "By model:") {
		t.Errorf("expected By model: section; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-opus-4-7") {
		t.Errorf("opus model missing from by-model table; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-haiku-4-5") {
		t.Errorf("haiku model missing from by-model table; got:\n%s", out)
	}
}

func TestRenderTimeStats_ZeroItemsShortCircuits(t *testing.T) {
	buf := &bytes.Buffer{}
	renderTimeStats(buf, &timeStats{ItemsWithMetrics: 0})
	out := buf.String()
	if !strings.Contains(out, "Items with metrics: 0") {
		t.Errorf("expected zero-items preamble; got:\n%s", out)
	}
	if strings.Contains(out, "Total cost") {
		t.Errorf("should not render totals when zero items; got:\n%s", out)
	}
}

// Regression: the per-turn ai_turns line must emit cache_out_1h when >0 so
// downstream readers can reconstruct the split. (Finding #3 on PR #14.)
func TestFormatAITurnLine_Emits1hBucketWhenNonZero(t *testing.T) {
	line := formatAITurnLine(SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7",
		CacheOutTokens: 500, CacheOut1hTokens: 300,
	}, 0.1, "2026-04-23T12:00:00-07:00")
	if !strings.Contains(line, "cache_out:500") {
		t.Errorf("missing cache_out:500 in %s", line)
	}
	if !strings.Contains(line, "cache_out_1h:300") {
		t.Errorf("missing cache_out_1h:300 in %s", line)
	}

	// When 1h is zero, the token should NOT appear (legacy form preserved)
	lineNo1h := formatAITurnLine(SessionLogPayload{
		SessionID: "s", Model: "claude-opus-4-7", CacheOutTokens: 500,
	}, 0.1, "2026-04-23T12:00:00-07:00")
	if strings.Contains(lineNo1h, "cache_out_1h:") {
		t.Errorf("should omit cache_out_1h when zero, got: %s", lineNo1h)
	}
}
