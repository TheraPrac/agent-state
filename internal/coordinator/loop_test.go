package coordinator

import (
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

func TestEligibleForDispatch(t *testing.T) {
	ok := &model.Item{ID: "T-1", Type: "task", Status: "queued"}
	if e, _ := EligibleForDispatch(ok, true, false, false); !e {
		t.Error("approved+unblocked+non-terminal+unclaimed must be eligible")
	}
	if e, why := EligibleForDispatch(nil, true, false, false); e || why == "" {
		t.Error("nil item must be ineligible with a reason")
	}
	if e, _ := EligibleForDispatch(ok, false, false, false); e {
		t.Error("unapproved (I-490) must be ineligible")
	}
	if e, _ := EligibleForDispatch(ok, true, true, false); e {
		t.Error("blocked must be ineligible")
	}
	if e, _ := EligibleForDispatch(ok, true, false, true); e {
		t.Error("terminal must be ineligible")
	}
	if e, _ := EligibleForDispatch(&model.Item{ID: "T-2", ClaimedBy: "sess"}, true, false, false); e {
		t.Error("claimed must be ineligible (single in-flight)")
	}
	if e, why := EligibleForDispatch(&model.Item{ID: "T-3", AssignedTo: "agent-c"}, true, false, false); e || why == "" {
		t.Error("peer-assigned must be ineligible with a reason (coordination rule)")
	}
}

func TestWedgeThreshold(t *testing.T) {
	if WedgeThreshold(4*time.Minute) != 5*time.Minute {
		t.Error("tiny size class must floor wedge at 5m")
	}
	if WedgeThreshold(40*time.Minute) != 10*time.Minute {
		t.Errorf("sizeClass/4 above the floor, got %v", WedgeThreshold(40*time.Minute))
	}
}

func TestDecide(t *testing.T) {
	b := &Boundary{RespawnLimit: 3, StuckMultiplier: 3}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	mk := func(s ProgressSnapshot) []ProgressSnapshot { return []ProgressSnapshot{s} }

	// Empty snaps → continue.
	if d := Decide(&WorkerState{}, nil, b, false, now); d.Action != ActionContinue {
		t.Error("no snapshots → continue")
	}

	// Terminated + item completed → done.
	st := &WorkerState{SizeClass: 30 * time.Minute}
	d := Decide(st, mk(ProgressSnapshot{PIDAlive: false}), b, true, now)
	if d.Action != ActionTerminalDone {
		t.Errorf("terminated + completed → done, got %v", d.Action)
	}

	// Terminated + not completed + budget remains → respawn.
	st = &WorkerState{RespawnCount: 1, SizeClass: 30 * time.Minute}
	d = Decide(st, mk(ProgressSnapshot{PIDAlive: false, ChangelogLen: 0}), b, false, now)
	if d.Action != ActionRespawn {
		t.Errorf("terminated, no progress, budget remains → respawn, got %v", d.Action)
	}

	// Terminated + not completed + at limit + same sig → escalate B1.
	st = &WorkerState{RespawnCount: 3, LastFailSig: "x", SizeClass: 30 * time.Minute}
	d = Decide(st, mk(ProgressSnapshot{PIDAlive: false, LastErrSig: "x"}), b, false, now)
	if d.Action != ActionEscalate || d.Verdict.Predicate != PredicateB1 {
		t.Errorf("at limit same-sig → escalate B1, got %v %s", d.Action, d.Verdict.Predicate)
	}

	// Live + wedged → escalate C2.
	st = &WorkerState{SizeClass: 40 * time.Minute} // wedge = 10m
	snaps := []ProgressSnapshot{
		{PIDAlive: true, RowCount: 1, SampledAt: now},
		{PIDAlive: true, RowCount: 1, SampledAt: now.Add(12 * time.Minute)},
	}
	d = Decide(st, snaps, b, false, now.Add(12*time.Minute))
	if d.Action != ActionEscalate || d.Verdict.Predicate != PredicateC2 {
		t.Errorf("live + static ≥ wedge → escalate C2, got %v %s", d.Action, d.Verdict.Predicate)
	}

	// Live + stuck (cost ≥ mult×cost-baseline) → escalate D2.
	// T-365: D2 is now cost-based. The wall-clock SizeClass still drives
	// the C2 wedge threshold, but D2 reads AICostUSD against CostBaseline.
	// SpawnedAt is NOT read by cost-based D2 (was used by legacy DetectStuck).
	// T-380: D2 compares the PER-ATTEMPT delta, so attemptStartCostUSD must
	// be set explicitly via BeginAttempt — here we record $0 to model a
	// FIRST-attempt worker with no prior cost (delta == lifetime cost).
	// TestDecide_D2UsesPerAttemptDelta below covers the prior-cost case.
	st = &WorkerState{SizeClass: 60 * time.Minute, CostBaseline: 10.0}
	st.BeginAttempt(now, 0, 0)
	live := []ProgressSnapshot{
		{PIDAlive: true, RowCount: 1, AICostUSD: 1.0, SampledAt: now},
		{PIDAlive: true, RowCount: 9, AICostUSD: 30.0, SampledAt: now.Add(5 * time.Minute)}, // $30 ≥ 3×$10
	}
	d = Decide(st, live, b, false, now.Add(5*time.Minute))
	if d.Action != ActionEscalate || d.Verdict.Predicate != PredicateD2 {
		t.Errorf("live + cost ≥ stuck_x×cost-baseline → escalate D2, got %v %s", d.Action, d.Verdict.Predicate)
	}

	// Live + fine → continue. CostBaseline MUST be non-zero or
	// DetectStuckByCost short-circuits on its baselineUSD<=0 guard and
	// the test would pass without actually evaluating D2's threshold.
	st = &WorkerState{SizeClass: 60 * time.Minute, CostBaseline: 10.0}
	st.BeginAttempt(now, 0, 0) // explicit per-attempt baseline (T-380)
	d = Decide(st, []ProgressSnapshot{
		{PIDAlive: true, RowCount: 1, AICostUSD: 1.0, SampledAt: now},
		{PIDAlive: true, RowCount: 9, AICostUSD: 5.0, SampledAt: now.Add(2 * time.Minute)}, // $5 < 3×$10 — D2 evaluates & returns false
	}, b, false, now.Add(2*time.Minute))
	if d.Action != ActionContinue {
		t.Errorf("live + progressing + cost < threshold → continue, got %v", d.Action)
	}
}

func TestWorkerStateAttemptLifecycle(t *testing.T) {
	st := &WorkerState{}
	t0 := time.Now()
	st.BeginAttempt(t0, 7, 12.5)
	if st.SpawnedAt != t0 || st.attemptStartChangelog != 7 || st.attemptStartCostUSD != 12.5 {
		t.Fatal("BeginAttempt must record spawn time + per-attempt changelog AND cost baseline")
	}
	st.RecordRespawn("sigA")
	if st.RespawnCount != 1 || st.LastFailSig != "sigA" {
		t.Fatal("RecordRespawn must bump count + carry the failure signature forward")
	}
	// A respawn that progressed past the baseline must read as progress.
	if !MadeItemProgress(ProgressSnapshot{ChangelogLen: st.attemptStartChangelog},
		ProgressSnapshot{ChangelogLen: st.attemptStartChangelog + 1}) {
		t.Error("changelog past the per-attempt baseline must be progress")
	}
}

// T-380: D2 must compare THIS WORKER's spend against the baseline, not
// the item's lifetime rollup. An item carrying $20 of prior-session
// spend that grows by only $5 this attempt is NOT stuck — delta is $5,
// under K2×$10. Same item growing to $60 (delta $40) IS stuck.
func TestDecide_D2UsesPerAttemptDelta(t *testing.T) {
	b := &Boundary{RespawnLimit: 3, StuckMultiplier: 3}
	now := time.Now()
	mkSt := func(priorCost float64) *WorkerState {
		st := &WorkerState{SizeClass: 60 * time.Minute, CostBaseline: 10.0}
		st.BeginAttempt(now, 0, priorCost) // record per-attempt cost baseline
		return st
	}
	// Item already at $20 from prior sessions; this attempt adds only $5.
	st := mkSt(20.0)
	d := Decide(st, []ProgressSnapshot{
		{PIDAlive: true, AICostUSD: 25.0, SampledAt: now.Add(2 * time.Minute)},
	}, b, false, now.Add(2*time.Minute))
	if d.Action == ActionEscalate {
		t.Errorf("delta=$5 < K2×$10=$30 must NOT escalate D2 (item lifetime $25 is irrelevant), got %v %s",
			d.Action, d.Verdict.Predicate)
	}

	// Same item; this attempt burned to $60 — delta $40, over the threshold.
	st = mkSt(20.0)
	d = Decide(st, []ProgressSnapshot{
		{PIDAlive: true, AICostUSD: 60.0, SampledAt: now.Add(2 * time.Minute)},
	}, b, false, now.Add(2*time.Minute))
	if d.Action != ActionEscalate || d.Verdict.Predicate != PredicateD2 {
		t.Errorf("delta=$40 ≥ K2×$10=$30 must escalate D2, got %v %s",
			d.Action, d.Verdict.Predicate)
	}

	// Defensive: if attemptStartCostUSD somehow exceeds current (substrate
	// edit, clock skew), delta clamps to 0 — D2 stays silent rather than
	// firing on a phantom negative.
	st = mkSt(100.0)
	d = Decide(st, []ProgressSnapshot{
		{PIDAlive: true, AICostUSD: 50.0, SampledAt: now.Add(2 * time.Minute)},
	}, b, false, now.Add(2*time.Minute))
	if d.Action == ActionEscalate {
		t.Errorf("negative delta must clamp to 0 (no false D2), got %v %s",
			d.Action, d.Verdict.Predicate)
	}
}
