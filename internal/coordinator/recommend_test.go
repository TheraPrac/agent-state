package coordinator

import (
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

func pInt(n int) *int { return &n }

var refNow = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

func ids(recs []Recommendation) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Item.ID
	}
	return out
}

// Priority dominates BY CONSTRUCTION: a p1 with zero secondary signal must
// outrank a p2 with maximal leverage. This is the load-bearing invariant —
// recommend reasons, it never silently re-ranks past the priority primitive.
func TestRecommend_PriorityDominates(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(1), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	recs := Recommend([]*model.Item{b, a}, map[string]int{"T-B": 100}, nil, nil, nil, refNow)
	if got := ids(recs); got[0] != "T-A" {
		t.Fatalf("priority must dominate: got %v, want T-A first", got)
	}
}

// Within one priority band, higher unblock leverage ranks first.
func TestRecommend_LeverageOrdersWithinBand(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	recs := Recommend([]*model.Item{a, b},
		map[string]int{"T-A": 1, "T-B": 5}, nil, nil, nil, refNow)
	if got := ids(recs); got[0] != "T-B" {
		t.Fatalf("higher leverage should lead in-band: got %v", got)
	}
}

// Sprint completion pressure contributes only for an ACTIVE sprint, scaled
// by completion fraction; an inactive sprint contributes nothing.
func TestRecommend_SprintCompletionPressure(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Sprint: "s-hot", Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Sprint: "s-cold", Created: refNow}
	c := &model.Item{ID: "T-C", Priority: pInt(2), Sprint: "s-done", Created: refNow}
	sprints := map[string]SprintInfo{
		"s-hot":  {Active: true, CompletionFrac: 0.9},
		"s-cold": {Active: false, CompletionFrac: 1.0}, // inactive ⇒ no points
		"s-done": {Active: true, CompletionFrac: 0.1},
	}
	recs := Recommend([]*model.Item{b, c, a}, nil, sprints, nil, nil, refNow)
	if got := ids(recs); got[0] != "T-A" {
		t.Fatalf("near-done active sprint should lead: got %v", got)
	}
	// s-cold (inactive) must score == bare age, i.e. below s-done (active 10%).
	if got := ids(recs); got[2] != "T-B" {
		t.Fatalf("inactive sprint must not contribute: got %v, want T-B last", got)
	}
}

// Age is a bounded anti-starvation tiebreak: it breaks an otherwise exact
// tie, but the cap guarantees it can never cross a priority band.
func TestRecommend_AgeBoundedTiebreak(t *testing.T) {
	old := &model.Item{ID: "T-OLD", Priority: pInt(2),
		Created: refNow.Add(-20 * 24 * time.Hour)}
	fresh := &model.Item{ID: "T-NEW", Priority: pInt(2), Created: refNow}
	recs := Recommend([]*model.Item{fresh, old}, nil, nil, nil, nil, refNow)
	if got := ids(recs); got[0] != "T-OLD" {
		t.Fatalf("older item should win a pure tie: got %v", got)
	}

	// Even a 1000-day item (age capped to 30d ⇒ ≤1.5 pts) cannot beat a
	// strictly-higher-priority fresh item.
	ancient := &model.Item{ID: "T-ANC", Priority: pInt(2),
		Created: refNow.Add(-1000 * 24 * time.Hour)}
	hi := &model.Item{ID: "T-HI", Priority: pInt(1), Created: refNow}
	r2 := Recommend([]*model.Item{ancient, hi}, nil, nil, nil, nil, refNow)
	if got := ids(r2); got[0] != "T-HI" {
		t.Fatalf("capped age must not cross priority: got %v", got)
	}
}

// Determinism: a shuffled input yields an identical ranking, with the ID
// breaking exact ties (the deps.Ready precedent).
func TestRecommend_DeterministicAndIDTiebreak(t *testing.T) {
	mk := func(id string) *model.Item {
		return &model.Item{ID: id, Priority: pInt(2), Created: refNow}
	}
	a, b, c := mk("T-001"), mk("T-002"), mk("T-003")
	want := []string{"T-001", "T-002", "T-003"} // exact tie ⇒ ID asc

	o1 := ids(Recommend([]*model.Item{a, b, c}, nil, nil, nil, nil, refNow))
	o2 := ids(Recommend([]*model.Item{c, a, b}, nil, nil, nil, nil, refNow))
	o3 := ids(Recommend([]*model.Item{b, c, a}, nil, nil, nil, nil, refNow))
	for _, got := range [][]string{o1, o2, o3} {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("non-deterministic / wrong tiebreak: got %v want %v", got, want)
		}
	}
}

