package command

import (
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

func itemWithTimeTracking(tt map[string]interface{}) *model.Item {
	return &model.Item{
		ID:           "T-300",
		Title:        "fixture",
		TimeTracking: tt,
	}
}

// fakeFloat builds an item that exercises every path ExtractItemMetrics
// reads, so a single fixture can verify all field translations.
func fixtureMetrics() *model.Item {
	now := time.Now().Add(-30 * time.Minute) // started 30m ago, still open
	return &model.Item{
		ID:    "T-300",
		Title: "Test item",
		TimeTracking: map[string]interface{}{
			"started_at":           now.Format(time.RFC3339),
			"process_time_seconds": float64(45),
			"ai_time_seconds":      float64(20),
			"ai_cost_usd":          float64(0.42),
			"total_input_tokens":   float64(12000),
			"total_output_tokens":  float64(2500),
		},
	}
}

func TestExtractItemMetricsOpenItem(t *testing.T) {
	item := fixtureMetrics()
	now := time.Now()
	m := ExtractItemMetrics(item, "", now, false)

	if m.Wall < 29*time.Minute || m.Wall > 31*time.Minute {
		t.Errorf("Wall = %v, want ~30m", m.Wall)
	}
	if m.ProcessTime != 45*time.Second {
		t.Errorf("ProcessTime = %v, want 45s", m.ProcessTime)
	}
	if m.AITime != 20*time.Second {
		t.Errorf("AITime = %v, want 20s", m.AITime)
	}
	if m.CostUSD != 0.42 {
		t.Errorf("CostUSD = %v, want 0.42", m.CostUSD)
	}
	if m.InputTokens != 12000 {
		t.Errorf("InputTokens = %d, want 12000", m.InputTokens)
	}
	if m.OutputTokens != 2500 {
		t.Errorf("OutputTokens = %d, want 2500", m.OutputTokens)
	}
}

func TestExtractItemMetricsClosedItem(t *testing.T) {
	started := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	completed := started.Add(2 * time.Hour)
	item := &model.Item{
		ID: "T-301",
		TimeTracking: map[string]interface{}{
			"started_at":   started.Format(time.RFC3339),
			"completed_at": completed.Format(time.RFC3339),
			"ai_cost_usd":  float64(1.25),
		},
	}
	m := ExtractItemMetrics(item, "", time.Now(), true)
	if m.Wall != 2*time.Hour {
		t.Errorf("closed Wall = %v, want 2h", m.Wall)
	}
	if m.CostUSD != 1.25 {
		t.Errorf("CostUSD = %v, want 1.25", m.CostUSD)
	}
}

func TestExtractItemMetricsLegacyFields(t *testing.T) {
	// Older items wrote run_wall_seconds / ai_duration_seconds /
	// input_tokens / output_tokens. Extractor must fall back.
	item := itemWithTimeTracking(map[string]interface{}{
		"run_wall_seconds":     float64(60),
		"ai_duration_seconds":  float64(30),
		"input_tokens":         float64(800),
		"output_tokens":        float64(150),
	})
	m := ExtractItemMetrics(item, "", time.Now(), false)
	if m.ProcessTime != 60*time.Second {
		t.Errorf("legacy ProcessTime = %v, want 60s", m.ProcessTime)
	}
	if m.AITime != 30*time.Second {
		t.Errorf("legacy AITime = %v, want 30s", m.AITime)
	}
	if m.InputTokens != 800 {
		t.Errorf("legacy InputTokens = %d, want 800", m.InputTokens)
	}
	if m.OutputTokens != 150 {
		t.Errorf("legacy OutputTokens = %d, want 150", m.OutputTokens)
	}
}

func TestHasMetricsZeroValue(t *testing.T) {
	var m ItemMetrics
	if m.HasMetrics() {
		t.Error("zero ItemMetrics reports HasMetrics = true")
	}
}

func TestHasMetricsAnyField(t *testing.T) {
	cases := []ItemMetrics{
		{Wall: time.Second},
		{ProcessTime: time.Second},
		{AITime: time.Second},
		{CostUSD: 0.01},
		{InputTokens: 1},
		{OutputTokens: 1},
		{CacheReadTokens: 1},
		{CacheWriteTokens: 1},
		{NetLOC: 1},
		{NetLOC: -1},
	}
	for i, m := range cases {
		if !m.HasMetrics() {
			t.Errorf("case %d: HasMetrics = false, want true", i)
		}
	}
}

func TestHasMetricsModelAloneIsFalse(t *testing.T) {
	// Model alone (no spend, no LOC) is not a useful metric line.
	m := ItemMetrics{Model: "claude-sonnet-4-6"}
	if m.HasMetrics() {
		t.Error("model-only ItemMetrics reports HasMetrics = true, want false")
	}
}

func TestFormatLineEmpty(t *testing.T) {
	var m ItemMetrics
	if got := m.FormatLine(); got != "" {
		t.Errorf("empty FormatLine = %q, want ''", got)
	}
}

func TestFormatLineFull(t *testing.T) {
	m := ItemMetrics{
		Wall:    14 * time.Minute,
		CostUSD: 0.12,
		NetLOC:  90,
	}
	got := m.FormatLine()
	for _, want := range []string{"14m", "$0.12", "+90"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatLine = %q, missing %q", got, want)
		}
	}
	// Pipe-separated sections.
	if !strings.Contains(got, " | ") {
		t.Errorf("FormatLine = %q, expected ' | ' separator", got)
	}
}

