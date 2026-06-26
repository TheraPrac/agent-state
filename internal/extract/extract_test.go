package extract

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/theraprac/agent-state/internal/transcript"
)

func textRow(role, text string) transcript.Row {
	return transcript.Row{Kind: transcript.KindText, Role: role, Text: text}
}

// TestExtract_ExplicitMarkerHighConfidence: an explicit "Decision:" marker is
// the strongest signal — recorded as a fact (above ConfirmThreshold), with
// the rationale pulled out of the same statement.
func TestExtract_ExplicitMarkerHighConfidence(t *testing.T) {
	rows := []transcript.Row{
		textRow("assistant", "Decision: gate decision-capture per-agent because a peer changelog write is a coordination violation."),
	}
	got := Extract(rows)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.Confidence < ConfirmThreshold {
		t.Errorf("explicit marker must be above ConfirmThreshold, got %.2f", c.Confidence)
	}
	if c.NeedsConfirm() {
		t.Errorf("explicit marker should not need boundary confirm")
	}
	if c.Rationale == "" {
		t.Errorf("rationale after 'because' must be captured; got %+v", c)
	}
}

// TestExtract_ChoseOverCapturesRejectedAlt: "chose X over Y" — the discarded
// option is the non-re-derivable half and must be captured.
func TestExtract_ChoseOverCapturesRejectedAlt(t *testing.T) {
	got := Extract([]transcript.Row{
		textRow("assistant", "We are going with PreCompact over a Stop-only design because Stop is not guaranteed on kill."),
	})
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].RejectedAlts == "" {
		t.Errorf("rejected alternative (Stop-only) must be captured; got %+v", got[0])
	}
	if got[0].Rationale == "" {
		t.Errorf("rationale (Stop not guaranteed) must be captured; got %+v", got[0])
	}
}

// TestExtract_OperatorOverrideIsAFork: an operator redirection in a user
// turn is a settled fork even with no marker word — mid confidence (acted
// on, above threshold) since it is prose-fuzzy.
func TestExtract_OperatorOverrideIsAFork(t *testing.T) {
	got := Extract([]transcript.Row{
		textRow("user", "No, don't put the guard only in the resolver — enforce it on the explicit path too."),
	})
	if len(got) != 1 {
		t.Fatalf("want 1 operator-override candidate, got %d: %+v", len(got), got)
	}
	if got[0].Role != "user" {
		t.Errorf("override must be attributed to the operator (user), got %q", got[0].Role)
	}
	if got[0].NeedsConfirm() {
		t.Errorf("operator override should be above ConfirmThreshold (acted on), got %.2f", got[0].Confidence)
	}
}

// TestExtract_BareAgentIntentNeedsConfirm: weak agent intent + reason is
// kept (backstop must not drop it) but lands below threshold so the resume
// handshake asks rather than asserts.
func TestExtract_BareAgentIntentNeedsConfirm(t *testing.T) {
	got := Extract([]transcript.Row{
		textRow("assistant", "I'll probably keep the extractor conservative because precision matters more than recall here."),
	})
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if !got[0].NeedsConfirm() {
		t.Errorf("bare agent intent must need boundary confirm, conf=%.2f", got[0].Confidence)
	}
}

// TestExtract_IgnoresNonDecisionProseAndNonText: ordinary narration and
// non-text rows (tool spam, thinking) produce nothing — high precision.
func TestExtract_IgnoresNonDecisionProseAndNonText(t *testing.T) {
	rows := []transcript.Row{
		textRow("assistant", "Let me read the file and check the test output now."),
		textRow("user", "thanks, looks good"),
		{Kind: transcript.KindToolUse, Role: "assistant", Text: "Decision: this is tool input, not prose"},
		{Kind: transcript.KindThinking, Role: "assistant", Text: "Decision: thinking is not a committed fork"},
	}
	if got := Extract(rows); len(got) != 0 {
		t.Fatalf("non-decision prose / non-text rows must yield nothing, got %+v", got)
	}
}

