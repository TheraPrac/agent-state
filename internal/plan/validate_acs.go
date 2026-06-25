package plan

import (
	"fmt"
	"regexp"
	"strings"
)

// ACFinding describes one un-verifiable acceptance criterion. The
// caller decides how to render: Save uses it for stderr warnings,
// `st plan approve --strict` uses it as a hard rejection. I-511.
type ACFinding struct {
	Index  int    // 1-based position of the AC in the list (for human-readable error messages)
	AC     string // the offending AC text (verbatim)
	Reason string // why it's not verifiable + a suggested fix
}

func (f ACFinding) String() string {
	return fmt.Sprintf("AC #%d %q: %s", f.Index, f.AC, f.Reason)
}

// ValidateACs reports findings for any acceptance criteria that lack a
// recognizable verification method. An AC is "verifiable" if it
// contains at least one of:
//
//   - a `cmd:` prefix (existing executable-check convention)
//   - a recognized test-suite name (api_unit, web_e2e, go test, ...)
//   - an assertion-shaped verb (returns, exits, equals, asserts, ...)
//   - a Go-style `TestFoo` / JS-style `it("...")` test reference
//   - a measurable threshold (e.g. `< 50ms`, `>= 99%`)
//
// The patterns are deliberately permissive — the goal is to flag the
// `"fix the bug"` and `"works correctly"` shape, not to grade prose
// quality. Returns nil when every AC is verifiable. I-511.
func ValidateACs(acs []string) []ACFinding {
	var findings []ACFinding

	// Pass 1: verifiability — each AC must have a recognizable proof method.
	for i, ac := range acs {
		trimmed := strings.TrimSpace(ac)
		if trimmed == "" {
			findings = append(findings, ACFinding{
				Index:  i + 1,
				AC:     ac,
				Reason: "empty AC — drop or replace with a concrete check",
			})
			continue
		}
		if isVerifiable(trimmed) {
			continue
		}
		findings = append(findings, ACFinding{
			Index:  i + 1,
			AC:     trimmed,
			Reason: "not verifiable — prefix with `cmd:` (e.g. `cmd: go test ./internal/foo/...`), name the test that proves it (e.g. `TestFoo passes`), or include a measurable threshold (e.g. `< 50ms`, `returns 200`)",
		})
	}

	// Pass 2: portability — cmd: ACs must not contain bare workspace-relative
	// paths. "agent-state/" and "theraprac-workspace/" only resolve from the
	// main workspace root; they silently fail when UAT runs from a worktree
	// base. The UAT runner injects $ST_WORKSPACE_ROOT (absolute) so authors
	// can write portable file checks that work in any run context.
	for i, ac := range acs {
		trimmed := strings.TrimSpace(ac)
		if !strings.HasPrefix(strings.ToLower(trimmed), "cmd:") {
			continue
		}
		if hasBareWorkspacePath(trimmed[4:]) {
			findings = append(findings, ACFinding{
				Index:  i + 1,
				AC:     trimmed,
				Reason: `non-portable workspace path — replace "agent-state/" or "theraprac-workspace/" with "$ST_WORKSPACE_ROOT/agent-state/" or "$ST_WORKSPACE_ROOT/theraprac-workspace/" so the check resolves from any run context`,
			})
		}
	}

	// Pass 3: hollow / false-pass detection (I-933). A full-corpus audit of
	// the plan-review sub-agent showed ~half its value was catching one
	// recurring shape — an AC that exits 0 without actually exercising the
	// behavior it claims to verify. These patterns are mechanizable, so they
	// move from a 4-6min LLM re-explore into this <1s deterministic gate.
	//
	// The checks are deliberately HIGH-PRECISION (only unambiguous
	// always-pass shapes are errors). The fuzzier semantic case — a
	// `go test -run X` / `pytest -k X` filter that silently matches zero
	// tests — is NOT flagged here because a static check cannot tell a typo
	// from a legitimate filter without false-flagging good ACs; that judgment
	// is left to the opt-in `--review` sub-agent. Keeping this gate correct
	// (no new flaky friction) is the governing constraint (I-1478).
	for i, ac := range acs {
		trimmed := strings.TrimSpace(ac)
		if !strings.HasPrefix(strings.ToLower(trimmed), "cmd:") {
			continue
		}
		cmd := strings.TrimSpace(trimmed[4:])
		switch {
		case isFailureMasked(cmd):
			findings = append(findings, ACFinding{
				Index:  i + 1,
				AC:     trimmed,
				Reason: "hollow AC — failure is masked by an always-succeeding fallback (`|| true`, `|| :`, `|| echo`, `; true`, `; exit 0`); the check passes regardless of the real result. Remove the fallback so a failure actually fails the AC",
			})
		case isNoOpVerification(cmd):
			findings = append(findings, ACFinding{
				Index:  i + 1,
				AC:     trimmed,
				Reason: "hollow AC — every command always exits 0 (true/:/echo/printf/pwd) so nothing is actually asserted. Replace with a check that fails when the behavior is wrong (run a test, grep with a non-zero-on-absence assertion, or compare output)",
			})
		case hasDisabledTestMarker(cmd):
			findings = append(findings, ACFinding{
				Index:  i + 1,
				AC:     trimmed,
				Reason: "hollow AC — invokes a disabled/skipped test (`xit(`, `it.skip`, `describe.skip`, `t.Skip(`, `@pytest.mark.skip`); a skipped test verifies nothing. Enable the test or point the AC at one that runs",
			})
		}
	}

	return findings
}

