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

// Queue-pin tests (T-461).

// A pinned item gets the "queue-pin" factor and floats to the top within
// its priority band.
func TestRecommend_PinBoostsWithinPriorityBand(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	b := &model.Item{ID: "T-B", Priority: pInt(2), Created: refNow}
	// T-B is pinned; should beat T-A within p2.
	pinned := map[string]bool{"T-B": true}
	recs := Recommend([]*model.Item{a, b}, nil, nil, nil, pinned, refNow)
	if got := ids(recs); got[0] != "T-B" {
		t.Fatalf("pin should lead in-band: got %v, want T-B first", got)
	}
	// Rationale must include "queue pin".
	if !strings.Contains(recs[0].Rationale(), "queue pin") {
		t.Errorf("pinned item rationale must include 'queue pin', got: %q", recs[0].Rationale())
	}
}

// A pin cannot override the priority primary key: a p1 item always beats a
// pinned p2 item.
func TestRecommend_PinCannotCrossPriorityBand(t *testing.T) {
	lo := &model.Item{ID: "T-LO", Priority: pInt(1), Created: refNow}
	hi := &model.Item{ID: "T-HI", Priority: pInt(2), Created: refNow}
	// T-HI is pinned but has lower priority — T-LO must still win.
	pinned := map[string]bool{"T-HI": true}
	recs := Recommend([]*model.Item{hi, lo}, nil, nil, nil, pinned, refNow)
	if got := ids(recs); got[0] != "T-LO" {
		t.Fatalf("priority must dominate over pin: got %v, want T-LO first", got)
	}
	// T-LO has no pin factor.
	if strings.Contains(recs[0].Rationale(), "queue pin") {
		t.Errorf("unpinned winner must not show queue-pin: %q", recs[0].Rationale())
	}
}

// A nil pinned map contributes no boost.
func TestRecommend_NilPinMapNoBoost(t *testing.T) {
	a := &model.Item{ID: "T-A", Priority: pInt(2), Created: refNow}
	r1 := Recommend([]*model.Item{a}, nil, nil, nil, nil, refNow)
	r2 := Recommend([]*model.Item{a}, nil, nil, nil, map[string]bool{}, refNow)
	if r1[0].Score != r2[0].Score {
		t.Fatalf("nil vs empty pinned map must produce same score: %v vs %v", r1[0].Score, r2[0].Score)
	}
	if strings.Contains(r1[0].Rationale(), "queue pin") {
		t.Errorf("unpin rationale must not contain 'queue pin': %q", r1[0].Rationale())
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
