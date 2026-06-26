package command

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// --- detectFilter ---

func TestDetectTestFilter(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantNil bool
		runner  string
		test    string
	}{
		{
			name:   "go -run flag",
			cmd:    "cd ../as && go test ./internal/command/ -run TestDetermineCascadeAction -count=1",
			runner: "go", test: "TestDetermineCascadeAction",
		},
		{
			name:   "go -run= form",
			cmd:    "go test ./... -run=TestFoo",
			runner: "go", test: "TestFoo",
		},
		{
			name:   "make RUN= variable",
			cmd:    "make test-unit RUN=TestBar",
			runner: "go", test: "TestBar",
		},
		{
			name:   "jest -t flag with quoted multi-word name",
			cmd:    "npx jest -t \"my test name\"",
			runner: "jest", test: "my test name",
		},
		{
			name:   "vitest --grep",
			cmd:    "npx vitest --grep MyComponent",
			runner: "vitest", test: "MyComponent",
		},
		{
			name:   "playwright --grep with quoted multi-word name",
			cmd:    "npx playwright test --grep \"login flow\"",
			runner: "playwright", test: "login flow",
		},
		{
			name:   "pytest -k",
			cmd:    "pytest -k test_login",
			runner: "pytest", test: "test_login",
		},
		{
			name:   "go -run compound regex",
			cmd:    `go test ./... -run "TestAuth|TestLogin" -v`,
			runner: "go", test: "TestAuth|TestLogin",
		},
		{
			name:    "no filter — plain go test",
			cmd:     "go test ./...",
			wantNil: true,
		},
		{
			name:    "no filter — make build",
			cmd:     "make build",
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := detectFilter(tc.cmd)
			if tc.wantNil {
				if f != nil {
					t.Errorf("expected nil filter, got runner=%q test=%q", f.runner, f.testName)
				}
				return
			}
			if f == nil {
				t.Fatalf("expected filter runner=%q test=%q, got nil", tc.runner, tc.test)
			}
			if f.runner != tc.runner {
				t.Errorf("runner: want %q got %q", tc.runner, f.runner)
			}
			if f.testName != tc.test {
				t.Errorf("testName: want %q got %q", tc.test, f.testName)
			}
		})
	}
}

// --- parseFilteredTestResult ---

// Go -v output fixtures

const goVerbosePassAndUnrelatedFail = `=== RUN   TestDetermineCascadeAction
--- PASS: TestDetermineCascadeAction (0.00s)
=== RUN   TestUnrelated
--- FAIL: TestUnrelated (0.01s)
    unrelated_test.go:42: assertion failed
FAIL
`

const goVerboseTargetedFail = `=== RUN   TestDetermineCascadeAction
--- FAIL: TestDetermineCascadeAction (0.01s)
    cascade_test.go:88: wrong result
FAIL
`

const goVerboseOnlyPass = `=== RUN   TestDetermineCascadeAction
--- PASS: TestDetermineCascadeAction (0.00s)
PASS
`

const goNonVerboseNoPerTestLines = `FAIL	github.com/theraprac/as/internal/command	0.042s
`

const goVerboseCompoundPassAndUnrelatedFail = `=== RUN   TestAuth
--- PASS: TestAuth (0.00s)
=== RUN   TestUnrelated
--- FAIL: TestUnrelated (0.01s)
    unrelated_test.go:55: wrong value
FAIL
`

func TestParseFilteredTestResult(t *testing.T) {
	cases := []struct {
		name       string
		runner     string
		testName   string
		output     string
		wantPassed bool
		wantFound  bool
	}{
		{
			name:       "go: targeted PASS + unrelated FAIL → passed=true found=true",
			runner:     "go", testName: "TestDetermineCascadeAction",
			output:     goVerbosePassAndUnrelatedFail,
			wantPassed: true, wantFound: true,
		},
		{
			name:       "go: targeted FAIL → passed=false found=true",
			runner:     "go", testName: "TestDetermineCascadeAction",
			output:     goVerboseTargetedFail,
			wantPassed: false, wantFound: true,
		},
		{
			name:       "go: only PASS line → passed=true found=true",
			runner:     "go", testName: "TestDetermineCascadeAction",
			output:     goVerboseOnlyPass,
			wantPassed: true, wantFound: true,
		},
		{
			name:       "go: non-verbose output → found=false (safe fallback)",
			runner:     "go", testName: "TestDetermineCascadeAction",
			output:     goNonVerboseNoPerTestLines,
			wantPassed: false, wantFound: false,
		},
		{
			name:       "go: different test name → found=false",
			runner:     "go", testName: "TestOtherTest",
			output:     goVerboseOnlyPass,
			wantPassed: false, wantFound: false,
		},
		{
			name:       "go: compound -run regex matches first alt → passed=true found=true",
			runner:     "go", testName: "TestAuth|TestLogin",
			output:     goVerboseCompoundPassAndUnrelatedFail,
			wantPassed: true, wantFound: true,
		},
		{
			name:       "go: invalid -run regex → found=false (safe fallback)",
			runner:     "go", testName: "Test[invalid",
			output:     goVerboseOnlyPass,
			wantPassed: false, wantFound: false,
		},
		{
			name:       "unknown runner → found=false",
			runner:     "unknown", testName: "anything",
			output:     goVerboseOnlyPass,
			wantPassed: false, wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			passed, found := parseFilteredTestResult(tc.runner, tc.testName, tc.output)
			if passed != tc.wantPassed || found != tc.wantFound {
				t.Errorf("parseFilteredTestResult(%q, %q) = (passed=%v, found=%v); want (passed=%v, found=%v)",
					tc.runner, tc.testName, passed, found, tc.wantPassed, tc.wantFound)
			}
		})
	}
}

