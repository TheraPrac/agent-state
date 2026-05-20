package command

import (
	"os"
	"strings"
	"testing"
)

// stdinForACTest pipes value into os.Stdin for the duration of fn,
// restoring stdin and stderr after. Used by the I-713 AC validator
// tests to drive UpdateModeStdin deterministically.
func stdinForACTest(t *testing.T, value string, fn func()) {
	t.Helper()
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	_, _ = w.WriteString(value)
	w.Close()
	fn()
}

// suppressStderr swallows stderr for fn. The AC validator emits
// detailed findings to stderr that the tests don't need to inspect.
func suppressStderr(t *testing.T, fn func()) {
	t.Helper()
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() {
		w.Close()
		os.Stderr = origStderr
		_, _ = r.Read(make([]byte, 4096))
	}()
	fn()
}

// runACUpdate is a small wrapper that runs Update on T-001's
// acceptance_criteria via the given mode + value, with stderr
// suppressed so the test output isn't polluted by validator
// findings.
func runACUpdate(t *testing.T, value string, mode UpdateMode) int {
	t.Helper()
	s, cfg := setupTestEnv(t)
	var code int
	if mode == UpdateModeStdin {
		stdinForACTest(t, value, func() {
			suppressStderr(t, func() {
				code = Update(s, cfg, "T-001", "acceptance_criteria", "", mode)
			})
		})
	} else {
		suppressStderr(t, func() {
			code = Update(s, cfg, "T-001", "acceptance_criteria", value, mode)
		})
	}
	return code
}

// TestUpdateACBlocksOnVagueAC — `the feature works` is the canonical
// vague-prose shape plan.ValidateACs rejects.
func TestUpdateACBlocksOnVagueAC(t *testing.T) {
	if code := runACUpdate(t, "- the feature works\n", UpdateModeStdin); code != 2 {
		t.Errorf("expected exit 2 on vague AC; got %d", code)
	}
}

// TestUpdateACBlocksOnPassesReviewAlone — post-I-511 vague-threshold
// rule: `passes review` alone is not testable.
func TestUpdateACBlocksOnPassesReviewAlone(t *testing.T) {
	if code := runACUpdate(t, "- the feature passes review\n", UpdateModeStdin); code != 2 {
		t.Errorf("expected exit 2 on 'passes review' alone; got %d", code)
	}
}

// TestUpdateACBlocksOnMissingCmdPrefix — a `go test ./...` line
// without the `cmd:` prefix is still verifiable per the existing
// validator (recognized suite name `go test` triggers the
// suite-name branch). This test confirms the validator's whitelist:
// the OPPOSITE case (an unprefixed verb that ISN'T a suite name)
// must be rejected.
func TestUpdateACBlocksOnMissingCmdPrefix(t *testing.T) {
	// `do the thing` is unprefixed and not a recognized suite name
	// or assertion verb — should be flagged as un-verifiable.
	if code := runACUpdate(t, "- do the thing\n", UpdateModeStdin); code != 2 {
		t.Errorf("expected exit 2 on prose AC without cmd: prefix or recognized suite; got %d", code)
	}
}

// TestUpdateACPassesOnWellFormedList — clean `cmd:`-prefixed ACs
// write successfully and exit 0.
func TestUpdateACPassesOnWellFormedList(t *testing.T) {
	if code := runACUpdate(t, "- cmd: go test ./...\n- cmd: go vet ./...\n", UpdateModeStdin); code != 0 {
		t.Errorf("expected exit 0 on clean ACs; got %d", code)
	}
}

// TestUpdateACPassesOnNamedTestReference — `TestFoo passes` is
// verifiable per the goTestPattern rule.
func TestUpdateACPassesOnNamedTestReference(t *testing.T) {
	if code := runACUpdate(t, "- TestPlanApprove passes\n", UpdateModeStdin); code != 0 {
		t.Errorf("expected exit 0 on named test reference; got %d", code)
	}
}

