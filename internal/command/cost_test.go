package command

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Cost depends on EstimateItemCostUSD which reads time_tracking.real_tokens
// and time_tracking.last_model. Build fixtures with those populated.

// real_tokens is stored as a single-line blob ("input=N output=N ..."),
// not nested YAML — see formatRealTokensBlob in session_log_schema.go.
const costItemWithTokens = `id: I-900
type: issue
title: Item with real tokens
status: queued
assigned_to: agent-g
created: 2026-05-01T00:00:00-06:00
last_touched: 2026-05-23T10:00:00-06:00

sbar:
  situation: x

time_tracking:
  last_model: claude-sonnet-4-6
  real_tokens: input=100000 output=5000 cache_read=50000 cache_creation_5m=10000 cache_creation_1h=0
`

const costItemNoTokens = `id: I-901
type: issue
title: Item with no token log yet
status: queued
assigned_to: agent-a
created: 2026-05-01T00:00:00-06:00
last_touched: 2026-05-23T10:00:00-06:00

sbar:
  situation: y
`

const costItemOpusBigSpender = `id: I-902
type: issue
title: Opus expensive item
status: active
assigned_to: agent-g
created: 2026-05-01T00:00:00-06:00
last_touched: 2026-05-23T18:00:00-06:00

sbar:
  situation: z

time_tracking:
  last_model: claude-opus-4-7
  real_tokens: input=200000 output=30000 cache_read=100000 cache_creation_5m=50000 cache_creation_1h=0
`

func TestCost_NoItemsInScope(t *testing.T) {
	root := modelRecTestEnv(t)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	exit := Cost(s, cfg, CostOpts{}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.Contains(out.String(), "no items with logged token usage") {
		t.Errorf("want 'no items with logged token usage', got %q", out.String())
	}
}

func TestCost_AggregatesAllOpenItems(t *testing.T) {
	root := modelRecTestEnv(t)
	writeItemFile(t, root, "issues", "I-900", costItemWithTokens)
	writeItemFile(t, root, "issues", "I-901", costItemNoTokens)
	writeItemFile(t, root, "issues", "I-902", costItemOpusBigSpender)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	exit := Cost(s, cfg, CostOpts{}, &out)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	got := out.String()

	// I-900 (sonnet) and I-902 (opus) have token usage → both in output.
	// I-901 (no tokens) → skipped.
	if !strings.Contains(got, "I-900") {
		t.Errorf("want I-900 in output, got %q", got)
	}
	if !strings.Contains(got, "I-902") {
		t.Errorf("want I-902 in output, got %q", got)
	}
	if strings.Contains(got, "I-901") {
		t.Errorf("did NOT want I-901 (no tokens), got %q", got)
	}
	if !strings.Contains(got, "TOTAL:") {
		t.Errorf("want TOTAL line, got %q", got)
	}
	if !strings.Contains(got, "synthetic API cost estimate") {
		t.Errorf("want SyntheticCostLabel disclaimer, got %q", got)
	}
}

func TestCost_OpusFirstWhenBiggerSpender(t *testing.T) {
	root := modelRecTestEnv(t)
	writeItemFile(t, root, "issues", "I-900", costItemWithTokens)
	writeItemFile(t, root, "issues", "I-902", costItemOpusBigSpender)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	if exit := Cost(s, cfg, CostOpts{}, &out); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}

	// Opus (I-902) is ~10x sonnet (I-900) per token, with 2x the input. Should
	// appear before I-900 in the descending-by-cost sort.
	pos902 := strings.Index(out.String(), "I-902")
	pos900 := strings.Index(out.String(), "I-900")
	if pos902 == -1 || pos900 == -1 {
		t.Fatalf("both items should appear: %q", out.String())
	}
	if pos902 > pos900 {
		t.Errorf("opus (I-902) should sort before sonnet (I-900) by cost descending")
	}
}

func TestCost_FilterByItemID(t *testing.T) {
	root := modelRecTestEnv(t)
	writeItemFile(t, root, "issues", "I-900", costItemWithTokens)
	writeItemFile(t, root, "issues", "I-902", costItemOpusBigSpender)
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	if exit := Cost(s, cfg, CostOpts{ItemID: "I-900"}, &out); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	got := out.String()
	if !strings.Contains(got, "I-900") {
		t.Errorf("want I-900, got %q", got)
	}
	if strings.Contains(got, "I-902") {
		t.Errorf("did NOT want I-902 (filtered out), got %q", got)
	}
}

func TestCost_FilterByAgent(t *testing.T) {
	root := modelRecTestEnv(t)
	writeItemFile(t, root, "issues", "I-900", costItemWithTokens)        // agent-g
	writeItemFile(t, root, "issues", "I-901", costItemNoTokens)          // agent-a, no tokens
	writeItemFile(t, root, "issues", "I-902", costItemOpusBigSpender)    // agent-g
	s, cfg := loadStore(t, root)

	var out bytes.Buffer
	if exit := Cost(s, cfg, CostOpts{Agent: "agent-a"}, &out); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// agent-a's only item has no tokens → "no items" message.
	if !strings.Contains(out.String(), "no items") {
		t.Errorf("agent-a has no items with tokens, want 'no items', got %q", out.String())
	}

	var out2 bytes.Buffer
	if exit := Cost(s, cfg, CostOpts{Agent: "agent-g"}, &out2); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out2.String(), "I-900") || !strings.Contains(out2.String(), "I-902") {
		t.Errorf("agent-g should show both I-900 and I-902, got %q", out2.String())
	}
}

func TestCost_FilterBySince(t *testing.T) {
	root := modelRecTestEnv(t)
	writeItemFile(t, root, "issues", "I-900", costItemWithTokens)     // last_touched 2026-05-23T10
	writeItemFile(t, root, "issues", "I-902", costItemOpusBigSpender) // last_touched 2026-05-23T18
	s, cfg := loadStore(t, root)

	// Since 2026-05-23T15: only I-902 (last_touched at 18:00) should pass.
	since, _ := time.Parse(time.RFC3339, "2026-05-23T15:00:00-06:00")
	var out bytes.Buffer
	if exit := Cost(s, cfg, CostOpts{Since: since}, &out); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	got := out.String()
	if !strings.Contains(got, "I-902") {
		t.Errorf("want I-902 (newer than --since), got %q", got)
	}
	if strings.Contains(got, "I-900") {
		t.Errorf("did NOT want I-900 (older than --since), got %q", got)
	}
}
