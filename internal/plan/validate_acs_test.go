package plan

import (
	"strings"
	"testing"
)

func TestValidateACs_VerifiableShapes(t *testing.T) {
	// Each entry should be considered verifiable — no findings expected.
	verifiable := []string{
		"cmd: go test ./internal/plan/...",
		"cmd: bats tests/foo.bats",
		"api_unit suite passes",
		"web_unit covers the new helper",
		"go test -run TestPreFlightDetectsMidRebase -count=1",
		"TestValidateACs passes",
		"TestWriteOK_RejectsUnknownType passes",
		`it("denies edit when plan not approved", () => {})`,
		"endpoint returns 200 OK",
		"latency < 50ms",
		"error rate >= 99%",
		"asserts the file moved to archive/",
		"hook denies the Edit when plan_approved is false",
		"emits a warning to stderr listing each un-verifiable AC",
	}
	for _, ac := range verifiable {
		t.Run(ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if len(findings) > 0 {
				t.Errorf("expected no findings for %q; got: %v", ac, findings)
			}
		})
	}
}

func TestValidateACsHollow(t *testing.T) {
	// Each of these is a cmd: AC the I-933 AST gate flags as always-exit-0: a
	// bare no-op, or a command whose result is masked (||/;/| with an
	// always-zero terminal). The AST resolves the quoting/sequence cases a
	// regex could not (`echo "a; b"`, `; echo done`, `|| echo failed`).
	hollow := []string{
		"cmd: go test ./... || true",
		"cmd: npm run build || echo failed", // OrStmt, echo terminal is always 0
		"cmd: ./check.sh ; true",
		"cmd: make test; exit 0",
		"cmd: go vet ./... || :",
		"cmd: echo done",
		`cmd: echo "hello world"`,
		`cmd: echo "a; b"`,             // quoted ; — AST keeps it a single echo
		"cmd: go test ./...; echo done", // last statement is a bare echo
		"cmd: true",
		"cmd: :",
		"cmd: go test ./... | true", // pipeline exit = last stage (true)
		"cmd: go test ./... | cat",  // cat reading stdin swallows status
		"cmd: go test ./... | tee",  // tee reading stdin swallows status
		"cmd: echo done > /dev/null", // output redirect to /dev/null can't fail
		"cmd: grep -q x f || echo missing > /dev/null", // mask via echo+/dev/null
		`cmd: printf "all good\n"`,   // conversion-free printf always succeeds
	}
	for _, ac := range hollow {
		t.Run("hollow/"+ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if !hasReasonContaining(findings, "hollow AC") {
				t.Errorf("expected a 'hollow AC' finding for %q; got: %v", ac, findings)
			}
		})
	}

	// These are legitimate cmd: ACs that exit non-zero on real failure — the
	// linter must NOT flag them as hollow. The trailing entries were
	// false-positives in the parser-based revision (escaped quotes, command
	// substitution, printf-under-dash, redirects, mask tokens inside quotes);
	// the end-anchored narrow gate is immune to all of them.
	legit := []string{
		"cmd: go test -run TestFoo -count=1",
		"cmd: cd as && go build ./...",
		"cmd: go run ./cmd/as plan approve --help 2>&1 | grep -q -- --review",
		`cmd: rg "\.skip\(" src/ && exit 1`, // searches for skips to prove absence; ends in exit 1 not exit 0
		"cmd: ls dist/ && cat dist/out.txt",
		"cmd: ./check.sh && echo ok",                                     // && echo only runs after prior passed
		`cmd: ! grep -rn '|| true' .as/plans/`,                          // mask token inside a search pattern
		`cmd: cd "$ST_WORKSPACE_ROOT/out" && pwd`,                       // cd can fail (missing dir)
		"cmd: echo done > /tmp/marker",                                  // redirect can fail
		`cmd: python3 -c "assert 'it.skip(' not in open('a.js').read()"`, // asserts skip absence
		`cmd: printf '%d\n' "$count"`,                                   // printf '%d' on non-numeric exits non-zero
		`cmd: printf "a\"b" && go test ./...`,                           // escaped quote must not desync
		`cmd: test $(echo 1 || echo 0) -gt 0`,                           // operator inside command substitution
	}
	for _, ac := range legit {
		t.Run("legit/"+ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if hasReasonContaining(findings, "hollow AC") {
				t.Errorf("did not expect a 'hollow AC' finding for %q; got: %v", ac, findings)
			}
		})
	}
}

func hasReasonContaining(findings []ACFinding, sub string) bool {
	for _, f := range findings {
		if strings.Contains(f.Reason, sub) {
			return true
		}
	}
	return false
}

func TestValidateACs_UnverifiableShapes(t *testing.T) {
	unverifiable := []string{
		"fix the bug",
		"works correctly",
		"users see the modal",
		"the feature is fast",
		"performance is good",
	}
	for _, ac := range unverifiable {
		t.Run(ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding for %q; got %d: %v", ac, len(findings), findings)
			}
			if !strings.Contains(findings[0].Reason, "not verifiable") {
				t.Errorf("finding reason should mention 'not verifiable'; got %q", findings[0].Reason)
			}
		})
	}
}

func TestValidateACs_EmptyACReportsFinding(t *testing.T) {
	findings := ValidateACs([]string{"", "  "})
	if len(findings) != 2 {
		t.Errorf("expected 2 findings for empty/whitespace ACs; got %d", len(findings))
	}
	for _, f := range findings {
		if !strings.Contains(f.Reason, "empty") {
			t.Errorf("empty AC reason should mention 'empty'; got %q", f.Reason)
		}
	}
}

