package coordinator

import (
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/model"
)

func TestFailSignature(t *testing.T) {
	a := FailSignature("FAIL TestFoo\nstack...\nmore")
	b := FailSignature("fail   testfoo\nDIFFERENT stack")
	if a == "" {
		t.Fatal("non-empty body must yield a signature")
	}
	if a != b {
		t.Errorf("same first line (case/space-insensitive) must hash equal: %q vs %q", a, b)
	}
	if FailSignature("totally other error") == a {
		t.Error("different first line must differ")
	}
	if FailSignature("\n  \n") != "" {
		t.Error("whitespace-only body must yield empty signature")
	}
}

func TestMadeItemProgress(t *testing.T) {
	base := ProgressSnapshot{ChangelogLen: 4, Stage: "active"}
	if !MadeItemProgress(base, ProgressSnapshot{ChangelogLen: 5, Stage: "active"}) {
		t.Error("more changelog entries = progress")
	}
	if !MadeItemProgress(base, ProgressSnapshot{ChangelogLen: 4, Stage: "pr_open"}) {
		t.Error("changed stage = progress")
	}
	if MadeItemProgress(base, ProgressSnapshot{ChangelogLen: 4, Stage: "active"}) {
		t.Error("no change = no progress (a looping worker emits rows, not progress)")
	}
}

func TestDetectWedged(t *testing.T) {
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	wedge := 10 * time.Minute
	// Alive, static row/mtime/changelog across ≥ wedge → wedged.
	snaps := []ProgressSnapshot{
		{PIDAlive: true, RowCount: 100, ChangelogLen: 3, SampledAt: t0},
		{PIDAlive: true, RowCount: 100, ChangelogLen: 3, SampledAt: t0.Add(12 * time.Minute)},
	}
	if _, w := DetectWedged(snaps, wedge); !w {
		t.Error("alive + static for ≥ wedge must be wedged")
	}
	// Row count advanced → not wedged.
	snaps[1].RowCount = 140
	if _, w := DetectWedged(snaps, wedge); w {
		t.Error("advancing row count is progress, not wedged")
	}
	// Dead PID is terminal, not wedged.
	dead := []ProgressSnapshot{
		{PIDAlive: false, SampledAt: t0},
		{PIDAlive: false, SampledAt: t0.Add(12 * time.Minute)},
	}
	if _, w := DetectWedged(dead, wedge); w {
		t.Error("dead PID must not be classified wedged (terminal path)")
	}
	// Not enough elapsed wall-clock yet.
	young := []ProgressSnapshot{
		{PIDAlive: true, RowCount: 1, SampledAt: t0},
		{PIDAlive: true, RowCount: 1, SampledAt: t0.Add(2 * time.Minute)},
	}
	if _, w := DetectWedged(young, wedge); w {
		t.Error("< wedge elapsed must not be wedged")
	}
	if _, w := DetectWedged(snaps[:1], wedge); w {
		t.Error("<2 snapshots can't be wedged")
	}
}

// T-365 — cost-based D2 detector. (T-381 removed TestDetectStuck and
// the wall-clock function alongside it; see git history if needed.)
func TestDetectStuckByCost(t *testing.T) {
	if reason, s := DetectStuckByCost(30.0, 10.0, 3.0); !s {
		t.Errorf("$30 ≥ 3×$10 must be stuck, got (%q, %v)", reason, s)
	} else if !strings.Contains(reason, "cost $") || !strings.Contains(reason, "§7-D2") {
		t.Errorf("reason must name cost + §7-D2: %q", reason)
	}
	if _, s := DetectStuckByCost(15.0, 10.0, 3.0); s {
		t.Error("$15 < 3×$10 must not be stuck")
	}
	// $0 rollup ⇒ not yet populated, NOT stuck (the I-369 wiring may not
	// have fired its first SubagentStop yet).
	if _, s := DetectStuckByCost(0, 10.0, 3.0); s {
		t.Error("$0 (rollup not populated) must NOT trigger D2")
	}
	if _, s := DetectStuckByCost(30, 0, 3.0); s {
		t.Error("zero baseline must not be stuck")
	}
	if _, s := DetectStuckByCost(30, 10.0, 0); s {
		t.Error("zero multiplier must not be stuck")
	}
}

// T-365 — heuristic cost baselines stay under K1=$40 with K2=3 so D2
// fires comfortably before the hard per_item budget cap.
func TestSizeClassCostBaseline_BelowK1Cap(t *testing.T) {
	const k1Cap = 40.0
	const k2Mult = 3.0 // the documented ceiling: function doc says baselines hold K1 headroom AT K2 ≤ 3
	pInt := func(n int) *int { return &n }
	cases := []*model.Item{
		{Type: "issue", Priority: pInt(0)},
		{Type: "issue", Priority: pInt(2)},
		{Type: "task", Priority: pInt(1)},
		{Type: "task", Priority: pInt(3)},
		{Type: "task"}, // priority nil → default
	}
	for _, it := range cases {
		baseline := SizeClassCostBaseline(it)
		if baseline*k2Mult >= k1Cap {
			t.Errorf("baseline $%g × K2=%g = $%g ≥ K1 $%g — D2 won't fire before budget cap (type=%s pri=%v)",
				baseline, k2Mult, baseline*k2Mult, k1Cap, it.Type, it.Priority)
		}
	}
}