// --- evaluateFilteredCmd ---

func TestEvaluateFilteredCmd(t *testing.T) {
	cases := []struct {
		name         string
		cmd          string
		output       string
		wantOverride *bool
		wantWarning  bool
	}{
		{
			name:         "filtered + targeted PASS + suite FAIL → override=true",
			cmd:          "go test ./internal/command/ -run TestDetermineCascadeAction -count=1",
			output:       goVerbosePassAndUnrelatedFail,
			wantOverride: boolPtr(true),
			wantWarning:  true,
		},
		{
			name:         "filtered + targeted FAIL → no override",
			cmd:          "go test ./internal/command/ -run TestDetermineCascadeAction -count=1",
			output:       goVerboseTargetedFail,
			wantOverride: nil,
		},
		{
			name:         "filtered + no per-test lines (non-verbose) → no override",
			cmd:          "go test ./internal/command/ -run TestDetermineCascadeAction -count=1",
			output:       goNonVerboseNoPerTestLines,
			wantOverride: nil,
		},
		{
			name:         "no filter → no override",
			cmd:          "go test ./...",
			output:       goVerbosePassAndUnrelatedFail,
			wantOverride: nil,
		},
		{
			name:         "make RUN= + targeted PASS → override=true",
			cmd:          "make test-unit RUN=TestDetermineCascadeAction",
			output:       goVerbosePassAndUnrelatedFail,
			wantOverride: boolPtr(true),
			wantWarning:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override, warning := evaluateFilteredCmd(tc.cmd, tc.output)
			if tc.wantOverride == nil {
				if override != nil {
					t.Errorf("expected nil override, got %v (warning=%q)", *override, warning)
				}
				return
			}
			if override == nil {
				t.Fatalf("expected override=%v, got nil", *tc.wantOverride)
			}
			if *override != *tc.wantOverride {
				t.Errorf("override: want %v got %v", *tc.wantOverride, *override)
			}
			if tc.wantWarning && warning == "" {
				t.Error("expected non-empty warning")
			}
			if !tc.wantWarning && warning != "" {
				t.Errorf("expected empty warning, got %q", warning)
			}
		})
	}
}

// --- evaluateCriterion integration (filtered pass despite suite fail) ---

func TestEvaluateCriterion_FilteredPassDespiteSuiteFail(t *testing.T) {
	// Stub runCmd: exits 1 but stdout has the targeted test passing + unrelated fail.
	stubRunCmd := func(cmd string) ([]byte, int, error) {
		return []byte(goVerbosePassAndUnrelatedFail), 1, nil
	}

	criterion := "cmd: go test ./internal/command/ -run TestDetermineCascadeAction -count=1"
	result := evaluateCriterion(criterion, testItemFixture(), nil, stubRunCmd)

	if !result.Passed {
		t.Errorf("expected Passed=true for filtered test that PASSED despite suite failure, got Detail=%q", result.Detail)
	}
	if result.Detail == "" {
		t.Error("expected non-empty Detail (warning about unrelated failure)")
	}
	if !strings.Contains(result.Detail, "exit 1") {
		t.Errorf("expected Detail to include exit code, got %q", result.Detail)
	}
	if result.Mode != "cmd" {
		t.Errorf("expected Mode=cmd, got %q", result.Mode)
	}
}

func TestEvaluateCriterion_FilteredFailStillFails(t *testing.T) {
	// Stub: targeted test also fails — should NOT override.
	stubRunCmd := func(cmd string) ([]byte, int, error) {
		return []byte(goVerboseTargetedFail), 1, nil
	}

	criterion := "cmd: go test ./internal/command/ -run TestDetermineCascadeAction -count=1"
	result := evaluateCriterion(criterion, testItemFixture(), nil, stubRunCmd)

	if result.Passed {
		t.Error("expected Passed=false when targeted test itself failed")
	}
}

func TestEvaluateCriterion_NoFilterNoOverride(t *testing.T) {
	// Unfiltered failing command — must not override.
	stubRunCmd := func(cmd string) ([]byte, int, error) {
		return []byte("FAIL\n"), 1, nil
	}

	criterion := "cmd: go test ./..."
	result := evaluateCriterion(criterion, testItemFixture(), nil, stubRunCmd)

	if result.Passed {
		t.Error("expected Passed=false for unfiltered failing command")
	}
}

// helpers

func boolPtr(b bool) *bool { return &b }

func testItemFixture() *model.Item {
	return &model.Item{ID: "I-999"}
}
