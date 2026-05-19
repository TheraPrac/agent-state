package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/model"
)

func TestDetectTapeGap(t *testing.T) {
	// git ahead of tape ⇒ loud gap, not verified
	g := detectTapeGap(2, 5, "fix/I-679")
	if !g.gap || g.verified {
		t.Fatalf("expected gap+unverified, got %+v", g)
	}
	if !strings.Contains(g.message, "3 uncaptured") {
		t.Errorf("gap message should quantify the delta: %q", g.message)
	}
	// tape consistent (equal) ⇒ verified, no gap
	c := detectTapeGap(3, 3, "fix/I-679")
	if c.gap || !c.verified {
		t.Errorf("equal counts should verify: %+v", c)
	}
	// tape ahead of git (e.g. amended/rebased) ⇒ still verified, never a
	// false gap alarm
	a := detectTapeGap(4, 2, "fix/I-679")
	if a.gap || !a.verified {
		t.Errorf("tape-ahead must not raise a gap: %+v", a)
	}
}

func TestFilterSession(t *testing.T) {
	entries := []changelog.Entry{
		{Op: "create", SessionID: "s1"},
		{Op: "commit", SessionID: "s2"},
		{Op: "update", SessionID: ""}, // unsessioned — always included
	}
	// scoped to s1: keeps s1 + unsessioned
	got := filterSession(entries, "s1")
	if len(got) != 2 {
		t.Errorf("s1 filter: got %d, want 2 (s1 + unsessioned)", len(got))
	}
	// empty session ⇒ all
	if len(filterSession(entries, "")) != 3 {
		t.Errorf("empty session should return all")
	}
	// non-matching session still keeps unsessioned (global) context — that
	// is correct scoping, not stranding: just the "" entry here.
	if got := filterSession(entries, "zzz"); len(got) != 1 || got[0].Op != "update" {
		t.Errorf("non-matching session should keep unsessioned context only, got %d entries", len(got))
	}
	// genuinely empty result (no match AND no unsessioned entries) ⇒ fall
	// back to the full set rather than an empty replay.
	sessioned := []changelog.Entry{{Op: "create", SessionID: "s1"}, {Op: "commit", SessionID: "s2"}}
	if len(filterSession(sessioned, "zzz")) != 2 {
		t.Errorf("empty scoped result must fall back to all, not strand the reader")
	}
}

func TestLastSessionID(t *testing.T) {
	if got := lastSessionID(&model.Item{Sessions: []string{"a", "b", "c"}}); got != "c" {
		t.Errorf("got %q, want last session c", got)
	}
	if got := lastSessionID(&model.Item{ClaimedBy: "claim-sess"}); got != "claim-sess" {
		t.Errorf("got %q, want claim fallback", got)
	}
	if got := lastSessionID(&model.Item{}); got != "" {
		t.Errorf("got %q, want empty (item-wide replay)", got)
	}
}

func TestRenderResume_GapBannerFirstAndDecisionsVerbatim(t *testing.T) {
	item := &model.Item{
		ID: "I-679", Title: "xsession record", Type: "issue", Status: "active",
		Sessions:     []string{"sess-1"},
		PlanApproved: true, PlanApprovedBy: "agent-c",
		WorkTracking: map[string]interface{}{"branch": "fix/I-679-xsession-record"},
		Delivery:     map[string]interface{}{"stage": "coding"},
	}
	entries := []changelog.Entry{
		{Timestamp: "2026-05-18T21:00:00-06:00", Op: "start", SessionID: "sess-1"},
		{Timestamp: "2026-05-18T21:10:00-06:00", Op: "commit", SessionID: "sess-1", NewValue: "abc1234"},
		{Timestamp: "2026-05-18T21:20:00-06:00", Op: "update", SessionID: "sess-1",
			Kind: changelog.KindDecision, Source: changelog.SourceStructured,
			Reason: "parallel over sequence — Phase 1 substrate already merged, not blocked on agent-b"},
		{Timestamp: "2026-05-18T21:25:00-06:00", Op: "update", SessionID: "sess-1",
			Kind: changelog.KindDecision, Source: changelog.SourceExtracted, Confidence: 0.4,
			Reason: "rejected: rebuild storage subsystem"},
	}
	gap := tapeAudit{gap: true, message: "branch has 3 commits but 1 on tape — 2 uncaptured."}

	out := renderResume(item, entries, "sess-1", "# Plan\nDo the thing.", "", gap)

	// Gap banner must precede the State/Decisions sections — a record that
	// looks complete but is not must be impossible to miss.
	gi := strings.Index(out, "EXECUTION-TAPE GAP")
	si := strings.Index(out, "## State")
	di := strings.Index(out, "## Decisions")
	if gi < 0 || si < 0 || di < 0 {
		t.Fatalf("missing required sections in:\n%s", out)
	}
	if !(gi < si && si < di) {
		t.Errorf("ordering wrong: gap=%d state=%d decisions=%d", gi, si, di)
	}
	// Structured decision verbatim, extracted decision flagged for confirm.
	if !strings.Contains(out, "[structured] parallel over sequence") {
		t.Errorf("structured decision not rendered verbatim:\n%s", out)
	}
	if !strings.Contains(out, "CONFIRM if acting on this") || !strings.Contains(out, "confidence 0.40") {
		t.Errorf("extracted low-confidence decision not flagged:\n%s", out)
	}
	// Exec tape present; plan folded in; live-regeneration footer present.
	if !strings.Contains(out, "## Execution tape") || !strings.Contains(out, "abc1234") {
		t.Errorf("exec tape missing:\n%s", out)
	}
	if !strings.Contains(out, "## Plan (.plans/I-679.md)") {
		t.Errorf("plan not folded in:\n%s", out)
	}
	if !strings.Contains(out, "Regenerated live from the changelog") {
		t.Errorf("missing live-regeneration footer (anti-snapshot):\n%s", out)
	}
}