// TestSizeClassCostBaseline_K2CeilingBreach proves the K2 ceiling
// documented in SizeClassCostBaseline's doc: raising K2 above 3
// breaks the K1-headroom guarantee for at least one size class. This
// is the asymmetric "if the operator raises stuck_multiplier > 3 the
// heuristics no longer protect K1 budget cap" invariant — surfaced as
// a test so anyone tempted to bump K2 finds the proof here.
func TestSizeClassCostBaseline_K2CeilingBreach(t *testing.T) {
	const k1Cap = 40.0
	const k2Above = 4.0 // any K2 > 3 must violate the invariant for at least one size class
	pInt := func(n int) *int { return &n }
	allCases := []*model.Item{
		{Type: "issue", Priority: pInt(0)},
		{Type: "issue", Priority: pInt(2)},
		{Type: "task", Priority: pInt(1)},
		{Type: "task", Priority: pInt(3)},
		{Type: "task"},
	}
	var breached bool
	for _, it := range allCases {
		if SizeClassCostBaseline(it)*k2Above >= k1Cap {
			breached = true
			break
		}
	}
	if !breached {
		t.Fatalf("expected at least one size class to breach K1=$%g at K2=%g — "+
			"if this test stops failing, either the baselines were lowered "+
			"(re-check the K2 ceiling) or K1 was raised (update test)", k1Cap, k2Above)
	}
}

// TestParseCostUSD covers the I-369 cost-rollup parsing branch added
// by T-365. Tested as a pure function so we don't depend on a config /
// changelog / registry. Without this test, a future storage-format
// change (string → float64 → int) could silently blind D2 because the
// existing TestDetectStuckByCost builds ProgressSnapshot directly and
// skips the parsing branch.
func TestParseCostUSD(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want float64
	}{
		{"string (current session_log.go format)", "12.500000", 12.5},
		{"string with trailing zeros", "3.068790", 3.06879},
		{"float64 (defensive fallback)", float64(7.25), 7.25},
		{"int (defensive fallback)", 3, 3.0},
		{"nil (missing key) ⇒ zero", nil, 0.0},
		{"unsupported type ⇒ zero", []string{"weird"}, 0.0},
		{"unparseable string ⇒ zero", "not-a-number", 0.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCostUSD(c.raw); got != c.want {
				t.Errorf("parseCostUSD(%#v) = %g, want %g", c.raw, got, c.want)
			}
		})
	}
}

func TestClassifyRespawn(t *testing.T) {
	b := &Boundary{RespawnLimit: 3}

	// Made progress → none, respawn permitted.
	if v := ClassifyRespawn(&WorkerState{}, "sig", true, b); v.Predicate != PredicateNone {
		t.Errorf("progress must not escalate, got %s", v.Predicate)
	}
	// No progress, budget remains → none (informed retry allowed).
	if v := ClassifyRespawn(&WorkerState{RespawnCount: 1}, "sig", false, b); v.Predicate != PredicateNone {
		t.Errorf("under limit must not escalate, got %s", v.Predicate)
	}
	// At limit, SAME signature, no progress → B1 (looping).
	st := &WorkerState{RespawnCount: 3, LastFailSig: "sigX"}
	if v := ClassifyRespawn(st, "sigX", false, b); v.Predicate != PredicateB1 {
		t.Errorf("same-sig no-progress at limit must be B1, got %s (%s)", v.Predicate, v.Reason)
	}
	// At limit, DIFFERENT signature, no progress → C2 (unrecoverable).
	if v := ClassifyRespawn(st, "sigY", false, b); v.Predicate != PredicateC2 {
		t.Errorf("diff-sig at limit must be C2, got %s", v.Predicate)
	}

	// HARD-CAP regression (code-review finding 2): respawn_limit bounds
	// TOTAL respawns even when the attempt MADE PROGRESS. A
	// progressing-but-never-completing worker at the limit must escalate
	// C2, NOT respawn unboundedly (the old madeProgress short-circuit bug).
	if v := ClassifyRespawn(&WorkerState{RespawnCount: 3, LastFailSig: "x"}, "x", true, b); v.Predicate != PredicateC2 {
		t.Errorf("progress must NOT bypass the respawn_limit hard cap — want C2, got %s (%s)", v.Predicate, v.Reason)
	}
	// Under the limit, progress → respawn permitted (none).
	if v := ClassifyRespawn(&WorkerState{RespawnCount: 1}, "x", true, b); v.Predicate != PredicateNone {
		t.Errorf("progress under the limit must permit respawn, got %s", v.Predicate)
	}
}

func TestFindWorkerRegTiebreakHelpers(t *testing.T) {
	older := "2026-05-19T12:00:00-06:00"
	newer := "2026-05-19T12:05:00-06:00"
	if !parseStarted(newer).After(parseStarted(older)) {
		t.Error("parseStarted must order RFC3339 timestamps")
	}
	if !parseStarted("garbage").IsZero() {
		t.Error("unparseable Started must be zero time (loses the tiebreak, never panics)")
	}
}

func TestSizeClassBaseline(t *testing.T) {
	p1 := 1
	p3 := 3
	iss := SizeClassBaseline(&model.Item{Type: "issue", Priority: &p1})
	issLo := SizeClassBaseline(&model.Item{Type: "issue", Priority: &p3})
	task := SizeClassBaseline(&model.Item{Type: "task", Priority: &p3})
	if iss <= 0 || issLo <= iss || task <= issLo {
		t.Errorf("baselines must be positive and ordered issue-hi<issue-lo<task: %v %v %v", iss, issLo, task)
	}
	// No priority → must still return a positive default (p2 path).
	if SizeClassBaseline(&model.Item{Type: "task"}) <= 0 {
		t.Error("missing priority must still yield a positive baseline")
	}
}
