// I-756: Empirical-claim guard for sbar.background.
//
// SBAR backgrounds routinely contain observation-shaped sentences
// ("final persisted claim state on demo X is Y") without a citation.
// Downstream items inherit those claims as ground truth; when the claim
// can't be reproduced the entire framing collapses.
//
// The validator detects sentences that match empirical-claim patterns
// and lack an evidence pointer or a hypothesis marker. Bypass is
// --evidence-skip "<reason>" (session-capped, audit-logged).
package quality

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/theraprac/agent-state/internal/model"
)

// empiricalClaimREs are patterns that match observation-shaped sentences —
// statements about concrete external-system state that require a source.
var empiricalClaimREs = []*regexp.Regexp{
	// "final persisted X state", "persisted claim state on demo"
	regexp.MustCompile(`(?i)\bfinal\s+(persisted|committed|saved|stored)\b`),
	regexp.MustCompile(`(?i)\b(persisted|committed|saved|stored)\s+\w+\s+state\b`),
	// "closed end-to-end", "round-trip closed"
	regexp.MustCompile(`(?i)\bclosed\s+end.?to.?end\b`),
	regexp.MustCompile(`(?i)\bround.?trip\s+closed\b`),
	// "confirmed X", "saw X", "verified X" as a direct observation
	regexp.MustCompile(`(?i)\b(saw|confirmed|verified)\s+the\s+round.?trip\b`),
	regexp.MustCompile(`(?i)\b(saw|confirmed|verified)\s+(it|the\s+gate|the\s+flow|the\s+hook|the\s+pipeline)\b`),
	// "the gate is firing in <env>", "the gate fires on <env>"
	regexp.MustCompile(`(?i)\bthe\s+gate\s+(is\s+firing|fires)\b`),
}

// hypothesisMarkerREs exempt a sentence — the claim is speculative,
// not a stated observation.
var hypothesisMarkerREs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\[hypothesis\]`),
	regexp.MustCompile(`(?i)\blikely\b`),
	regexp.MustCompile(`(?i)\bprobably\b`),
	regexp.MustCompile(`(?i)\bsuspect\b`),
	regexp.MustCompile(`(?i)\bmay\s+\w+\b`),
	regexp.MustCompile(`(?i)\bcould\s+\w+\b`),
	regexp.MustCompile(`(?i)\bshould\s+\w+\b`),
	regexp.MustCompile(`(?i)\bseems?\b`),
}

// sectionEvidencePointerREs are strong explicit citations that ground the
// entire background section. If any of these match anywhere in the background,
// all claims in it are considered sourced.
//
// UUID is intentionally absent here — a UUID embedded in background prose
// (e.g. a tenant-ID reference or a dependency ID) does not prove that the
// surrounding empirical claims are sourced. UUIDs ground only the specific
// sentence they appear in (see sentenceUUIDRE below).
var sectionEvidencePointerREs = []*regexp.Regexp{
	regexp.MustCompile(`https?://`),
	regexp.MustCompile(`(?i)\btest\s+run\b`),
	regexp.MustCompile(`(?i)\b(DB|database)\s+(query|read|result)\b`),
	regexp.MustCompile(`(?i)\bscreenshot\b`),
	regexp.MustCompile(`(?i)\bgit\s+(log|blame|show|commit)\b`),
	regexp.MustCompile(`(?i)\bpr\s*#\d+\b`),
	regexp.MustCompile(`(?i)\bgrep\s+(output|result)\b`),
	regexp.MustCompile(`(?i)\bquery\s+result\b`),
	regexp.MustCompile(`(?i)\bS3\.archived\b`),
	regexp.MustCompile(`(?i)\bdirect\s+(DB|database)\s+read\b`),
	regexp.MustCompile(`(?i)\blog\s+(line|output|entry)\b`),
}

// sentenceUUIDRE matches a UUID and grounds the specific sentence it appears
// in (e.g. "tenant ea94525e-... showed 6 rows" is anchored to a real row).
var sentenceUUIDRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// reSentenceSplit splits text into sentence-like fragments.
var reSentenceSplit = regexp.MustCompile(`[.!?\n]+`)

// splitSentences splits text into sentence-like spans on `.`, `!`, `?`, or newline.
// Fragments shorter than 10 chars are dropped (headings, bullets, etc.).
func splitSentences(text string) []string {
	parts := reSentenceSplit.Split(text, -1)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 10 {
			out = append(out, p)
		}
	}
	return out
}

func matchesAny(text string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// ValidateBackgroundEvidenceClaims scans sbar.background for observation-
// shaped sentences that lack evidence pointers. Returns a Violation for
// each offending sentence. Returns nil when clean.
//
// Hypothesis-marked sentences (likely/probably/suspect/[hypothesis]/…) are
// exempt — they are conjectural, not stated observations.
//
// Whole-section shortcut: if a strong explicit citation (URL, test run,
// DB read, etc.) appears anywhere in the background, the whole section is
// considered grounded. This handles a single citation at the top or bottom
// covering all claims in a paragraph. UUID does NOT trigger the shortcut —
// it grounds only the specific sentence it appears in.
//
// I-756.
func ValidateBackgroundEvidenceClaims(item *model.Item) []Violation {
	background := strings.TrimSpace(item.SBAR.Background)
	if background == "" {
		return nil
	}

	// Whole-section shortcut: a strong citation anywhere grounds the section.
	if matchesAny(background, sectionEvidencePointerREs) {
		return nil
	}

	var out []Violation
	sentences := splitSentences(background)
	for _, sent := range sentences {
		if matchesAny(sent, hypothesisMarkerREs) {
			continue
		}
		if !matchesAny(sent, empiricalClaimREs) {
			continue
		}
		// Per-sentence evidence: a strong citation OR a UUID in the same
		// sentence grounds this specific claim.
		if matchesAny(sent, sectionEvidencePointerREs) || sentenceUUIDRE.MatchString(sent) {
			continue
		}
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "sbar.background",
			Message: fmt.Sprintf(
				"observation-shaped claim without an evidence pointer: %q — "+
					"add a source (test run ID, DB query output, git commit, screenshot, URL) "+
					"or mark as hypothesis with [hypothesis]/likely/probably. "+
					"Bypass: --evidence-skip \"<reason>\" (audit-logged). I-756.",
				truncate(sent, 120),
			),
		})
	}
	return out
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