// The rationale is the sum of its labelled parts — never an opaque number.
func TestRecommend_RationaleDecomposed(t *testing.T) {
	it := &model.Item{ID: "T-A", Priority: pInt(1), Sprint: "s",
		Created: refNow.Add(-9 * 24 * time.Hour)}
	recs := Recommend([]*model.Item{it}, map[string]int{"T-A": 2},
		map[string]SprintInfo{"s": {Active: true, CompletionFrac: 0.6}}, nil, nil, refNow)
	rat := recs[0].Rationale()
	for _, want := range []string{"priority p1", "unblocks 2", "sprint s 60%", "age 9d"} {
		if !strings.Contains(rat, want) {
			t.Errorf("rationale %q missing %q", rat, want)
		}
	}
}

func TestRecommend_NilItemsSkipped(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	recs := Recommend([]*model.Item{nil, a, nil}, nil, nil, nil, nil, refNow)
	if len(recs) != 1 || recs[0].Item.ID != "T-A" {
		t.Fatalf("nil items must be skipped, got %v", ids(recs))
	}
}

// Goal weight orders items within a priority band.
func TestRecommend_GoalWeightOrdersWithinBand(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	// T-A has goal weight 20 (→ +10pts), T-B has none.
	recs := Recommend([]*model.Item{b, a}, nil, nil, map[string]float64{"T-A": 20}, nil, refNow)
	if got := ids(recs); got[0] != "T-A" {
		t.Fatalf("goal weight should lead in-band: got %v, want T-A first", got)
	}
}

// Goal weight cannot cross a priority band.
func TestRecommend_GoalWeightCannotCrossPriority(t *testing.T) {
	lo := &model.Item{ID: "T-LO", Priority: pInt(1), Created: refNow}
	hi := &model.Item{ID: "T-HI", Priority: pInt(2), Created: refNow}
	// Even max weight (100 → +50pts) on T-HI cannot beat a p1 item.
	recs := Recommend([]*model.Item{hi, lo}, nil, nil, map[string]float64{"T-HI": 100}, nil, refNow)
	if got := ids(recs); got[0] != "T-LO" {
		t.Fatalf("priority must dominate over goal weight: got %v, want T-LO first", got)
	}
}

// Goal weight appears in the rationale.
func TestRecommend_GoalWeightRationale(t *testing.T) {
	it := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	recs := Recommend([]*model.Item{it}, nil, nil, map[string]float64{"T-A": 40}, nil, refNow)
	rat := recs[0].Rationale()
	if !strings.Contains(rat, "goal-weight 40") {
		t.Errorf("rationale must contain goal-weight term, got: %q", rat)
	}
}

// Zero or nil goal weight map contributes nothing.
func TestRecommend_GoalWeightZeroWhenMapEmpty(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	// nil map — must be identical ranking to empty map
	r1 := Recommend([]*model.Item{a, b}, nil, nil, nil, nil, refNow)
	r2 := Recommend([]*model.Item{a, b}, nil, nil, map[string]float64{}, nil, refNow)
	if ids(r1)[0] != ids(r2)[0] {
		t.Fatalf("nil vs empty goalWeights should produce same result: %v vs %v", ids(r1), ids(r2))
	}
	// No "goal" factor in rationale when weight is zero.
	if strings.Contains(r1[0].Rationale(), "goal") {
		t.Errorf("zero goal weight must not appear in rationale: %q", r1[0].Rationale())
	}
}

// Priority override tests: overrides model transitive dependency inheritance
// (a p2 item unblocked by a p1 dep gets lifted). Pins are a separate score
// boost (pinWeight) applied within the band — they never cross bands.

// An item with a lower effective priority override (dep inheritance) beats a same-label item.
func TestRecommend_PriorityOverrideWins(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	// T-B has an effective priority of p1 (e.g. inherited from a p1 dependency).
	overrides := map[string]int{"T-B": 1}
	recs := Recommend([]*model.Item{a, b}, nil, nil, nil, overrides, refNow)
	if got := ids(recs); got[0] != "T-B" {
		t.Fatalf("priority override should lead: got %v, want T-B first", got)
	}
	if recs[0].Priority != 1 {
		t.Errorf("Recommendation.Priority should reflect effective value: got %d, want 1", recs[0].Priority)
	}
}

