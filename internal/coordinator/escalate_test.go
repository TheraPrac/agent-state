package coordinator

import (
	"errors"
	"testing"
	"time"
)

func TestCategoryAndPingClass(t *testing.T) {
	cases := []struct {
		p    Predicate
		cat  string
		ping string
	}{
		{PredicateB1, "B", ""},
		{PredicateC2, "C", ""},
		{PredicateD2, "D", ""},
		{PredicateD1, "D", "budget_cap"},
		{PredicateE2, "E", "category_E"},
	}
	for _, c := range cases {
		if Category(c.p) != c.cat {
			t.Errorf("Category(%s) = %s, want %s", c.p, Category(c.p), c.cat)
		}
		if PingClass(c.p) != c.ping {
			t.Errorf("PingClass(%s) = %q, want %q", c.p, PingClass(c.p), c.ping)
		}
	}
}

func TestDeduper(t *testing.T) {
	dd := NewDeduper()
	e := Escalation{Predicate: PredicateB1, Item: "T-1", FailSig: "sig"}
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	win := 30 * time.Minute

	if !dd.ShouldFire(e, win, t0) {
		t.Fatal("first occurrence must fire")
	}
	if dd.ShouldFire(e, win, t0.Add(10*time.Minute)) {
		t.Error("same root cause within window must collapse")
	}
	if !dd.ShouldFire(e, win, t0.Add(31*time.Minute)) {
		t.Error("after the window the same root cause fires again")
	}
	// Different root cause is independent even within the window.
	other := Escalation{Predicate: PredicateB1, Item: "T-1", FailSig: "DIFFERENT"}
	if !dd.ShouldFire(other, win, t0.Add(11*time.Minute)) {
		t.Error("a different failure signature is a different root cause — must fire")
	}
	// window<=0 disables dedupe (never swallow on misconfig).
	if !dd.ShouldFire(e, 0, t0) || !dd.ShouldFire(e, 0, t0) {
		t.Error("non-positive window must always fire (fail-safe)")
	}
}

// fakeEscalator records calls so Fire's orchestration is asserted without
// real side effects.
type fakeEscalator struct {
	filed, logged, mailed, notified int
	failFile, failNotify            bool
}

func (f *fakeEscalator) FileBlocker(Escalation) (string, error) {
	f.filed++
	if f.failFile {
		return "", errors.New("file boom")
	}
	return "I-999", nil
}
func (f *fakeEscalator) Log(Escalation, string) error  { f.logged++; return nil }
func (f *fakeEscalator) Mail(Escalation, string) error { f.mailed++; return nil }
func (f *fakeEscalator) Notify(Escalation) error {
	f.notified++
	if f.failNotify {
		return errors.New("notify boom")
	}
	return nil
}

func TestFire(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

	// B1 (no ping class) with active_ping set: files+logs+mails, NO ping.
	b := &Boundary{DedupeWindowMin: 30, ActivePingClasses: []string{"category_E", "budget_cap"}}
	dd := NewDeduper()
	fe := &fakeEscalator{}
	res := Fire(Escalation{Predicate: PredicateB1, Item: "T-1", FailSig: "s"}, b, dd, fe, now)
	if !res.Fired || res.IssueID != "I-999" {
		t.Fatalf("B1 must fire and file a blocker, got %+v", res)
	}
	if fe.filed != 1 || fe.logged != 1 || fe.mailed != 1 {
		t.Errorf("file/log/mail must each fire once: %+v", fe)
	}
	if fe.notified != 0 || res.Pinged {
		t.Error("B1 has no active-ping class — must not notify")
	}

	// Second identical within window → collapsed (no new side effects).
	res2 := Fire(Escalation{Predicate: PredicateB1, Item: "T-1", FailSig: "s"}, b, dd, fe, now.Add(time.Minute))
	if res2.Fired || fe.filed != 1 {
		t.Error("dedupe must collapse the second identical escalation")
	}

	// E2 (category_E) WITH the class in active_ping → notify fires.
	feE := &fakeEscalator{}
	res = Fire(Escalation{Predicate: PredicateE2, Item: "T-2", FailSig: "trip"}, b, NewDeduper(), feE, now)
	if !res.Pinged || feE.notified != 1 {
		t.Errorf("E2 with category_E in active_ping must active-ping, got %+v / %+v", res, feE)
	}

	// E2 but boundary does NOT list category_E → no ping (boundary governs).
	bNoPing := &Boundary{DedupeWindowMin: 30}
	feNo := &fakeEscalator{}
	res = Fire(Escalation{Predicate: PredicateE2, Item: "T-3"}, bNoPing, NewDeduper(), feNo, now)
	if res.Pinged || feNo.notified != 0 {
		t.Error("active-ping must respect the boundary's active_ping set")
	}

	// A failed side effect is collected, never aborts (loud, not silent).
	feErr := &fakeEscalator{failFile: true, failNotify: true}
	res = Fire(Escalation{Predicate: PredicateE2, Item: "T-4"}, b, NewDeduper(), feErr, now)
	if !res.Fired || len(res.Errs) < 2 {
		t.Errorf("failed side effects must be surfaced in Errs, not swallowed: %+v", res)
	}
	if feErr.logged != 1 || feErr.mailed != 1 {
		t.Error("a FileBlocker failure must NOT short-circuit the durable log/mail")
	}
}

func TestEscalationTitleDeterministic(t *testing.T) {
	e := Escalation{Predicate: PredicateC2, Item: "T-1", Reason: "wedged: PID alive, no progress. extra."}
	a := EscalationTitle(e)
	if a != EscalationTitle(e) {
		t.Error("title must be deterministic for the same escalation")
	}
	if a == "" || len(a) > 200 {
		t.Errorf("title must be non-empty and bounded: %q", a)
	}
}
