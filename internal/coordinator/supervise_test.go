package coordinator

import (
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
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

func TestDetectStuck(t *testing.T) {
	spawn := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	base := 30 * time.Minute
	if _, s := DetectStuck(spawn, spawn.Add(95*time.Minute), base, 3); !s {
		t.Error("95m ≥ 3×30m must be stuck")
	}
	if _, s := DetectStuck(spawn, spawn.Add(60*time.Minute), base, 3); s {
		t.Error("60m < 3×30m must not be stuck")
	}
	if _, s := DetectStuck(time.Time{}, spawn, base, 3); s {
		t.Error("zero spawn time must not be stuck (no basis)")
	}
	if _, s := DetectStuck(spawn, spawn.Add(time.Hour), 0, 3); s {
		t.Error("zero baseline must not be stuck")
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