func TestFormatLineShowsLOCAndTokensTogether(t *testing.T) {
	// LOC and tokens are independent — both appear when both are present.
	m := ItemMetrics{Wall: time.Minute, NetLOC: 50, InputTokens: 1000, OutputTokens: 200}
	got := m.FormatLine()
	if !strings.Contains(got, "+50") {
		t.Errorf("FormatLine = %q, missing LOC", got)
	}
	if !strings.Contains(got, "tok") {
		t.Errorf("FormatLine = %q, missing tokens when both LOC and tokens present", got)
	}
}

func TestFormatLineFallsBackToTokens(t *testing.T) {
	m := ItemMetrics{Wall: time.Minute, InputTokens: 1500, OutputTokens: 300}
	got := m.FormatLine()
	if !strings.Contains(got, "tok") {
		t.Errorf("FormatLine = %q, expected tokens fallback when no LOC", got)
	}
}

func TestFormatColumnsFull(t *testing.T) {
	m := ItemMetrics{
		Wall:         time.Hour,
		ProcessTime:  30 * time.Minute,
		AITime:       10 * time.Minute,
		CostUSD:      2.50,
		InputTokens:  10000,
		OutputTokens: 2000,
		NetLOC:       -120,
	}
	c := m.FormatColumns()
	if c.Wall == "" || c.ProcessTime == "" || c.AITime == "" || c.Cost == "" || c.Tokens == "" || c.LOC == "" {
		t.Errorf("FormatColumns left a field empty: %+v", c)
	}
	if !strings.Contains(c.Tokens, "/") {
		t.Errorf("Tokens column = %q, expected I/O/T tri-format", c.Tokens)
	}
}

func TestFormatColumnsEmpty(t *testing.T) {
	var m ItemMetrics
	c := m.FormatColumns()
	if c.Wall != "" || c.Cost != "" || c.LOC != "" {
		t.Errorf("zero-value FormatColumns produced non-empty fields: %+v", c)
	}
}

func TestItemMetricsAdd(t *testing.T) {
	a := ItemMetrics{Wall: time.Hour, CostUSD: 1.5, InputTokens: 100, NetLOC: 50,
		CacheReadTokens: 500, CacheWriteTokens: 200, Model: "claude-sonnet-4-6"}
	b := ItemMetrics{Wall: 30 * time.Minute, CostUSD: 0.5, InputTokens: 200, NetLOC: -10,
		CacheReadTokens: 300, CacheWriteTokens: 100, Model: "claude-opus-4-7"}
	sum := a.Add(b)
	if sum.Wall != 90*time.Minute {
		t.Errorf("sum.Wall = %v, want 90m", sum.Wall)
	}
	if sum.CostUSD != 2.0 {
		t.Errorf("sum.CostUSD = %v, want 2.0", sum.CostUSD)
	}
	if sum.InputTokens != 300 {
		t.Errorf("sum.InputTokens = %d, want 300", sum.InputTokens)
	}
	if sum.NetLOC != 40 {
		t.Errorf("sum.NetLOC = %d, want 40", sum.NetLOC)
	}
	if sum.CacheReadTokens != 800 {
		t.Errorf("sum.CacheReadTokens = %d, want 800", sum.CacheReadTokens)
	}
	if sum.CacheWriteTokens != 300 {
		t.Errorf("sum.CacheWriteTokens = %d, want 300", sum.CacheWriteTokens)
	}
	if sum.Model != "" {
		t.Errorf("sum.Model = %q, want empty (aggregates have no model)", sum.Model)
	}
}