// A priority override can cross priority bands: a p2 item overridden to p1
// ties with a native p1 item and falls through to score/ID ordering.
func TestRecommend_PriorityOverrideTiesNativePriority(t *testing.T) {
	native := &model.Item{ID: "T-LO", Priority: pInt(1), Created: refNow}
	lifted := &model.Item{ID: "T-HI", Priority: pInt(2), Created: refNow}
	// T-HI overridden to p1 — ties with T-LO on effective priority; ID wins.
	overrides := map[string]int{"T-HI": 1}
	recs := Recommend([]*model.Item{lifted, native}, nil, nil, nil, overrides, refNow)
	got := ids(recs)
	// Both at effective p1; "T-HI" < "T-LO" alphabetically → T-HI wins tiebreak.
	if got[0] != "T-HI" {
		t.Fatalf("ID tiebreak within effective-priority tie: got %v, want T-HI first", got)
	}
}

// A nil priorityOverrides map has no effect on scoring.
func TestRecommend_NilOverridesNoEffect(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	r1 := Recommend([]*model.Item{a}, nil, nil, nil, nil, refNow)
	r2 := Recommend([]*model.Item{a}, nil, nil, nil, map[string]int{}, refNow)
	if r1[0].Score != r2[0].Score {
		t.Fatalf("nil vs empty overrides must produce same score: %v vs %v", r1[0].Score, r2[0].Score)
	}
}

// A pinned item gets a score boost but stays in its priority band.
func TestRecommend_PinBoostsWithinBand(t *testing.T) {
	// p1 item (native) must beat pinned p2 even with pin score boost.
	p1 := &model.Item{ID: "I-001", Priority: pInt(1), Created: refNow}
	p2 := &model.Item{ID: "T-001", Priority: pInt(2), Created: refNow}
	pins := map[string]bool{"T-001": true}
	recs := Recommend([]*model.Item{p2, p1}, nil, nil, nil, nil, refNow, pins)
	if got := ids(recs); got[0] != "I-001" {
		t.Fatalf("p1 must beat pinned p2: got %v", got)
	}
	// T-001 score must be higher than p2 item without pin — pin boost is real.
	unpinned := &model.Item{ID: "T-002", Priority: pInt(2), Created: refNow}
	recs2 := Recommend([]*model.Item{p2, unpinned}, nil, nil, nil, nil, refNow, pins)
	if got := ids(recs2); got[0] != "T-001" {
		t.Fatalf("pinned p2 must beat unpinned p2: got %v", got)
	}
	// Pin factor must appear in the rationale.
	for _, r := range recs2 {
		if r.Item.ID == "T-001" && !strings.Contains(r.Rationale(), "queue-pin") {
			t.Errorf("pinned item rationale must contain 'queue-pin': %q", r.Rationale())
		}
	}
}

// Priority override beats high unblock leverage: effective priority is the
// primary sort key, so a lower-override item wins regardless of score delta.
func TestRecommend_PriorityOverrideBeatsHighLeverage(t *testing.T) {
	lifted := &model.Item{ID: "T-LFT", Priority: pInt(2), Created: refNow}
	heavy := &model.Item{ID: "T-HVY", Priority: pInt(2), Created: refNow}
	// T-LFT overridden to p1; T-HVY unblocks 3 items (score=30) but stays p2.
	overrides := map[string]int{"T-LFT": 1}
	recs := Recommend([]*model.Item{heavy, lifted},
		map[string]int{"T-HVY": 3}, nil, nil, overrides, refNow)
	if got := ids(recs); got[0] != "T-LFT" {
		t.Fatalf("priority override must win over high leverage: got %v, want T-LFT first", got)
	}
}

func TestNamedUnblocked(t *testing.T) {
	if got := NamedUnblocked("unblocks 2", []string{"A", "B"}); got != "unblocks 2 (A,B)" {
		t.Errorf("got %q", got)
	}
	if got := NamedUnblocked("unblocks 5", []string{"A", "B", "C", "D", "E"}); got != "unblocks 5 (A,B,C,+2)" {
		t.Errorf("truncation wrong: got %q", got)
	}
	if got := NamedUnblocked("unblocks 0", nil); got != "unblocks 0" {
		t.Errorf("no-ids passthrough wrong: got %q", got)
	}
}
