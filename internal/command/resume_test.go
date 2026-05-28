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

	out := renderResume(item, entries, "sess-1", "# Plan\nDo the thing.", "", gap, remoteState{})

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
	// Structured decision renders inline as a fact (verbatim).
	if !strings.Contains(out, "[structured] parallel over sequence") {
		t.Errorf("structured decision not rendered verbatim:\n%s", out)
	}
	// I-679 Phase C: a low-confidence EXTRACTED fork must NOT be asserted
	// inline as settled truth — it is consolidated into exactly one
	// "Confirm before acting" boundary block, with its confidence + reason.
	ci := strings.Index(out, "## Confirm before acting")
	if ci < 0 {
		t.Fatalf("low-confidence extracted decision must surface in a boundary-confirm block:\n%s", out)
	}
	if ci < di {
		t.Errorf("confirm block must come after Decisions (boundary, not inline): confirm=%d decisions=%d", ci, di)
	}
	if !strings.Contains(out, "confidence 0.40") || !strings.Contains(out, "rejected: rebuild storage subsystem") {
		t.Errorf("confirm block missing the low-confidence record's confidence/reason:\n%s", out)
	}
	if strings.Contains(out, "[extracted, confidence 0.40]") {
		t.Errorf("low-confidence extracted must NOT be rendered inline as a fact:\n%s", out)
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
	}, remoteState{})
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
	}, remoteState{})
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
		tapeAudit{verified: true, message: "consistent."}, remoteState{})

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
		tapeAudit{verified: true, message: "consistent."}, remoteState{})

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

// TestPriorSessionUnfinalized: the kill/interrupt detector EXCLUDES the
// current/live session (2nd arg) and scans PRIOR ones. Killed prior session
// (activity, no marker, not current) ⇒ true. Marker present ⇒ false.
// The lone-current-session case (mid-session resume) ⇒ false (the critical
// false-positive guard). Read-only prior session (no real activity) ⇒ false.
func TestPriorSessionUnfinalized(t *testing.T) {
	priorDec := changelog.Entry{SessionID: "s1", Op: "decision", Kind: changelog.KindDecision, Source: changelog.SourceStructured, Reason: "x"}
	priorFin := changelog.Entry{SessionID: "s1", Op: sessionFinalizedOp, Kind: changelog.KindTransition}
	curWork := changelog.Entry{SessionID: "s2", Op: "commit", Kind: changelog.KindExec, NewValue: "abc"}

	// s1 killed (activity, no marker); s2 is the current/live session.
	if !priorSessionUnfinalized([]changelog.Entry{priorDec, curWork}, "s2") {
		t.Errorf("a prior session with activity + no marker ⇒ must report killed")
	}
	// s1 cleanly finalized.
	if priorSessionUnfinalized([]changelog.Entry{priorDec, priorFin, curWork}, "s2") {
		t.Errorf("prior session with finalize marker ⇒ must NOT report killed")
	}
	// CRITICAL false-positive guard: only one session, and it IS the
	// current/live one (mid-session resume) — even with activity + no
	// marker, must NOT fire (Stop has not run; session is alive).
	if priorSessionUnfinalized([]changelog.Entry{{SessionID: "s2", Op: "decision", Kind: changelog.KindDecision}}, "s2") {
		t.Errorf("the live/current session must never trip the killed banner")
	}
	// Prior read-only/meta session: no decision/exec activity ⇒ no banner.
	ro := changelog.Entry{SessionID: "s1", Op: "update", Kind: changelog.KindTransition}
	if priorSessionUnfinalized([]changelog.Entry{ro, curWork}, "s2") {
		t.Errorf("a prior read-only session (no real activity) must NOT false-positive")
	}
}

