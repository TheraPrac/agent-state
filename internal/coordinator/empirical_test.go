package coordinator

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// empItem constructs a model.Item whose ai_turns list contains one entry per
// session in sessions (sid → per-session cost). status and typ control which
// size-class bin the item lands in.
func empItem(id, typ, status string, priority *int, sessions map[string]float64) *model.Item {
	lines := []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id, Indent: 0},
		{Raw: "type: " + typ, Key: "type", Value: typ, Indent: 0},
		{Raw: "status: " + status, Key: "status", Value: status, Indent: 0},
		{Raw: "time_tracking:", Key: "time_tracking", Indent: 0},
		{Raw: "  ai_turns:", Key: "ai_turns", Indent: 2},
	}
	for sid, cost := range sessions {
		entry := fmt.Sprintf(
			"session:%s model:claude-sonnet-4-6 cost:$%.6f reg_in:1000 reg_out:200 cache_in:0 cache_out:0 step:interactive at:2026-01-01T00:00:00Z",
			sid, cost)
		lines = append(lines, model.Line{Raw: "  - " + entry, Indent: 4})
	}
	return &model.Item{
		ID:       id,
		Type:     typ,
		Status:   status,
		Priority: priority,
		Doc:      &model.ParsedDocument{Lines: lines},
	}
}

func pint(n int) *int { return &n }

// captureEmpStderr captures output written to os.Stderr during fn.
func captureEmpStderr(fn func()) string {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestLoadEmpiricalBaselines(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// 5 done task:lo items (priority 2), each with one session at $7.
	var items []*model.Item
	for i := 0; i < 5; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 7.0}))
	}

	LoadEmpiricalBaselines(items, b)

	// task:lo baseline should be the empirical median $7, not the heuristic $10.
	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 7.0 {
		t.Errorf("task:lo got $%g, want $7.0 (empirical median)", got)
	}
	if n := EmpiricalSamplesForBin(CostBinKey(taskLo)); n != 5 {
		t.Errorf("EmpiricalSamplesForBin = %d, want 5", n)
	}
}

func TestLoadEmpiricalBaselines_NBelowFiveFallback(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// Only 4 samples — below the N≥5 floor.
	var items []*model.Item
	for i := 0; i < 4; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 7.0}))
	}
	LoadEmpiricalBaselines(items, b)

	// Must fall back to heuristic ($10 for task:lo).
	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 10.0 {
		t.Errorf("N=4 → want heuristic $10.0, got $%g", got)
	}
	if n := EmpiricalSamplesForBin(CostBinKey(taskLo)); n != 0 {
		t.Errorf("EmpiricalSamplesForBin = %d, want 0 (bin absent)", n)
	}
}

func TestLoadEmpiricalBaselines_K1InvariantBreachFallback(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	// K2=3, K1=40: median $14 × 3 = $42 ≥ $40 → breach → heuristic fallback.
	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	var items []*model.Item
	for i := 0; i < 5; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 14.0}))
	}
	LoadEmpiricalBaselines(items, b)

	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 10.0 {
		t.Errorf("K1-breach → want heuristic $10.0, got $%g", got)
	}
	if n := EmpiricalSamplesForBin(CostBinKey(taskLo)); n != 0 {
		t.Errorf("EmpiricalSamplesForBin = %d, want 0 (bin dropped)", n)
	}
}