// failureMaskPattern matches always-succeeding fallbacks that make a cmd:
// AC pass regardless of the real result. `|| true|:|exit 0|echo` mask
// failure wherever they appear (they are the error branch); `; true|:|exit 0`
// mask only as the terminal command, so they are anchored to end-of-string.
var failureMaskPattern = regexp.MustCompile(`(?i)\|\|\s*(?::|true\b|echo\b|exit\s+0\b)|;\s*(?::|true|exit\s+0)\s*$`)

func isFailureMasked(cmd string) bool {
	return failureMaskPattern.MatchString(cmd)
}

// disabledTestPattern matches references to a disabled/skipped test.
var disabledTestPattern = regexp.MustCompile(`(?i)\bx(?:it|describe|test)\s*\(|\b(?:it|test|describe)\.skip\b|\.skip\s*\(|@pytest\.mark\.skip|\bt\.Skip\s*\(`)

// testSearchTool matches commands that SEARCH for a pattern (rather than run
// it) — an AC that greps for skip markers to PROVE their absence must not be
// flagged as invoking one. Carve-out keeps hasDisabledTestMarker precise.
var testSearchTool = regexp.MustCompile(`\b(?:grep|rg|ag|ripgrep|find|ack)\b`)

func hasDisabledTestMarker(cmd string) bool {
	if testSearchTool.MatchString(cmd) {
		return false
	}
	return disabledTestPattern.MatchString(cmd)
}

// alwaysZeroExitHeads are command heads that always exit 0 (or are pure
// shell builtins with no observable assertion). An AC whose every
// pipeline/sequence segment begins with one of these asserts nothing.
var alwaysZeroExitHeads = map[string]bool{
	"true": true, ":": true, "echo": true, "printf": true,
	"pwd": true, "cd": true, "export": true, "sleep": true,
}

// segmentSplitter splits a shell command into segments on the operators that
// separate distinct commands. If even one segment is a "real" command (its
// head is not in alwaysZeroExitHeads), the AC can fail and is not hollow.
var segmentSplitter = regexp.MustCompile(`&&|\|\||;|\|`)

func isNoOpVerification(cmd string) bool {
	segments := segmentSplitter.Split(cmd, -1)
	sawSegment := false
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		sawSegment = true
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		if !alwaysZeroExitHeads[strings.ToLower(fields[0])] {
			return false // a segment that can fail — not hollow
		}
	}
	return sawSegment
}

// bareWorkspacePathPatterns are substrings whose presence in a cmd: AC
// (without $ST_WORKSPACE_ROOT) indicates a non-portable workspace-relative
// path that silently breaks in worktree UAT runs.
var bareWorkspacePathPatterns = []string{
	"agent-state/",
	"theraprac-workspace/",
}

func hasBareWorkspacePath(cmd string) bool {
	if strings.Contains(cmd, "$ST_WORKSPACE_ROOT") {
		return false
	}
	for _, p := range bareWorkspacePathPatterns {
		if strings.Contains(cmd, p) {
			return true
		}
	}
	return false
}

