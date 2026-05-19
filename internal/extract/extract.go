// Package extract is the I-679 Phase C deterministic decision extractor: the
// lossy backstop writer for the two-writer decision model. Phase B's
// PostToolUse hook captures structured forks verbatim (AskUserQuestion /
// ExitPlanMode / plan-approve / push); but a fork settled purely in prose
// ("operator said don't do X, do Y", "I'll go with A over B because …")
// never passes through a structured channel and is lost when the window is
// compacted away. This package scans the about-to-be-summarized transcript
// and recovers those prose forks.
//
// Design constraints (from the approved I-679 plan, decision #4):
//   - Extraction is EXPLICITLY LOSSY. It is deliberately high-precision /
//     low-recall: a missed fork degrades to "not journaled" (the pre-I-679
//     status quo), but a wrong-but-confident fork would actively mislead a
//     resuming session — far worse. So thresholds favour silence over noise.
//   - Every Candidate carries a Confidence. Below-threshold candidates are
//     still emitted (the backstop must not drop them) but flagged so the
//     resume handshake can ask for a single boundary confirmation rather
//     than presenting them as settled truth.
//   - Pure and deterministic: same rows in ⇒ same candidates out, no clock,
//     no I/O, no randomness. This is what makes it table-testable and what
//     lets PreCompact re-runs reconcile idempotently upstream.
//
// It consumes internal/transcript read-only (agent-b owns that package for
// T-353); it never writes — the caller (command.ExtractDecisions) reconciles
// against existing structured entries and appends.
package extract

import (
	"regexp"
	"strings"

	"github.com/jfinlinson/agent-state/internal/transcript"
)

// ConfirmThreshold is the confidence at/above which an extracted decision is
// presented as a (provisional) fact; below it the resume handshake must ask
// for a single boundary confirmation before the agent acts on it. 0.5 is the
// midpoint: explicit-marker and operator-override signals land above it;
// bare inference lands below.
const ConfirmThreshold = 0.5

// Candidate is one recovered prose fork. Text is the decision itself;
// Rationale and RejectedAlts are the "why" / "discarded options" when the
// prose stated them (the half of a decision that is the actual non-
// re-derivable value — a bare verdict is useless). Confidence is the
// extractor's self-assessed signal strength, 0 < c <= 1.
type Candidate struct {
	Text         string
	Rationale    string
	RejectedAlts string
	Confidence   float64
	Role         string // "user" (operator override) | "assistant" (agent-stated)
}

// NeedsConfirm reports whether this candidate is below the boundary-confirm
// threshold and must be surfaced as a question, not a fact, at resume.
func (c Candidate) NeedsConfirm() bool { return c.Confidence < ConfirmThreshold }

var (
	// Strongest signal: an explicit decision/rejection marker the writer
	// deliberately used. Anchored to a statement start (line or sentence).
	reDecisionMarker = regexp.MustCompile(`(?i)\b(decision|decided|conclusion|verdict)\s*[:\-—]\s*(.+)`)
	reChoseOver      = regexp.MustCompile(`(?i)\b(?:chose|choosing|picked|going with|go with|went with)\b(.+?)\b(?:over|instead of|rather than|not)\b(.+)`)
	reRejected       = regexp.MustCompile(`(?i)\b(rejected|ruled out|discarded|ruled-out)\b(?:\s+alternative[s]?)?\s*[:\-—]?\s*(.+)`)
	// Operator-override imperatives in a user turn: a redirection is a
	// settled fork even without a marker word.
	reOperatorOverride = regexp.MustCompile(`(?i)^\s*(?:no[,.]?\s+|actually[,.]?\s+|instead[,.]?\s+|don'?t\s+|do not\s+|stop\s+|let'?s\s+(?:go with|use|do)\b|use\s+\w+\s+instead\b)`)
	// Rationale connectives — the clause after one of these is the "why".
	reBecause = regexp.MustCompile(`(?i)\b(because|since|so that|due to|in order to|rationale[:\-])\b\s*(.+)`)
)

const maxField = 400

func condense(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " \t.-—:")
	if len(s) > maxField {
		s = s[:maxField-1] + "…"
	}
	return s
}

// splitStatements breaks a prose blob into candidate statement units. A
// decision rarely spans paragraphs, and scanning whole multi-KB assistant
// turns as one unit destroys precision, so we segment on line and sentence
// boundaries and test each segment independently.
func splitStatements(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Sentence-ish split; keep it cheap and deterministic.
		for _, seg := range regexp.MustCompile(`(?:\.\s+|;\s+|! )`).Split(line, -1) {
			seg = strings.TrimSpace(seg)
			if len(seg) >= 8 { // a sub-8-char "statement" is noise
				out = append(out, seg)
			}
		}
	}
	return out
}