func TestValidateACs_FindingIndexIs1Based(t *testing.T) {
	findings := ValidateACs([]string{
		"cmd: go test ./...",  // OK, no finding
		"works correctly",     // un-verifiable, finding
		"cmd: pytest",         // OK
		"the feature is fast", // un-verifiable, finding
	})
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings; got %d", len(findings))
	}
	if findings[0].Index != 2 || findings[1].Index != 4 {
		t.Errorf("expected indices [2,4]; got [%d,%d]", findings[0].Index, findings[1].Index)
	}
}

func TestValidateACs_FindingStringIncludesACAndIndex(t *testing.T) {
	findings := ValidateACs([]string{"vague AC"})
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %d", len(findings))
	}
	s := findings[0].String()
	if !strings.Contains(s, "vague AC") {
		t.Errorf("String() should include AC text; got %q", s)
	}
	if !strings.Contains(s, "#1") {
		t.Errorf("String() should include 1-based index; got %q", s)
	}
}

// TestValidateACs_VagueThresholdsRejected covers the post-review
// fix that requires a unit/% on the comparator — a bare comparator
// with no quantifier ("errors > 0") is no longer treated as a
// measurable threshold.
func TestValidateACs_VagueThresholdsRejected(t *testing.T) {
	vague := []string{
		"errors > 0",
		"users > 5",
		"items <= 100",
	}
	for _, ac := range vague {
		t.Run(ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if len(findings) != 1 {
				t.Errorf("expected vague threshold %q to be flagged; got %d findings", ac, len(findings))
			}
		})
	}
}

// TestValidateACs_PassesAloneIsVague covers the post-review fix that
// removed bare `passes` / `succeeds` from assertion verbs. Named
// test references like `TestFoo passes` are still verifiable via
// goTestPattern.
func TestValidateACs_PassesAloneIsVague(t *testing.T) {
	findings := ValidateACs([]string{"the feature passes review"})
	if len(findings) != 1 {
		t.Errorf("expected 'passes' alone in prose to be vague; got %d findings", len(findings))
	}
	// Positive control: TestFoo passes still counts (goTestPattern).
	findings = ValidateACs([]string{"TestFoo passes"})
	if len(findings) != 0 {
		t.Errorf("named test reference should still be verifiable; got %d findings", len(findings))
	}
}

// TestContainsWord_MultibyteUTF8 covers the post-review fix that
// switched containsWord from byte-based to rune-based boundary
// checks. ACs with em-dashes or accented chars now produce correct
// boundary decisions.
func TestContainsWord_MultibyteUTF8(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        bool
	}{
		{"system — accepts the input", "accepts", true},     // em-dash boundary
		{"resumé returns 200", "returns", true},             // accented char doesn't break match
		{"smartquotes “returns” mid-stream", "returns", true}, // curly quotes
	}
	for _, c := range cases {
		t.Run(c.hay+"/"+c.needle, func(t *testing.T) {
			got := containsWord(c.hay, c.needle)
			if got != c.want {
				t.Errorf("containsWord(%q,%q) = %v, want %v", c.hay, c.needle, got, c.want)
			}
		})
	}
}

func TestContainsWord_WordBoundaries(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        bool
	}{
		{"the test passes", "passes", true},
		{"the test passed", "passes", false}, // close-but-not-equal
		{"unaccepting input", "accepts", false},
		{"system accepts the input", "accepts", true},
		{"prefix returns 200 ok suffix", "returns", true},
		{"returnsmetadata", "returns", false},
	}
	for _, c := range cases {
		t.Run(c.hay+"/"+c.needle, func(t *testing.T) {
			got := containsWord(c.hay, c.needle)
			if got != c.want {
				t.Errorf("containsWord(%q,%q) = %v, want %v", c.hay, c.needle, got, c.want)
			}
		})
	}
}

func TestValidateACs_BareWorkspacePathRejected(t *testing.T) {
	// cmd: ACs that reference workspace-relative paths without
	// $ST_WORKSPACE_ROOT must be rejected — they silently fail when UAT
	// runs from a worktree base instead of the main workspace root.
	bad := []string{
		"cmd: test -f agent-state/goals/G-001-alpha-go-live.md",
		"cmd: ls agent-state/tasks/",
		"cmd: test -f theraprac-workspace/agent-state/goals/G-004.md",
		"cmd: grep -q G-001 agent-state/index.md",
	}
	for _, ac := range bad {
		t.Run(ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if len(findings) == 0 {
				t.Errorf("expected portability finding for %q; got none", ac)
			} else if !strings.Contains(findings[0].Reason, "non-portable") {
				t.Errorf("expected non-portable reason for %q; got %q", ac, findings[0].Reason)
			}
		})
	}
}

func TestValidateACs_PortableWorkspacePathAccepted(t *testing.T) {
	// cmd: ACs that use $ST_WORKSPACE_ROOT must be accepted.
	good := []string{
		"cmd: test -f $ST_WORKSPACE_ROOT/agent-state/goals/G-001-alpha-go-live.md",
		"cmd: ls $ST_WORKSPACE_ROOT/agent-state/tasks/",
		"cmd: grep -q G-001 $ST_WORKSPACE_ROOT/agent-state/index.md",
	}
	for _, ac := range good {
		t.Run(ac, func(t *testing.T) {
			findings := ValidateACs([]string{ac})
			if len(findings) > 0 {
				t.Errorf("expected no findings for portable AC %q; got: %v", ac, findings)
			}
		})
	}
}
