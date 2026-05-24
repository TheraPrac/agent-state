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

	return findings
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