func TestRenderResume_UnverifiedNeverReadsAsClean(t *testing.T) {
	item := &model.Item{ID: "T-1", Title: "x", Type: "task", Status: "active"}
	out := renderResume(item, nil, "", "", "NOT FOUND — expected .plans/T-1.md", tapeAudit{
		verified: false, message: "no resolvable git worktree — exec tape is UNVERIFIED.",
	})
	if !strings.Contains(out, "UNVERIFIED") {
		t.Errorf("unverified audit must surface loudly, not as clean:\n%s", out)
	}
	if strings.Contains(out, "verified against ground truth") {
		t.Errorf("unverified state must NOT render the clean banner:\n%s", out)
	}
}

func TestRenderResume_VerifiedPathRendersCleanBannerFirst(t *testing.T) {
	item := &model.Item{ID: "I-679", Title: "x", Type: "issue", Status: "active"}
	out := renderResume(item, nil, "", "", "NOT FOUND — expected .plans/I-679.md", tapeAudit{
		verified: true, message: `branch "b": 3 commit(s), 3 on the recorded exec tape — consistent.`,
	})
	if !strings.Contains(out, "✓ Execution tape verified against ground truth") {
		t.Errorf("verified audit must render the clean banner:\n%s", out)
	}
	// Clean banner still precedes State — ordering is invariant across all
	// three switch arms (gap / unverified / verified).
	ci := strings.Index(out, "verified against ground truth")
	si := strings.Index(out, "## State")
	if ci < 0 || si < 0 || ci > si {
		t.Errorf("verified banner must precede ## State: banner=%d state=%d", ci, si)
	}
	if strings.Contains(out, "UNVERIFIED") || strings.Contains(out, "EXECUTION-TAPE GAP") {
		t.Errorf("verified path must not emit gap/unverified text:\n%s", out)
	}
}

// I-690: the two artifacts a cold session most needs — the plan body and
// next_actions — must both render, and Next must sit between State and the
// historical record (forward directive before backward narrative).
func TestRenderResume_PlanAndNextRendered(t *testing.T) {
	item := &model.Item{
		ID: "I-679", Title: "xsession record", Type: "issue", Status: "active",
		NextActions: []string{
			"Phase B remaining increment: PostToolUse hook + SessionStart compact",
			"", // empty entries must be filtered, not rendered as a blank arrow
			"Then Phase C (PreCompact/Stop backstop)",
		},
	}
	out := renderResume(item, nil, "", "# I-690 Plan\nDo the renderer fix.", "",
		tapeAudit{verified: true, message: "consistent."})

	si := strings.Index(out, "## State")
	ni := strings.Index(out, "## Next")
	pi := strings.Index(out, "## Plan (.plans/I-679.md)")
	if si < 0 || ni < 0 || pi < 0 {
		t.Fatalf("missing State/Next/Plan section:\n%s", out)
	}
	if !(si < ni && ni < pi) {
		t.Errorf("ordering must be State < Next < Plan: state=%d next=%d plan=%d", si, ni, pi)
	}
	if !strings.Contains(out, "→ Phase B remaining increment: PostToolUse hook + SessionStart compact") ||
		!strings.Contains(out, "→ Then Phase C (PreCompact/Stop backstop)") {
		t.Errorf("next_actions not rendered:\n%s", out)
	}
	if strings.Contains(out, "→ \n") || strings.Contains(out, "→ \n\n") {
		t.Errorf("empty next_actions entry must be filtered, not rendered:\n%s", out)
	}
	if !strings.Contains(out, "Do the renderer fix.") {
		t.Errorf("plan body not folded in:\n%s", out)
	}
}

// I-690: a missing/unreadable/empty plan must be LOUD, never a silent omit
// (operator silent-failure principle). The normal Plan header must NOT appear
// — a cold session must not mistake "no plan shown" for "no plan needed".
func TestRenderResume_MissingPlanIsLoudNotSilent(t *testing.T) {
	item := &model.Item{ID: "I-690", Title: "x", Type: "issue", Status: "active"}
	out := renderResume(item, nil, "", "",
		"NOT FOUND — expected /ws/.plans/I-690.md",
		tapeAudit{verified: true, message: "consistent."})

	if !strings.Contains(out, "## ⚠️  PLAN NOT FOUND — expected /ws/.plans/I-690.md") {
		t.Errorf("missing plan must render a loud ⚠️ block:\n%s", out)
	}
	if !strings.Contains(out, "re-run `st resume I-690`") {
		t.Errorf("loud plan block must tell the operator how to repair it:\n%s", out)
	}
	if strings.Contains(out, "## Plan (.plans/I-690.md)") {
		t.Errorf("the normal Plan header must NOT appear when the plan is missing:\n%s", out)
	}
}

func TestFlattenLine(t *testing.T) {
	if got := flattenLine("a\nb\r\nc   d"); got != "a b c d" {
		t.Errorf("flattenLine collapse failed: %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := flattenLine(long); len(got) != 200 || !strings.HasSuffix(got, "...") {
		t.Errorf("flattenLine should cap at 200 with ellipsis, got len %d", len(got))
	}
}