func TestLoadEmpiricalBaselines_UsesBoundaryK2(t *testing.T) {
	// Median $11 × K2=3 = $33 < $40 → kept. Median $11 × K2=4 = $44 ≥ $40 → dropped.
	var items []*model.Item
	for i := 0; i < 5; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 11.0}))
	}

	taskLo := &model.Item{Type: "task", Priority: pint(2)}

	// K2=3: empirical kept.
	ResetEmpiricalForTest()
	LoadEmpiricalBaselines(items, &Boundary{StuckMultiplier: 3, PerItemUSD: 40})
	gotK3 := SizeClassCostBaseline(taskLo)

	// K2=4: empirical dropped → heuristic.
	ResetEmpiricalForTest()
	LoadEmpiricalBaselines(items, &Boundary{StuckMultiplier: 4, PerItemUSD: 40})
	gotK4 := SizeClassCostBaseline(taskLo)
	ResetEmpiricalForTest()

	if gotK3 != 11.0 {
		t.Errorf("K2=3: got $%g, want $11.0 (empirical kept)", gotK3)
	}
	if gotK4 != 10.0 {
		t.Errorf("K2=4: got $%g, want $10.0 (heuristic — breach at K2=4)", gotK4)
	}
}

func TestLoadEmpiricalBaselines_FiltersNonDoneAndZeroCost(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// One non-done item, one done item with a zero-cost session, two done items
	// with real costs → total real samples = 2 (below floor of 5).
	items := []*model.Item{
		empItem("T-001", "task", "active", pint(2), map[string]float64{"sA": 7.0}),
		empItem("T-002", "task", "done", pint(2), map[string]float64{"sB": 0.0}),
		empItem("T-003", "task", "done", pint(2), map[string]float64{"sC": 7.0}),
		empItem("T-004", "task", "done", pint(2), map[string]float64{"sD": 7.0}),
	}
	LoadEmpiricalBaselines(items, b)

	// Only 2 real samples → N<5 → heuristic.
	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 10.0 {
		t.Errorf("filtered samples → want heuristic $10.0, got $%g", got)
	}
}

func TestLoadEmpiricalBaselines_FiltersMalformedCost(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// Build items with some malformed cost entries alongside good ones.
	good := func(sid string, cost float64) model.Line {
		entry := fmt.Sprintf("session:%s model:claude-sonnet-4-6 cost:$%.6f reg_in:100 reg_out:50 cache_in:0 cache_out:0 step:interactive at:2026-01-01T00:00:00Z", sid, cost)
		return model.Line{Raw: "  - " + entry, Indent: 4}
	}
	bad := func(sid, costField string) model.Line {
		entry := fmt.Sprintf("session:%s model:claude-sonnet-4-6 %s reg_in:100 cache_in:0 step:interactive at:2026-01-01T00:00:00Z", sid, costField)
		return model.Line{Raw: "  - " + entry, Indent: 4}
	}
	header := []model.Line{
		{Raw: "type: task", Key: "type", Value: "task", Indent: 0},
		{Raw: "status: done", Key: "status", Value: "done", Indent: 0},
		{Raw: "time_tracking:", Key: "time_tracking", Indent: 0},
		{Raw: "  ai_turns:", Key: "ai_turns", Indent: 2},
	}
	makeItem := func(id string, lines ...model.Line) *model.Item {
		all := append(append([]model.Line{{Raw: "id: " + id, Key: "id", Indent: 0}}, header...), lines...)
		return &model.Item{ID: id, Type: "task", Status: "done", Priority: pint(2), Doc: &model.ParsedDocument{Lines: all}}
	}
	items := []*model.Item{
		// Item with one good turn + one malformed — only the good turn counts.
		makeItem("T-001", good("s1a", 7.0), bad("s1b", "cost:notanumber")),
		makeItem("T-002", good("s2a", 7.0), bad("s2b", "cost:missing-dollar")), // no $ prefix
		makeItem("T-003", good("s3a", 7.0), bad("s3b", "")),                    // no cost field at all
		makeItem("T-004", good("s4a", 7.0)),
		makeItem("T-005", good("s5a", 7.0)),
	}
	LoadEmpiricalBaselines(items, b)

	// 5 good samples → empirical median = $7.
	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 7.0 {
		t.Errorf("malformed entries filtered, 5 good → want $7.0, got $%g", got)
	}
}

