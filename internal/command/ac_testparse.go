package command

import (
	"fmt"
	"regexp"
	"strings"
)

// testFilter represents a detected test-name filter in a command string.
type testFilter struct {
	runner   string // "go", "jest", "vitest", "playwright", "pytest"
	testName string // raw filter value extracted from the flag
}

// filterRule maps a flag pattern to a runner label.
type filterRule struct {
	runner  string
	pattern *regexp.Regexp
}

// filterRules lists known filter flags in order of specificity.
// Each rule's capture group 1 is the test name / filter expression.
var filterRules = []filterRule{
	// Go: -run TestFoo, -run=TestFoo, RUN=TestFoo (make variable)
	{"go", regexp.MustCompile(`(?:^|\s)-run[= ](\S+)`)},
	{"go", regexp.MustCompile(`(?:^|\s)RUN=(\S+)`)},
	// Jest: -t "name", --testNamePattern "name" / --testNamePattern="name"
	{"jest", regexp.MustCompile(`(?:^|\s)(?:-t|--testNamePattern)[= ]"?([^"\s]+)"?`)},
	// Vitest: --grep "name"
	{"vitest", regexp.MustCompile(`(?:^|\s)--grep[= ]"?([^"\s]+)"?`)},
	// Playwright: --grep "name", -g "name"
	{"playwright", regexp.MustCompile(`(?:^|\s)(?:--grep|-g)[= ]"?([^"\s]+)"?`)},
	// Pytest: -k "expr"
	{"pytest", regexp.MustCompile(`(?:^|\s)-k[= ]"?([^"\s]+)"?`)},
}

// detectRunner identifies the test runner from the command string.
// Returns "" when unrecognized.
func detectRunner(cmd string) string {
	lower := strings.ToLower(cmd)
	switch {
	case strings.Contains(lower, "playwright"):
		return "playwright"
	case strings.Contains(lower, "vitest"):
		return "vitest"
	case strings.Contains(lower, "jest"):
		return "jest"
	case strings.Contains(lower, "pytest"):
		return "pytest"
	case strings.Contains(lower, "go test") || strings.Contains(lower, "make test"):
		return "go"
	default:
		return ""
	}
}

// detectFilter returns the first matching test filter found in cmd, or nil if
// no recognized filter flag is present.  Runner detection from the command
// context takes precedence when multiple rules share the same flag (e.g.
// --grep is used by both vitest and playwright).
func detectFilter(cmd string) *testFilter {
	contextRunner := detectRunner(cmd)
	for _, rule := range filterRules {
		// Skip rules that don't match the detected runner (when we can tell).
		if contextRunner != "" && rule.runner != contextRunner {
			continue
		}
		m := rule.pattern.FindStringSubmatch(cmd)
		if m != nil {
			return &testFilter{runner: rule.runner, testName: m[1]}
		}
	}
	return nil
}

// testOutputParser holds compiled per-runner PASS/FAIL line matchers for a
// specific test name.
type testOutputParser struct {
	passLine *regexp.Regexp
	failLine *regexp.Regexp
}

// buildParser constructs a parser for the given runner and test name.
func buildParser(runner, testName string) testOutputParser {
	esc := regexp.QuoteMeta(testName)
	switch runner {
	case "go":
		// go test -v output: "--- PASS: TestFoo (0.00s)"
		return testOutputParser{
			passLine: regexp.MustCompile(`--- PASS: ` + esc + `(?:\s|$)`),
			failLine: regexp.MustCompile(`--- FAIL: ` + esc + `(?:\s|$)`),
		}
	case "jest", "vitest":
		// ✓ / ✕ prefixed test lines
		return testOutputParser{
			passLine: regexp.MustCompile(`(?:✓|✔|√|PASS)\s+` + esc),
			failLine: regexp.MustCompile(`(?:✕|✗|×|FAIL|●)\s+` + esc),
		}
	case "playwright":
		return testOutputParser{
			passLine: regexp.MustCompile(`(?:✓|passed).*` + esc),
			failLine: regexp.MustCompile(`(?:✗|failed).*` + esc),
		}
	case "pytest":
		return testOutputParser{
			passLine: regexp.MustCompile(`PASSED.*` + esc),
			failLine: regexp.MustCompile(`FAILED.*` + esc),
		}
	default:
		return testOutputParser{}
	}
}

// parseFilteredTestResult scans output for the named test's PASS/FAIL line.
// Returns (passed, found): found=false when no per-test line is present (e.g.,
// non-verbose output) — the caller must fall back to exit-code behavior.
func parseFilteredTestResult(runner, testName, output string) (passed, found bool) {
	p := buildParser(runner, testName)
	if p.passLine == nil {
		return false, false
	}
	hasFail := p.failLine.MatchString(output)
	hasPass := p.passLine.MatchString(output)
	if !hasPass && !hasFail {
		return false, false
	}
	return hasPass && !hasFail, true
}

// evaluateFilteredCmd inspects a non-zero-exit command for a filtered-test
// pass override. Returns a non-nil override when the targeted test definitively
// passed despite the suite's non-zero exit, along with a warning to surface the
// unrelated failure. Returns (nil, "") when no override applies (caller keeps
// exit-code behavior).
func evaluateFilteredCmd(cmd, output string) (override *bool, warning string) {
	f := detectFilter(cmd)
	if f == nil {
		return nil, ""
	}
	passed, found := parseFilteredTestResult(f.runner, f.testName, output)
	if !found {
		// No per-test line present — possibly non-verbose; can't override safely.
		return nil, ""
	}
	if passed {
		t := true
		return &t, fmt.Sprintf(
			"targeted test %q PASSED — suite exited non-zero due to an unrelated failure (use -v / --verbose to surface it)",
			f.testName,
		)
	}
	return nil, ""
}