func TestExtractItemMetricsNilItem(t *testing.T) {
	m := ExtractItemMetrics(nil, "", time.Now(), false)
	if m.HasMetrics() {
		t.Error("nil item produced metrics")
	}
}

// TestStatusDashboardRendersMetricLine verifies that the active-work loop
// in statusDashboard emits the FormatLine() row under items that have
// time_tracking. The fixture's T-003 is already active; mutating it to
// add ai_cost_usd + reg tokens should make the metric line appear.
func TestStatusDashboardRendersMetricLine(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-003", func(it *model.Item) error {
		started := time.Now().Add(-30 * time.Minute).Format(time.RFC3339)
		it.SetNested("time_tracking", "started_at", started)
		it.SetNested("time_tracking", "ai_cost_usd", "0.420000")
		it.SetNested("time_tracking", "reg_input_tokens", "12000")
		it.SetNested("time_tracking", "reg_output_tokens", "2500")
		return nil
	}); err != nil {
		t.Fatalf("seed T-003 metrics: %v", err)
	}

	// Reload so the typed TimeTracking map is repopulated from the parsed file.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}

	out := captureStdout(t, func() {
		Status(s2, cfg, "", StatusOpts{NoRefresh: true})
	})

	if !strings.Contains(out, "T-003") {
		t.Fatalf("dashboard missing T-003:\n%s", out)
	}
	if !strings.Contains(out, "$0.42") {
		t.Errorf("dashboard missing cost on T-003:\n%s", out)
	}
	// T-001 and T-002 have no time_tracking — no metric line should appear
	// for them. We can't directly assert "no line under T-001" without
	// parsing, so check that the only $... in the output is T-003's $0.42.
	costCount := strings.Count(out, "$0.")
	if costCount != 1 {
		t.Errorf("expected 1 cost marker (T-003 only), got %d:\n%s", costCount, out)
	}
}

// TestStatusSingleRendersMetricLine: `st status T-003` emits a "Metrics:"
// line when time_tracking is populated.
func TestStatusSingleRendersMetricLine(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "ai_cost_usd", "1.500000")
		it.SetNested("time_tracking", "reg_input_tokens", "5000")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	out := captureStdout(t, func() {
		Status(s2, cfg, "T-003", StatusOpts{NoRefresh: true})
	})
	if !strings.Contains(out, "Metrics:") || !strings.Contains(out, "$1.50") {
		t.Errorf("statusSingle did not render Metrics line:\n%s", out)
	}
}

// TestStatusSingleNoMetricLineWhenAbsent: items without time_tracking
// should not see a "Metrics:" line — keeping the surface clean.
func TestStatusSingleNoMetricLineWhenAbsent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Status(s, cfg, "T-001", StatusOpts{NoRefresh: true})
	})
	if strings.Contains(out, "Metrics:") {
		t.Errorf("statusSingle rendered Metrics line for item with no time_tracking:\n%s", out)
	}
}

func TestFormatLineIncludesModelLabel(t *testing.T) {
	m := ItemMetrics{Wall: time.Minute, CostUSD: 0.10, Model: "claude-sonnet-4-6"}
	got := m.FormatLine()
	if !strings.Contains(got, "[sonnet-4.6]") {
		t.Errorf("FormatLine = %q, missing model label [sonnet-4.6]", got)
	}
}

func TestFormatLineIncludesCacheAnnotation(t *testing.T) {
	m := ItemMetrics{
		Wall:             time.Minute,
		InputTokens:      50000,
		OutputTokens:     5000,
		CacheReadTokens:  80000,
		CacheWriteTokens: 2000,
	}
	got := m.FormatLine()
	if !strings.Contains(got, "cached") {
		t.Errorf("FormatLine = %q, missing cache annotation", got)
	}
	// With zero cache, no annotation
	m2 := ItemMetrics{Wall: time.Minute, InputTokens: 50000, OutputTokens: 5000}
	got2 := m2.FormatLine()
	if strings.Contains(got2, "cache") {
		t.Errorf("FormatLine = %q, should not have cache annotation when cache is zero", got2)
	}
}