// suiteNames are the TheraPrac Tier-1/Tier-2 suite identifiers + a few
// common cross-language test runners. Matches case-insensitively.
var suiteNames = []string{
	"api_unit", "api_lint", "api_integration",
	"web_typecheck", "web_unit", "web_integration", "web_e2e",
	"bats", "go test", "pytest", "jest", "vitest", "playwright",
}

// assertionVerbs are the action words that signal an AC is observably
// testable. Match is whole-word, case-insensitive. A single match is
// enough to consider the AC verifiable.
//
// Note: `passes` / `succeeds` were intentionally NOT included here
// because they're commonly used in vague prose ("the feature passes
// review", "everything succeeds"). The verifiable case
// `"TestFoo passes"` is already covered by goTestPattern, which
// matches the named test reference itself.
var assertionVerbs = []string{
	"returns", "exits", "equals", "contains", "matches", "asserts",
	"outputs", "produces", "blocks", "rejects", "accepts", "surfaces",
	"emits", "fails", "denies", "allows",
	"renders", "displays", "shows",
}

// goTestPattern matches Go test names like `TestFoo` or `TestFoo_Bar`.
var goTestPattern = regexp.MustCompile(`\bTest[A-Z]\w*`)

// thresholdPattern matches measurable thresholds. The number must
// carry a unit (or `%`) so vague comparisons like `"errors > 0"` or
// `"coverage > 0%"` aren't treated as testable — both halves of a
// real threshold (the comparator AND a quantifier) must be present.
//
// Recognized shapes:
//   - `<NN[unit]`, `>= NN[unit]`, etc. where unit is a 1-3 letter
//     suffix (ms, s, kb, mb, ...) or `%`
//   - HTTP status references: `status 200`, `200 OK`, `404 Not Found`
var thresholdPattern = regexp.MustCompile(
	// Comparator + number + (% OR unit-suffix-with-word-boundary).
	// `%` is non-word so \b after it doesn't trigger; place \b inside
	// the alternation to anchor only the unit-suffix branch.
	`(?i)(?:[<>]=?|~)\s*\d+(?:\.\d+)?\s*(?:%|[a-z]{1,3}\b)` +
		`|status\s+\d{3}` +
		`|\b\d{3}\s+(?:ok|created|accepted|no\s+content|bad\s+request|unauthorized|forbidden|not\s+found)\b`)

// jsTestPattern matches JavaScript test-name calls: `it("...")`,
// `test("...")`, `describe("...")`. Single-quoted variants too.
var jsTestPattern = regexp.MustCompile(`\b(it|test|describe)\s*\(\s*['"]`)

func isVerifiable(ac string) bool {
	lower := strings.ToLower(ac)

	// cmd: prefix — explicit executable check.
	if strings.HasPrefix(lower, "cmd:") {
		return true
	}

	// Recognized suite names.
	for _, suite := range suiteNames {
		if strings.Contains(lower, suite) {
			return true
		}
	}

	// Assertion verbs (whole-word match).
	for _, verb := range assertionVerbs {
		if containsWord(lower, verb) {
			return true
		}
	}

	// Go / JS test references.
	if goTestPattern.MatchString(ac) {
		return true
	}
	if jsTestPattern.MatchString(ac) {
		return true
	}

	// Measurable thresholds.
	if thresholdPattern.MatchString(ac) {
		return true
	}

	return false
}

// containsWord reports whether word appears in haystack as a whole
// word (bordered by start-of-string, end-of-string, or non-word
// rune). Avoids matching `accepts` inside `unaccepting` and similar.
//
// Operates on runes (not bytes) so multibyte UTF-8 input — em-dashes,
// accented characters, curly quotes — produces correct boundary
// decisions. word itself is assumed to be ASCII (the assertionVerbs
// list is all ASCII).
func containsWord(haystack, word string) bool {
	hayRunes := []rune(haystack)
	wordRunes := []rune(word)
	if len(wordRunes) == 0 || len(hayRunes) < len(wordRunes) {
		return false
	}
	for i := 0; i+len(wordRunes) <= len(hayRunes); i++ {
		match := true
		for j, r := range wordRunes {
			if hayRunes[i+j] != r {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		left := i == 0 || !isWordRune(hayRunes[i-1])
		right := i+len(wordRunes) == len(hayRunes) || !isWordRune(hayRunes[i+len(wordRunes)])
		if left && right {
			return true
		}
	}
	return false
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_'
}
