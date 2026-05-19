package coordinator

import (
	"strings"
	"time"
)

// Escalation is one contract-§7 engagement of the operator. It carries
// everything the operator needs to act WITHOUT re-deriving it (§2:
// observability replaces approval — the record must be self-contained).
type Escalation struct {
	Predicate Predicate // B1/C2/D2/D1/E2
	Item      string    // the affected agent-state item
	Reason    string    // verbatim detector reason (goes into the record)
	FailSig   string    // failure signature, when applicable (dedupe input)
	At        time.Time
}

// Additional predicates the loop raises outside the stall detectors so the
// escalation path is complete (live-verify induces a tripwire to exercise
// the active-ping). B1/C2/D2 are defined in supervise.go.
const (
	PredicateD1 Predicate = "D1" // budget: per_objective cumulative cap exceeded
	PredicateE2 Predicate = "E2" // tripwire: an always-escalate op (coordinator.yaml)
)

// Category maps a predicate to its contract-§7 category letter. The
// escalation_channel (§11-K6) active-pings only category-E and the budget
// cap (D1); everything else surfaces to the alerts band (here: the durable
// changelog + conversation-channel record) without stop-the-world.
func Category(p Predicate) string {
	switch p {
	case PredicateB1:
		return "B"
	case PredicateC2:
		return "C"
	case PredicateD1, PredicateD2:
		return "D"
	case PredicateE2:
		return "E"
	}
	return "?"
}

// PingClass returns the escalation_channel class string for a predicate,
// matching coordinator.yaml escalation_channel.active_ping values
// ("category_E", "budget_cap"). A predicate with no active-ping class
// returns "" (alerts-band only).
func PingClass(p Predicate) string {
	switch p {
	case PredicateE2:
		return "category_E"
	case PredicateD1:
		return "budget_cap"
	}
	return ""
}

// Deduper collapses same-root-cause escalations within the boundary's
// dedupe window (§7 design rule: one escalation per root cause, not per
// affected worker). In-memory per loop process; the FILED issue is the
// durable cross-process record, so a loop restart re-firing once is
// acceptable and intended (better a duplicate than a swallowed incident).
type Deduper struct {
	last map[string]time.Time
}

// NewDeduper returns an empty Deduper.
func NewDeduper() *Deduper { return &Deduper{last: map[string]time.Time{}} }

// dedupeKey is predicate + item + failure-signature: the root cause, not
// the worker. Two workers failing the same gate the same way collapse.
func dedupeKey(e Escalation) string {
	return string(e.Predicate) + ":" + e.Item + ":" + e.FailSig
}

// ShouldFire reports whether this escalation may fire now given window,
// and records the decision when it returns true. A non-positive window
// disables dedupe (always fire) — a safe failure mode (never SWALLOW an
// incident because the window was misconfigured).
func (d *Deduper) ShouldFire(e Escalation, window time.Duration, now time.Time) bool {
	if window <= 0 {
		return true
	}
	k := dedupeKey(e)
	if prev, ok := d.last[k]; ok && now.Sub(prev) < window {
		return false
	}
	d.last[k] = now
	return true
}

// Escalator performs the side effects of an escalation. The interface is
// injected so the pure dedupe/orchestration here is unit-testable with a
// fake, and the real implementation (internal/command) can reuse the
// correct issue-creation + changelog + notification paths without an
// import cycle (command → coordinator for the cobra command).
type Escalator interface {
	// FileBlocker files a tracked issue that BLOCKS the item and returns
	// its id (the durable, dependency-linked §7/§13 record).
	FileBlocker(e Escalation) (issueID string, err error)
	// Log writes the durable, observable changelog record on the item
	// (the alerts-band substrate — visible via st watch / st transcript).
	Log(e Escalation, issueID string) error
	// Mail puts the escalation on the conversation channel (§8.2 source).
	Mail(e Escalation, issueID string) error
	// Notify fires the active-ping (macOS osascript) — called ONLY for
	// predicates whose PingClass is in the boundary's active_ping set.
	Notify(e Escalation) error
}

// FireResult records what Fire did, so the loop can report it (and tests
// can assert it) without scraping side effects.
type FireResult struct {
	Fired   bool   // false ⇒ collapsed by dedupe
	IssueID string // filed blocker id ("" if FileBlocker failed)
	Pinged  bool   // active-ping fired
	Errs    []error
}

// Fire orchestrates one escalation: dedupe → file blocker → durable log →
// conversation-channel mail → (conditional) active-ping. It is
// best-effort and NEVER aborts the loop: a failed side effect is collected
// into Errs and surfaced, because a half-failed escalation must still be
// loud, not silent (operator silent-failure principle). The loop's caller
// STOPS the item after Fire regardless — Fire records, it does not decide
// to continue.
func Fire(e Escalation, b *Boundary, dd *Deduper, ex Escalator, now time.Time) FireResult {
	var res FireResult
	window := time.Duration(b.DedupeWindowMin) * time.Minute
	if !dd.ShouldFire(e, window, now) {
		return res // Fired=false: collapsed; the prior fire is the record
	}
	res.Fired = true

	id, err := ex.FileBlocker(e)
	if err != nil {
		res.Errs = append(res.Errs, err)
	}
	res.IssueID = id

	if err := ex.Log(e, id); err != nil {
		res.Errs = append(res.Errs, err)
	}
	if err := ex.Mail(e, id); err != nil {
		res.Errs = append(res.Errs, err)
	}

	if class := PingClass(e.Predicate); class != "" && b.ActivePings(class) {
		if err := ex.Notify(e); err != nil {
			res.Errs = append(res.Errs, err)
		} else {
			res.Pinged = true
		}
	}
	return res
}

// EscalationTitle is the deterministic title for a filed blocker, so a
// loop restart that re-files (different process, dedupe reset) is at least
// recognisable as the same incident class.
func EscalationTitle(e Escalation) string {
	return strings.TrimSpace("Coordinator escalation " + string(e.Predicate) +
		" on " + e.Item + " — " + firstSentence(e.Reason))
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	if len(s) > 80 {
		return strings.TrimSpace(s[:80])
	}
	return s
}