func TestExtractItemMetricsReadsModelAndCache(t *testing.T) {
	// Path 1: real_tokens blob (canonical I-569)
	item := itemWithTimeTracking(map[string]interface{}{
		"started_at":   time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
		"real_tokens":  "input=5000 output=1000 cache_read=40000 cache_creation_5m=2000 cache_creation_1h=1000",
		"last_model":   "claude-sonnet-4-6",
	})
	m := ExtractItemMetrics(item, "", time.Now(), false)
	if m.CacheReadTokens != 40000 {
		t.Errorf("CacheReadTokens = %d, want 40000", m.CacheReadTokens)
	}
	if m.CacheWriteTokens != 3000 {
		t.Errorf("CacheWriteTokens = %d, want 3000 (2000+1000)", m.CacheWriteTokens)
	}
	if m.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", m.Model)
	}

	// Path 2: legacy cache_in_tokens / cache_out_tokens (no real_tokens blob)
	item2 := itemWithTimeTracking(map[string]interface{}{
		"started_at":        time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
		"reg_input_tokens":  float64(3000),
		"reg_output_tokens": float64(800),
		"cache_in_tokens":   float64(12000),
		"cache_out_tokens":  float64(500),
		"cache_out_1h_tokens": float64(200),
		"last_model":        "claude-opus-4-7",
	})
	m2 := ExtractItemMetrics(item2, "", time.Now(), false)
	if m2.CacheReadTokens != 12000 {
		t.Errorf("legacy CacheReadTokens = %d, want 12000", m2.CacheReadTokens)
	}
	if m2.CacheWriteTokens != 700 {
		t.Errorf("legacy CacheWriteTokens = %d, want 700 (500+200)", m2.CacheWriteTokens)
	}
	if m2.Model != "claude-opus-4-7" {
		t.Errorf("legacy Model = %q, want claude-opus-4-7", m2.Model)
	}
}

func TestShortModelLabel(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"claude-opus-4-7", "opus-4.7"},
		{"claude-sonnet-4-6", "sonnet-4.6"},
		{"claude-haiku-4-5", "haiku-4.5"},
		{"claude-haiku-4-5-20251001", "haiku-4.5"},
		{"claude-opus-4", "opus-4"},
		{"gpt-5.2", "gpt-5.2"},
		{"", ""},
		{"claude-sonnet-4-6-20260101", "sonnet-4.6"},
	}
	for _, c := range cases {
		got := shortModelLabel(c.input)
		if got != c.want {
			t.Errorf("shortModelLabel(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestPipelineStepLabelDoneAfterFinalStep guards finding #2 of the PR #41
// review: when the pipeline is fully complete (last_completed_step matches
// the final step name), the label must not echo the final step as if it
// were still pending — should say "done" instead.
func TestPipelineStepLabelDoneAfterFinalStep(t *testing.T) {
	steps := []string{"build", "test", "merge", "deploy"}

	doneItem := &model.Item{Doc: nil}
	// Use SetNested via a real item so getNestedField finds delivery fields.
	d := &model.Item{}
	d.SetNested("delivery", "last_completed_step", "deploy")
	if got := pipelineStepLabel(d, steps); !strings.HasPrefix(got, "done") {
		t.Errorf("pipelineStepLabel after final step = %q, want 'done...'", got)
	}

	// Sanity: a still-in-progress item should NOT report "done".
	mid := &model.Item{}
	mid.SetNested("delivery", "last_completed_step", "build")
	if got := pipelineStepLabel(mid, steps); got == "done" || strings.HasPrefix(got, "done · ") {
		t.Errorf("pipelineStepLabel after build = %q, expected next step (test)", got)
	}

	// And a clamp still kicks in cleanly when no last_completed_step is set —
	// should return the first step, not "done".
	first := &model.Item{}
	if got := pipelineStepLabel(first, steps); got != "build" {
		t.Errorf("pipelineStepLabel with no last_completed_step = %q, want 'build'", got)
	}

	_ = doneItem
}
