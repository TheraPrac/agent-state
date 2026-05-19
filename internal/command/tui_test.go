package command

import (
	"bytes"
	"strings"
	"testing"
)

// tuiRender drives tuiTo against an in-memory writer for headless
// assertions — no tea.NewProgram, no TTY, no flakes.
func tuiRender(t *testing.T, opts TuiOpts) (string, int) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	// setupTestEnv leaves the queue empty; add the seeded T-001 so the
	// default-focus path has something to anchor on (no --item flag).
	QueueAdd(s, cfg, "T-001", QueueOpts{})
	var buf bytes.Buffer
	rc := tuiTo(&buf, s, cfg, opts)
	return buf.String(), rc
}

// All four Layout-A panels must appear in the rendered frame, and the
// renderer must reuse showFull's section glyphs / recommend's "why" line
// (the §7 invariant — no facet logic duplicated).
func TestTui_AllFourPanelsRender(t *testing.T) {
	out, rc := tuiRender(t, TuiOpts{Width: 140})
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for _, want := range []string{
		"agents:",                          // agent-strip panel
		"▼ item",                           // composite reuses show --full glyphs
		"planning queue (st recommend top", // planning panel
		"awaiting approval",                // alerts band
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel marker %q missing\n--- output ---\n%s", want, out)
		}
	}
}

func TestTui_ItemFlagFocusesThatItem(t *testing.T) {
	out, rc := tuiRender(t, TuiOpts{Item: "I-001", Width: 140})
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out, "I-001") {
		t.Errorf("--item I-001 must appear in the focused composite\n%s", out)
	}
}

func TestTui_NotFoundItemFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	if rc := tuiTo(&buf, s, cfg, TuiOpts{Item: "NOPE-999"}); rc != 1 {
		t.Errorf("not-found --item must rc=1, got %d", rc)
	}
}

func TestTui_EmptyQueueNoItemFlagFailsLoudly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No QueueAdd here: the queue is empty AND no --item is given, so
	// the default-focus path must fail loudly per the operator
	// silent-failure principle.
	var buf bytes.Buffer
	if rc := tuiTo(&buf, s, cfg, TuiOpts{}); rc != 1 {
		t.Errorf("empty queue + no --item must rc=1, got %d", rc)
	}
}

// Determinism: agent strip + composite + recommend + alerts compose
// reproducibly across runs (the T-369 F1 / T-370 / T-371 discipline).
func TestTui_Deterministic(t *testing.T) {
	run := func() string {
		out, _ := tuiRender(t, TuiOpts{Width: 140})
		return out
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("tui View() must be deterministic\nA:\n%s\nB:\n%s", a, b)
	}
}
