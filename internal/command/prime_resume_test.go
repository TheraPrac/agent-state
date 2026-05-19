package command

import (
	"strings"
	"testing"
)

// TestResumePointer_Format pins the exact pickup-trigger line — a fresh
// session's only cue to load the I-679 cross-session record, so its shape
// must not silently drift.
func TestResumePointer_Format(t *testing.T) {
	got := resumePointer("→", "I-679")
	if !strings.Contains(got, "st resume I-679") {
		t.Errorf("resumePointer must name `st resume <id>`, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("resumePointer must be one terminated line, got %q", got)
	}
	if !strings.Contains(got, "cross-session record") {
		t.Errorf("resumePointer must say WHY (cross-session record), got %q", got)
	}
	// The arrow is caller-supplied so the line matches its host block's
	// glyph (sprintScopedPrime `->` vs globalPrime `→`); the helper must
	// honor it, not hardcode one.
	if !strings.HasPrefix(got, "  → ") {
		t.Errorf("resumePointer must use the caller's arrow, got %q", got)
	}
	if asc := resumePointer("->", "T-9"); !strings.HasPrefix(asc, "  -> ") || !strings.Contains(asc, "st resume T-9") {
		t.Errorf("resumePointer must honor an ASCII arrow too, got %q", asc)
	}
}

// assertResumePointerUnderCurrent checks that in `out` the line directly
// under `Current: <id>` is the `st resume <id>` pickup pointer and the
// next-action line follows it. The fixtures are deterministic (a known
// active item), so a missing Current block is a hard failure (Fatalf) —
// NOT a skip: skipping would turn the regression guard for "the trigger
// always rides in the dashboard" into a silent green pass.
func assertResumePointerUnderCurrent(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	curIdx := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "Current:") {
			curIdx = i
			break
		}
	}
	if curIdx < 0 {
		t.Fatalf("prime emitted no `Current:` block for the known-active fixture — the cold-session pickup trigger is missing:\n%s", out)
	}
	if curIdx+2 >= len(lines) {
		t.Fatalf("Current block truncated:\n%s", out)
	}
	id := strings.Fields(strings.TrimSpace(lines[curIdx]))[1] // "Current: <id>"
	if !strings.Contains(lines[curIdx+1], "st resume "+id) {
		t.Fatalf("line under `Current: %s` must be the `st resume %s` pickup pointer, got %q\nfull:\n%s", id, id, lines[curIdx+1], out)
	}
	if !strings.Contains(lines[curIdx+2], "->") && !strings.Contains(lines[curIdx+2], "→") {
		t.Errorf("the next-action line must follow the resume pointer, got %q", lines[curIdx+2])
	}
}

// TestPrime_ActiveItemEmitsResumePointerBeforeAction exercises the GLOBAL
// path (no sprint binding) — the common cold-session dashboard
// session-start.sh injects.
func TestPrime_ActiveItemEmitsResumePointerBeforeAction(t *testing.T) {
	s, cfg := setupTestEnv(t) // T-003 is the lone active item
	out := captureStdout(t, func() { Prime(s, cfg, PrimeOpts{Compact: true}) })
	assertResumePointerUnderCurrent(t, out)
}

// TestPrime_SprintScopedPathAlsoEmitsResumePointer covers the OTHER
// emission site (sprintScopedPrime) — the half of the "two sites cannot
// drift" claim that was previously untested, and where the ASCII-arrow
// host block lives. A session bound to a sprint containing the active
// item must still get the pickup pointer.
func TestPrime_SprintScopedPathAlsoEmitsResumePointer(t *testing.T) {
	s, cfg, sprintID := setupSprintJoinEnv(t)
	if code := SprintAdd(s, cfg, sprintID, []string{"T-003"}); code != 0 { // T-003 is active
		t.Fatalf("SprintAdd returned %d", code)
	}
	if code := SprintJoin(cfg, sprintID); code != 0 {
		t.Fatalf("SprintJoin returned %d", code)
	}
	if got := resolveSessionSprint(cfg); got != sprintID {
		t.Fatalf("precondition: session not bound to sprint (resolveSessionSprint=%q want %q) — sprintScopedPrime would not run", got, sprintID)
	}
	out := captureStdout(t, func() { Prime(s, cfg, PrimeOpts{Compact: true}) })
	assertResumePointerUnderCurrent(t, out)
}

// TestPrime_ResumePointerScopedToCurrentOnly: the pointer must be emitted
// ONLY as part of a `Current:` active-item block — exactly one per Current,
// never stray elsewhere in the dashboard. Deterministic with the standard
// fixture (one active item ⇒ one Current ⇒ exactly one resume pointer).
func TestPrime_ResumePointerScopedToCurrentOnly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() { Prime(s, cfg, PrimeOpts{Compact: true}) })

	currents := strings.Count(out, "Current:")
	pointers := strings.Count(out, "st resume ")
	if pointers != currents {
		t.Errorf("expected exactly one `st resume` pointer per `Current:` block (currents=%d pointers=%d):\n%s",
			currents, pointers, out)
	}
}