// extractRationaleAndAlts pulls the "why" and "discarded options" out of a
// statement (and any continuation passed as ctx). Both are best-effort: an
// absent rationale is fine, a wrong one is not, so the patterns are narrow.
func extractRationaleAndAlts(stmt string) (rationale, rejected string) {
	if m := reBecause.FindStringSubmatch(stmt); m != nil {
		rationale = condense(m[2])
	}
	if m := reChoseOver.FindStringSubmatch(stmt); m != nil {
		rejected = condense(m[2])
	} else if m := reRejected.FindStringSubmatch(stmt); m != nil {
		rejected = condense(m[2])
	}
	return rationale, rejected
}

// scoreStatement returns the confidence for a single statement and the
// decision text to record, or ok=false if the statement carries no decision
// signal at all. role lets an operator turn (a redirection) score on its own
// even without a marker word — an operator override IS a settled fork.
func scoreStatement(stmt, role string) (text string, conf float64, ok bool) {
	switch {
	case reDecisionMarker.MatchString(stmt):
		m := reDecisionMarker.FindStringSubmatch(stmt)
		return condense(m[2]), 0.85, true
	case reRejected.MatchString(stmt):
		return condense(stmt), 0.8, true
	case reChoseOver.MatchString(stmt):
		// "chose X over Y" — the choice itself is the decision.
		return condense(stmt), 0.75, true
	case role == "user" && reOperatorOverride.MatchString(stmt):
		// An operator redirection. High-value, but prose-fuzzy: mid score
		// so it lands above ConfirmThreshold (acted on) yet flagged-able.
		return condense(stmt), 0.6, true
	case role == "assistant" && regexp.MustCompile(`(?i)\b(i'?ll|i will|let'?s|we'?ll|plan to|going to)\b`).MatchString(stmt) &&
		reBecause.MatchString(stmt):
		// Agent intent WITH a stated reason: weak-but-real. Below
		// threshold ⇒ kept, but boundary-confirmed not asserted.
		return condense(stmt), 0.4, true
	}
	return "", 0, false
}

// minContainmentLen guards substring-based dedup: only collapse two forks
// when the shorter normalized text is at least this long AND is a contiguous
// substring of the longer. Short fragments ("use X") would false-merge
// unrelated decisions; a >= 14-char contiguous shared core is a restatement
// of the same fork in practice ("I'll go with <core>" then "Decision:
// <core>"), which real transcripts produce constantly.
const minContainmentLen = 14

// Norm normalizes decision text for equivalence comparison (lowercase,
// whitespace-collapsed). Exported so the upstream reconcile step
// (command.ExtractDecisions, skipping forks already captured structured)
// uses the SAME normalization as intra-extract dedup — the match logic must
// not drift between the two.
func Norm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// SameFork reports whether two NORMALIZED decision texts (see Norm) are the
// same fork restated: identical, or the shorter is a long-enough contiguous
// substring of the longer (the explicit-marker form strips a "Decision:" /
// "I'll go with" lead-in, so exact-key dedup alone misses restatements).
// Exported for the reconcile step so "already captured" uses identical
// semantics to intra-extract dedup.
func SameFork(a, b string) bool {
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= minContainmentLen && strings.Contains(long, short)
}

// Extract scans transcript rows for prose decision forks. Only prose Rows
// (assistant/user text) are considered — tool_use/tool_result/thinking are
// either already structured (Phase B) or not decisions. A fork restated
// across turns (commonly "I'll go with X" then "Decision: X") collapses to
// one candidate at the strongest confidence, enriched with any why/alts
// seen on any occurrence; order is the transcript order of first occurrence
// so a resuming reader sees forks chronologically. Deterministic: same rows
// in ⇒ same candidates out.
func Extract(rows []transcript.Row) []Candidate {
	var order []*Candidate

	for _, r := range rows {
		if r.Kind != transcript.KindText {
			continue
		}
		role := r.Role
		for _, stmt := range splitStatements(r.Text) {
			txt, conf, ok := scoreStatement(stmt, role)
			if !ok || txt == "" {
				continue
			}
			rationale, rejected := extractRationaleAndAlts(stmt)
			key := Norm(txt)

			var hit *Candidate
			for _, s := range order {
				if SameFork(Norm(s.Text), key) {
					hit = s
					break
				}
			}
			if hit != nil {
				// Same fork restated: keep the strongest verdict text +
				// confidence, enrich missing why/alts. Never downgrade.
				if conf > hit.Confidence {
					hit.Confidence = conf
					hit.Text = txt
					hit.Role = role
				}
				if hit.Rationale == "" {
					hit.Rationale = rationale
				}
				if hit.RejectedAlts == "" {
					hit.RejectedAlts = rejected
				}
				continue
			}
			order = append(order, &Candidate{
				Text:         txt,
				Rationale:    rationale,
				RejectedAlts: rejected,
				Confidence:   conf,
				Role:         role,
			})
		}
	}

	out := make([]Candidate, 0, len(order))
	for _, s := range order {
		out = append(out, *s)
	}
	return out
}