// TestUpdateACBlocksAllThreeModes — same vague AC fed through all
// three UpdateMode paths must produce the same exit-2 result. The
// I-713 contract: validation is mode-agnostic.
func TestUpdateACBlocksAllThreeModes(t *testing.T) {
	const vague = "- the feature works"
	for _, mode := range []UpdateMode{UpdateModeValue, UpdateModeStdin} {
		t.Run(modeName(mode), func(t *testing.T) {
			if code := runACUpdate(t, vague, mode); code != 2 {
				t.Errorf("mode %s: expected exit 2; got %d", modeName(mode), code)
			}
		})
	}
	// UpdateModeEditor needs an $EDITOR script to write the value;
	// skipped at the unit-test layer (covered by the Update field
	// switch — the validator runs after the value is sourced from
	// any mode).
}

func modeName(m UpdateMode) string {
	switch m {
	case UpdateModeValue:
		return "value"
	case UpdateModeStdin:
		return "stdin"
	case UpdateModeEditor:
		return "editor"
	}
	return "unknown"
}

// TestUpdateACRefusesEmptyInput — empty piped input is refused
// (the pre-existing empty-stdin guard at update.go ~line 203 catches
// it with exit 1 before the AC validator runs; both paths produce
// the same "refuse the write" outcome).
func TestUpdateACRefusesEmptyInput(t *testing.T) {
	code := runACUpdate(t, "", UpdateModeStdin)
	if code == 0 {
		t.Errorf("expected non-zero exit on empty input; got %d (write should be refused)", code)
	}
}

// TestUpdateACRefusesWhitespaceOnlyInput — input that's only YAML
// list bullets without content (e.g., `-\n-\n`) leaves splitACInput
// with an empty slice, which the I-713 validator refuses with
// exit 2 (it's non-empty after stdin's TrimRight, so the
// pre-existing empty-stdin guard doesn't catch it).
func TestUpdateACRefusesWhitespaceOnlyInput(t *testing.T) {
	if code := runACUpdate(t, "-\n-\n", UpdateModeStdin); code != 2 {
		t.Errorf("expected exit 2 on bullet-only input; got %d", code)
	}
}

// TestSplitACInputStripsListPrefix — the helper that parses the AC
// stdin payload must strip leading `- ` bullets so the validator
// sees the AC content, not the YAML wrapping.
func TestSplitACInputStripsListPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"- cmd: go test ./...", []string{"cmd: go test ./..."}},
		{"- cmd: a\n- cmd: b", []string{"cmd: a", "cmd: b"}},
		{"cmd: go test ./...", []string{"cmd: go test ./..."}},   // no bullet
		{"", nil},                                                  // empty
		{"-\n- cmd: a", []string{"cmd: a"}},                       // stray dash dropped
		{"\n\n- cmd: a\n\n", []string{"cmd: a"}},                  // blank lines stripped
	}
	for _, c := range cases {
		got := splitACInput(c.in)
		if !equalStringSlices(got, c.want) {
			t.Errorf("splitACInput(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUpdateACFindingFormatMatchesAdapter — the stderr finding
// format from the update validator must match the format
// quality.ValidateACList emits (the adapter over plan.ValidateACs).
// Asserted by checking the stderr contains the
// `acceptance_criteria[N]` field prefix.
func TestUpdateACFindingFormatMatchesAdapter(t *testing.T) {
	s, cfg := setupTestEnv(t)
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	stdinForACTest(t, "- the feature works\n", func() {
		_ = Update(s, cfg, "T-001", "acceptance_criteria", "", UpdateModeStdin)
	})
	w.Close()
	os.Stderr = origStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderr := string(buf[:n])
	if !strings.Contains(stderr, "acceptance_criteria[") {
		t.Errorf("expected `acceptance_criteria[i]` field prefix in stderr; got:\n%s", stderr)
	}
}