func TestLoadEmpiricalBaselines_LogsPerBin(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// task:lo: 5 samples (good) → "median=$X (N=5)"
	// issue:lo: 2 samples → "heuristic-fallback (N=2 < 5)"
	var items []*model.Item
	for i := 0; i < 5; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("ts%d", i): 7.0}))
	}
	for i := 0; i < 2; i++ {
		items = append(items, empItem(fmt.Sprintf("I-%03d", i), "issue", "done", pint(2),
			map[string]float64{fmt.Sprintf("is%d", i): 4.0}))
	}

	logged := captureEmpStderr(func() {
		LoadEmpiricalBaselines(items, b)
	})

	if !strings.Contains(logged, "task:lo") {
		t.Errorf("expected 'task:lo' in log output:\n%s", logged)
	}
	if !strings.Contains(logged, "issue:lo") {
		t.Errorf("expected 'issue:lo' in log output:\n%s", logged)
	}
	if !strings.Contains(logged, "median=$") {
		t.Errorf("expected 'median=$' in log output:\n%s", logged)
	}
	if !strings.Contains(logged, "heuristic-fallback") {
		t.Errorf("expected 'heuristic-fallback' in log output:\n%s", logged)
	}
	if !strings.Contains(logged, "N=5") {
		t.Errorf("expected 'N=5' in log output:\n%s", logged)
	}
}

func TestLoadEmpiricalBaselines_Idempotent(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	first := make([]*model.Item, 5)
	for i := range first {
		first[i] = empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 7.0})
	}
	second := make([]*model.Item, 5)
	for i := range second {
		second[i] = empItem(fmt.Sprintf("T-%03d", 10+i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", 10+i): 20.0})
	}

	LoadEmpiricalBaselines(first, b)
	// Second call must be a no-op (sync.Once).
	LoadEmpiricalBaselines(second, b)

	taskLo := &model.Item{Type: "task", Priority: pint(2)}
	if got := SizeClassCostBaseline(taskLo); got != 7.0 {
		t.Errorf("second call must not overwrite first: got $%g, want $7.0", got)
	}
	if n := EmpiricalSamplesForBin(CostBinKey(taskLo)); n != 5 {
		t.Errorf("EmpiricalSamplesForBin = %d after second call, want 5 (first call only)", n)
	}
}

func TestSizeClassCostBaseline_EmpiricalThenHeuristic(t *testing.T) {
	ResetEmpiricalForTest()
	t.Cleanup(ResetEmpiricalForTest)

	b := &Boundary{StuckMultiplier: 3, PerItemUSD: 40}
	// Populate task:lo with 5 samples at $7 (empirical wins).
	var items []*model.Item
	for i := 0; i < 5; i++ {
		items = append(items, empItem(fmt.Sprintf("T-%03d", i), "task", "done", pint(2),
			map[string]float64{fmt.Sprintf("sid%d", i): 7.0}))
	}
	LoadEmpiricalBaselines(items, b)

	// task:lo → empirical $7 (not heuristic $10).
	if got := SizeClassCostBaseline(&model.Item{Type: "task", Priority: pint(2)}); got != 7.0 {
		t.Errorf("task:lo empirical: got $%g, want $7.0", got)
	}
	// issue:hi → no empirical data → heuristic $3.
	if got := SizeClassCostBaseline(&model.Item{Type: "issue", Priority: pint(0)}); got != 3.0 {
		t.Errorf("issue:hi heuristic: got $%g, want $3.0", got)
	}
	// issue:lo → no empirical data → heuristic $4.
	if got := SizeClassCostBaseline(&model.Item{Type: "issue", Priority: pint(2)}); got != 4.0 {
		t.Errorf("issue:lo heuristic: got $%g, want $4.0", got)
	}
	// task:hi → no empirical data → heuristic $6.
	if got := SizeClassCostBaseline(&model.Item{Type: "task", Priority: pint(0)}); got != 6.0 {
		t.Errorf("task:hi heuristic: got $%g, want $6.0", got)
	}
}
