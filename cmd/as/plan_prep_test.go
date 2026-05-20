package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// runInProcessCapturingStderr is a sibling of runInProcess that also
// captures stderr. Used by T-376 to assert on the deprecation banner
// emitted by `st prep`. Differs from runInProcess in three ways
// (deliberate, addressing review findings on PR #140):
//
//  1. Pipe drainers run in goroutines started BEFORE app.Execute so
//     output larger than the kernel pipe buffer (~64KB) can't
//     deadlock the test (review F1).
//  2. Env vars and os.Stdout/os.Stderr restoration go through
//     t.Setenv and t.Cleanup so a panic mid-Execute can't poison
//     subsequent tests in the same binary (review F2, F3, F7).
//  3. Cobra's error writer points at a distinct bytes.Buffer rather
//     than the captured-stderr pipe, matching the existing
//     runInProcess pattern so cobra-emitted errors don't co-mingle
//     with the application's direct stderr writes (review F6).
func runInProcessCapturingStderr(t *testing.T, cwd string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stderr): %v", err)
	}

	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	t.Cleanup(func() {
		os.Stdout, os.Stderr = origStdout, origStderr
	})

	// Drain concurrently before Execute starts so a > 64KB write
	// inside the cobra Run callbacks can't block on a full pipe.
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() { _, _ = io.Copy(outBuf, outR); close(outDone) }()
	go func() { _, _ = io.Copy(errBuf, errR); close(errDone) }()

	exitCode = 0
	t.Setenv("AS_AGENT_ID", "test-agent")
	t.Setenv("AS_SESSION_ID", "test-session")

	app := newApp(cwd)
	app.SetArgs(args)
	app.SetErr(&bytes.Buffer{}) // matches runInProcess: suppress cobra errors
	_ = app.Execute()

	outW.Close()
	errW.Close()
	<-outDone
	<-errDone

	return outBuf.String(), errBuf.String(), exitCode
}

// TestStPrepPrintsDeprecationBanner verifies T-376's deprecation
// posture: invoking `st prep` (top-level alias) emits a one-line
// banner on stderr before dispatching.
func TestStPrepPrintsDeprecationBanner(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, stderr, code := runInProcessCapturingStderr(t, ws, "prep", "--dry-run", "T-001")

	if code != 0 {
		t.Fatalf("st prep --dry-run T-001 exit=%d, want 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "as: deprecation:") {
		t.Errorf("expected deprecation prefix `as: deprecation:` in stderr; got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "st plan prep") {
		t.Errorf("expected deprecation banner to point at `st plan prep`; got:\n%s", stderr)
	}
}

// TestStPlanPrepDoesNotPrintDeprecationBanner confirms the new
// subcommand path does NOT emit the deprecation banner. The banner
// belongs to the alias, not the canonical name.
func TestStPlanPrepDoesNotPrintDeprecationBanner(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, stderr, code := runInProcessCapturingStderr(t, ws, "plan", "prep", "--dry-run", "T-001")

	if code != 0 {
		t.Fatalf("st plan prep --dry-run T-001 exit=%d, want 0\nstderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "as: deprecation:") {
		t.Errorf("st plan prep should NOT emit deprecation banner; got stderr:\n%s", stderr)
	}
}

// TestPlanPrepSubcommandDispatchesToSameHandlers asserts that
// `st prep <id>` and `st plan prep <id>` invoke the same code path.
// Both forms go through runPrepDispatch (the shared closure), so
// stdout content MUST be byte-identical. The deprecation banner goes
// to stderr, not stdout, so equality holds on the stdout stream.
// If the two paths ever diverge — extra line, different wording,
// reordered output — this assertion catches it.
func TestPlanPrepSubcommandDispatchesToSameHandlers(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	prepOut, _, prepCode := runInProcessCapturingStderr(t, ws, "prep", "--dry-run", "T-001")
	planPrepOut, _, planPrepCode := runInProcessCapturingStderr(t, ws, "plan", "prep", "--dry-run", "T-001")

	if prepCode != planPrepCode {
		t.Errorf("exit codes diverged: prep=%d, plan prep=%d", prepCode, planPrepCode)
	}
	if prepOut != planPrepOut {
		t.Errorf("stdout parity broken between `st prep` and `st plan prep`:\n--- st prep ---\n%s\n--- st plan prep ---\n%s\n", prepOut, planPrepOut)
	}
	// Belt-and-braces: confirm the shared output is non-empty and
	// looks like a --dry-run announcement, so we catch the case
	// where BOTH paths regress to empty output simultaneously.
	for _, want := range []string{"Would plan", "T-001"} {
		if !strings.Contains(prepOut, want) {
			t.Errorf("expected %q in shared stdout, got:\n%s", want, prepOut)
		}
	}
}

// TestStPlanPrepRoutesStandaloneForSprintlessItem confirms the new
// subcommand actually reaches the PrepStandalone branch when the
// item has no sprint (I-571 routing). Positive signal: PrepStandalone
// emits "Would plan 1 item:" (no "(s)" suffix) on the announcement
// line, while the sprint-mode Prep emits "Would plan N item(s):".
// Asserting the singular form proves the standalone path was taken.
func TestStPlanPrepRoutesStandaloneForSprintlessItem(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, stderr, code := runInProcessCapturingStderr(t, ws, "plan", "prep", "--dry-run", "T-001")

	if code != 0 {
		t.Fatalf("st plan prep --dry-run T-001 exit=%d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "has no sprint assigned") {
		t.Errorf("standalone dispatch should not emit legacy error: %s", stdout)
	}
	// PrepStandalone-specific signal: announcement uses "1 item:"
	// (singular). Sprint-mode Prep uses "N item(s):" with the parens
	// always present. The presence of "Would plan 1 item:" proves
	// the standalone branch fired.
	if !strings.Contains(stdout, "Would plan 1 item:") {
		t.Errorf("expected PrepStandalone announcement `Would plan 1 item:` (no parens); got:\n%s", stdout)
	}
	if strings.Contains(stdout, "Would plan 1 item(s):") {
		t.Errorf("sprint-mode Prep announcement leaked into standalone path:\n%s", stdout)
	}
}

// TestStPlanPrepViaItemFlag is the --item flag-form sibling of the
// test above for `st plan prep`.
func TestStPlanPrepViaItemFlag(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, _, code := runInProcessCapturingStderr(t, ws, "plan", "prep", "--dry-run", "--item", "T-001")

	if code != 0 {
		t.Fatalf("st plan prep --dry-run --item T-001 exit=%d, want 0\nstdout:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Would plan") {
		t.Errorf("expected 'Would plan' in stdout, got:\n%s", stdout)
	}
}

// TestStPrepStillWorks confirms the deprecated alias still functions
// (deprecation is announcement-only; the alias does not refuse). This
// is the back-compat guarantee for the one-release deprecation window.
func TestStPrepStillWorks(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, _, code := runInProcessCapturingStderr(t, ws, "prep", "--dry-run", "T-001")

	if code != 0 {
		t.Fatalf("st prep --dry-run T-001 exit=%d, want 0\nstdout:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Would plan") {
		t.Errorf("expected 'Would plan' in stdout, got:\n%s", stdout)
	}
}
