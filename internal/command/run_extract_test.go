package command

import (
	"strings"
	"testing"
)

// TestExtractRecommendation_PrefersVerdictTokenLineOverNarrative
// is the I-718 regression: a sub-agent report whose body mentions
// the word "recommendation" multiple times — once in narrative
// (e.g. "the updated recommendation reflects...") and once as the
// trailing verdict header — must return the verdict, not the
// narrative.
func TestExtractRecommendation_PrefersVerdictTokenLineOverNarrative(t *testing.T) {
	output := `Plan review complete.

CHANGES MADE: I tightened the updated recommendation in the SBAR.

REMAINING CONCERNS: None.

**RECOMMENDATION** — Accept`
	got := extractRecommendation(output)
	if !strings.Contains(strings.ToLower(got), "accept") {
		t.Errorf("expected verdict containing 'accept'; got %q", got)
	}
}

// TestExtractRecommendation_FallsBackToLastMatchWhenNoVerdictToken
// preserves backward compat: if no candidate has a recognized
// verdict token, return the LAST header-shape candidate's text.
// Today no callers' sub-agents emit verdicts outside the token
// list, but the fallback guards against future divergence.
func TestExtractRecommendation_FallsBackToLastMatchWhenNoVerdictToken(t *testing.T) {
	output := `Some preamble.

RECOMMENDATION: Defer

More body text.

RECOMMENDATION — Hold`
	got := extractRecommendation(output)
	// Neither "Defer" nor "Hold" is a recognized verdict token, so
	// the function returns the LAST header-shape candidate's text.
	if !strings.Contains(strings.ToLower(got), "hold") {
		t.Errorf("expected fallback to LAST header-shape candidate ('Hold'); got %q", got)
	}
}

// TestExtractRecommendation_HandlesMarkdownBoldVerdict verifies
// the header regex accepts markdown-bold formatting (the form
// Claude sub-agents emit by default in summary reports).
func TestExtractRecommendation_HandlesMarkdownBoldVerdict(t *testing.T) {
	cases := []string{
		`**RECOMMENDATION**: Accept`,
		`**RECOMMENDATION** — Accept`,
		`**RECOMMENDATION**: **Accept** — reason`,
		`RECOMMENDATION: Accept`,
	}
	for _, in := range cases {
		got := extractRecommendation(in)
		if !strings.Contains(strings.ToLower(got), "accept") {
			t.Errorf("input %q → got %q; expected verdict containing 'accept'", in, got)
		}
	}
}

// TestExtractRecommendation_ExampleVerdictInRationaleDoesNotWin is
// the I-717 third-attempt failure mode: the sub-agent's report
// body contained an EXAMPLE verdict like
// `RECOMMENDATION: Fresh — ignore the stale signal` as documentation
// of how the parser should behave, and the trailing actual verdict
// was `RECOMMENDATION: Accept`. The fix must return Accept.
func TestExtractRecommendation_ExampleVerdictInRationaleDoesNotWin(t *testing.T) {
	output := `Plan review.

The plan must specify: RECOMMENDATION: Fresh — ignore the stale signal
should map to Fresh, not Stale.

**RECOMMENDATION**: Accept`
	got := extractRecommendation(output)
	if !strings.Contains(strings.ToLower(got), "accept") {
		t.Errorf("expected verdict containing 'accept' (the trailing verdict header); got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "ignore the stale signal") {
		t.Errorf("verdict captured the example narrative instead of the actual verdict: %q", got)
	}
}

// TestExtractRecommendation_EmptyOutputReturnsEmpty preserves the
// existing empty-input contract.
func TestExtractRecommendation_EmptyOutputReturnsEmpty(t *testing.T) {
	if got := extractRecommendation(""); got != "" {
		t.Errorf("expected empty result on empty input; got %q", got)
	}
}

// TestExtractRecommendation_NarrativeOnlyReturnsEmpty: when only
// narrative mentions of "recommendation" exist (no header-shape
// line), the function returns empty (caller falls through to
// menu).
func TestExtractRecommendation_NarrativeOnlyReturnsEmpty(t *testing.T) {
	output := `My recommendation is to follow the spec.
The updated recommendation reflects the new findings.`
	got := extractRecommendation(output)
	if got != "" {
		t.Errorf("narrative-only input should return empty; got %q", got)
	}
}

// TestExtractRecommendation_TwoVerdictHeadersReturnsLast — when a
// sub-agent emits two verdict-token candidates in one report (e.g.,
// echoing a prior iteration's Reject before re-evaluating to
// Accept), the LAST verdict wins. Behavioral pin for I-718's
// last-verdict-wins selection.
func TestExtractRecommendation_TwoVerdictHeadersReturnsLast(t *testing.T) {
	output := `Initial pass:

RECOMMENDATION: Reject

After re-evaluation:

RECOMMENDATION: Accept`
	got := extractRecommendation(output)
	if !strings.Contains(strings.ToLower(got), "accept") {
		t.Errorf("expected LAST verdict (Accept); got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "reject") {
		t.Errorf("LAST-wins selection picked Reject instead of Accept: %q", got)
	}
}

// TestExtractRecommendation_ApproveMapsToAccept — sub-agents that
// emit `RECOMMENDATION: Approve` should map to the Accept menu
// option (parity with the downstream `Contains("approve")` check).
// Review F1 fix: "approve" added to extractRecVerdictTokens.
func TestExtractRecommendation_ApproveMapsToAccept(t *testing.T) {
	got := extractRecommendation(`**RECOMMENDATION**: Approve — looks good`)
	if !strings.HasPrefix(got, "[1] Accept") {
		t.Errorf("expected Approve to route to [1] Accept menu option; got %q", got)
	}
}

// TestExtractRecommendation_RationaleWithSecondEmDashKeepsVerdict —
// review F3 fix: switched LastIndex to Index for separator
// detection, so `RECOMMENDATION — Accept — for reasons above`
// doesn't drop the verdict in favor of the trailing fragment.
func TestExtractRecommendation_RationaleWithSecondEmDashKeepsVerdict(t *testing.T) {
	got := extractRecommendation(`**RECOMMENDATION** — Accept — for reasons above`)
	if !strings.HasPrefix(got, "[1] Accept") {
		t.Errorf("rationale's second em-dash dropped the verdict; got %q", got)
	}
}