// TestExtract_DeterministicDedupKeepsStrongest: the same fork restated
// across turns collapses to one candidate at the strongest confidence, in
// first-occurrence order — deterministic same-in/same-out.
func TestExtract_DeterministicDedupKeepsStrongest(t *testing.T) {
	rows := []transcript.Row{
		textRow("assistant", "I'll go with agent-scoped resolution because peers collide."),
		textRow("assistant", "Decision: agent-scoped resolution because peers collide."),
	}
	a := Extract(rows)
	b := Extract(rows)
	if len(a) != 1 {
		t.Fatalf("restated fork must dedup to 1, got %d: %+v", len(a), a)
	}
	if a[0].Confidence < ConfirmThreshold {
		t.Errorf("dedup must keep the STRONGEST occurrence (the explicit marker), got %.2f", a[0].Confidence)
	}
	if len(a) != len(b) || a[0].Text != b[0].Text || a[0].Confidence != b[0].Confidence {
		t.Errorf("Extract must be deterministic: %+v vs %+v", a, b)
	}
}

// TestExtract_RejectedMarker: an explicit "Rejected:" line is a high-signal
// record of a discarded alternative (so it is not re-litigated).
func TestExtract_RejectedMarker(t *testing.T) {
	got := Extract([]transcript.Row{
		textRow("assistant", "Rejected: a Stop-only design — Stop is best-effort and skipped on SIGKILL."),
	})
	if len(got) != 1 || got[0].Confidence < ConfirmThreshold {
		t.Fatalf("explicit Rejected marker must be a high-confidence candidate, got %+v", got)
	}
}

// TestExtract_PrecisionRejectsNarration is the regression guard for the
// PR #124 review: the prior loose regexes recorded confidently-wrong forks
// from prose that merely *discusses* decisions. None of these may extract.
func TestExtract_PrecisionRejectsNarration(t *testing.T) {
	noFork := []string{
		"The decision-capture hook fires on PostToolUse for AskUserQuestion.",
		"I refactored the decision writers and the decided-against list.",
		"going with caution here, not rushing the migration",
		"picked up the task, not done yet",
		"The CI rejected the push because lint failed.",
		"the server rejected the request with a 403.",
		"stop by the docs when you can",
		"don't forget to run the unit tests before pushing",
		"actually, the weather is nice today",
		"Run the e2e tests since the auth flow changed.",
	}
	for _, s := range noFork {
		// assistant AND user role — operator-override path must not fire either.
		for _, role := range []string{"assistant", "user"} {
			if got := Extract([]transcript.Row{textRow(role, s)}); len(got) != 0 {
				t.Errorf("[%s] narration must NOT extract a fork: %q ⇒ %+v", role, s, got)
			}
		}
	}
}

// TestExtract_DecidedToIsHighConfidence: "decided to/on/against X" is a
// deliberate decision phrasing (no colon) and must score as a fact.
func TestExtract_DecidedToIsHighConfidence(t *testing.T) {
	got := Extract([]transcript.Row{textRow("assistant", "We decided to scope the extractor high-precision because a wrong fork misleads.")})
	if len(got) != 1 || got[0].NeedsConfirm() {
		t.Fatalf("'decided to …' must be a high-confidence fork, got %+v", got)
	}
	if got[0].Rationale == "" {
		t.Errorf("rationale after because must be captured: %+v", got[0])
	}
}

// TestCondense_RuneSafeCap: truncation must not split a multibyte rune and
// must respect the rune cap (no invalid UTF-8 in the persisted changelog).
func TestCondense_RuneSafeCap(t *testing.T) {
	long := strings.Repeat("é—", 400) // multibyte runes straddling the cap
	got := condense(long)
	if !utf8.ValidString(got) {
		t.Errorf("condense produced invalid UTF-8 (mid-rune byte slice)")
	}
	if n := utf8.RuneCountInString(got); n > maxField {
		t.Errorf("condense exceeded rune cap: %d > %d", n, maxField)
	}
}

// TestSameFork_LengthWindowRejectsRefinement: a later, narrower/contradicting
// decision must NOT be merged into an earlier one (the final decision is what
// a resuming session needs); a mere lead-in strip still dedups.
func TestSameFork_LengthWindowRejectsRefinement(t *testing.T) {
	base := Norm("use the agent-scoped resolver")
	refined := Norm("use the agent-scoped resolver only as a fallback, explicit id always wins")
	if SameFork(base, refined) {
		t.Errorf("a clause-level refinement must NOT be merged as the same fork")
	}
	leadIn := Norm("Decision: use the agent-scoped resolver")
	if !SameFork(base, leadIn) {
		t.Errorf("a marker lead-in strip MUST still dedup as the same fork")
	}
}