// TestRenderResume_KilledSessionBannerIsLoudAndEarly: a killed PRIOR session
// (distinct from the current one) must be announced in the impossible-to-miss
// top zone (before State); a healthy mid-session resume must NOT show it.
func TestRenderResume_KilledSessionBannerIsLoudAndEarly(t *testing.T) {
	// s1 was killed (activity, no marker); the live session being resumed
	// is s2 (the freshly-started one after the kill).
	item := &model.Item{ID: "I-679", Title: "x", Type: "issue", Status: "active", Sessions: []string{"s1", "s2"}}
	entries := []changelog.Entry{
		{Timestamp: "2026-05-19T10:00:00-06:00", Op: "commit", SessionID: "s1", NewValue: "abc", Kind: changelog.KindExec},
		{Timestamp: "2026-05-19T10:05:00-06:00", Op: "decision", SessionID: "s1",
			Kind: changelog.KindDecision, Source: changelog.SourceStructured, Reason: "a real fork"},
		{Timestamp: "2026-05-19T10:10:00-06:00", Op: "start", SessionID: "s2", Kind: changelog.KindTransition},
	}
	out := renderResume(item, entries, "s2", "", "NOT FOUND — expected .plans/I-679.md", tapeAudit{verified: true, message: "ok"}, remoteState{})
	bi := strings.Index(out, "PREVIOUS SESSION DID NOT FINALIZE")
	si := strings.Index(out, "## State")
	if bi < 0 {
		t.Fatalf("killed prior session must be announced:\n%s", out)
	}
	if si >= 0 && bi > si {
		t.Errorf("killed-session banner must precede ## State (top zone): banner=%d state=%d", bi, si)
	}
	// s1 finalized cleanly ⇒ banner must vanish.
	fin := changelog.Entry{Timestamp: "2026-05-19T10:06:00-06:00", Op: sessionFinalizedOp, SessionID: "s1", Kind: changelog.KindTransition}
	if strings.Contains(renderResume(item, append(entries, fin), "s2", "", "x", tapeAudit{verified: true, message: "ok"}, remoteState{}), "PREVIOUS SESSION DID NOT FINALIZE") {
		t.Errorf("a finalized prior session must NOT show the killed banner")
	}
	// Mid-session resume (only the live session s9, activity, no marker):
	// must NOT show the alarmist banner.
	mid := &model.Item{ID: "I-679", Title: "x", Type: "issue", Status: "active", Sessions: []string{"s9"}}
	midOut := renderResume(mid, []changelog.Entry{
		{Timestamp: "2026-05-19T11:00:00-06:00", Op: "commit", SessionID: "s9", NewValue: "d", Kind: changelog.KindExec},
	}, "s9", "", "x", tapeAudit{verified: true, message: "ok"}, remoteState{})
	if strings.Contains(midOut, "PREVIOUS SESSION DID NOT FINALIZE") {
		t.Errorf("healthy mid-session resume must NOT fire the killed banner:\n%s", midOut)
	}
}

// TestRenderResume_HighConfidenceExtractedIsInlineNotConfirmed: an extracted
// fork AT/above the threshold is a (provisional) fact rendered inline — only
// BELOW-threshold extracted goes to the single boundary-confirm block.
func TestRenderResume_HighConfidenceExtractedIsInlineNotConfirmed(t *testing.T) {
	item := &model.Item{ID: "I-679", Title: "x", Type: "issue", Status: "active", Sessions: []string{"s1"}}
	entries := []changelog.Entry{
		{Timestamp: "2026-05-19T10:00:00-06:00", Op: "decision", SessionID: "s1",
			Kind: changelog.KindDecision, Source: changelog.SourceExtracted, Confidence: 0.85,
			Reason: "agent-scoped resolution"},
		{Timestamp: "2026-05-19T10:01:00-06:00", Op: sessionFinalizedOp, SessionID: "s1", Kind: changelog.KindTransition},
	}
	out := renderResume(item, entries, "s1", "", "x", tapeAudit{verified: true, message: "ok"}, remoteState{})
	if !strings.Contains(out, "[extracted, confidence 0.85] agent-scoped resolution") {
		t.Errorf("high-confidence extracted must render inline as a fact:\n%s", out)
	}
	if strings.Contains(out, "## Confirm before acting") {
		t.Errorf("no below-threshold entries ⇒ no confirm block should appear:\n%s", out)
	}
}

// TestResumeRendersRemoteStateSection: an OPEN PR in remoteState surfaces a
// visible warning section between State and Next so a cold session can't miss
// that a parallel agent has already pushed this work (I-876).
func TestResumeRendersRemoteStateSection(t *testing.T) {
	item := &model.Item{ID: "I-876", Title: "x", Type: "issue", Status: "active"}
	rs := remoteState{
		prState: "OPEN",
		prURLs:  []string{"https://github.com/org/repo/pull/42"},
	}
	out := renderResume(item, nil, "", "", "x", tapeAudit{verified: true, message: "ok"}, rs)

	if !strings.Contains(out, "## Remote state") {
		t.Errorf("open PR must render a Remote state section:\n%s", out)
	}
	if !strings.Contains(out, "OPEN PR exists") {
		t.Errorf("open PR warning text missing:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com/org/repo/pull/42") {
		t.Errorf("PR URL missing from remote state section:\n%s", out)
	}
	// Section must appear after State and before Plan.
	si := strings.Index(out, "## State")
	ri := strings.Index(out, "## Remote state")
	pi := strings.Index(out, "## ⚠️  PLAN")
	if si < 0 || ri < 0 {
		t.Fatalf("State or Remote state section missing:\n%s", out)
	}
	if ri <= si {
		t.Errorf("Remote state must come after State: state=%d remote=%d", si, ri)
	}
	if pi > 0 && ri >= pi {
		t.Errorf("Remote state must come before Plan warning: remote=%d plan=%d", ri, pi)
	}
}

// TestResumeNoRemoteStateSectionWhenNoPR: when no PR is found, the Remote state
// section must NOT appear (no noise for clean items).
func TestResumeNoRemoteStateSectionWhenNoPR(t *testing.T) {
	item := &model.Item{ID: "I-876", Title: "x", Type: "issue", Status: "active"}
	out := renderResume(item, nil, "", "", "x", tapeAudit{verified: true, message: "ok"}, remoteState{})
	if strings.Contains(out, "## Remote state") {
		t.Errorf("no PR ⇒ Remote state section must not appear:\n%s", out)
	}
}
