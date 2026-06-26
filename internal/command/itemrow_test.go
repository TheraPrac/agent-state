package command

import (
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/model"
)

func ptrInt(v int) *int { return &v }

// fixedTouched is a stable timestamp so tests assert on a known
// "2026-04-27" date string rather than a moving target.
var fixedTouched = time.Date(2026, 4, 27, 14, 0, 0, 0, time.UTC)

// TestFormatItemRow_BaseStructure pins the base columns: ID block,
// priority tag, status tag, title (fixed-width), touched date.
// Future column additions break this test loudly — that is the point.
func TestFormatItemRow_BaseStructure(t *testing.T) {
	item := &model.Item{
		ID:          "I-440",
		Type:        "issue",
		Status:      "active",
		Title:       "shared row formatter",
		Priority:    ptrInt(1),
		LastTouched: fixedTouched,
	}
	out := FormatItemRow(item, ItemRowOpts{TitleWidth: 45})

	for _, want := range []string{
		"I-440",                // ID column
		"[p1]",                 // priorityLabel
		"[active]",             // statusLabel
		"shared row formatter", // title
		"2026-04-27",           // touched
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q. got:\n  %q", want, out)
		}
	}
}

// TestFormatItemRow_BlockedTurnsIDRed verifies that opts.Blocked swaps
// the green ID color for red. Caller (statusDashboard / printIssues)
// already computes blockedness from the dep graph; the formatter only
// paints.
func TestFormatItemRow_BlockedTurnsIDRed(t *testing.T) {
	item := &model.Item{ID: "I-1", Type: "issue", Status: "queued",
		Title: "x", Priority: ptrInt(2), LastTouched: fixedTouched}

	clean := FormatItemRow(item, ItemRowOpts{Blocked: false})
	blocked := FormatItemRow(item, ItemRowOpts{Blocked: true})

	if !strings.Contains(clean, cGreen) {
		t.Errorf("non-blocked row should contain green ANSI; got: %q", clean)
	}
	if !strings.Contains(blocked, cRed) {
		t.Errorf("blocked row should contain red ANSI; got: %q", blocked)
	}
}

// TestFormatItemRow_NilPriorityRenders verifies items with nil priority
// still render a fixed-width tag — otherwise the column shifts and the
// I-440 parity contract breaks.
func TestFormatItemRow_NilPriorityRenders(t *testing.T) {
	item := &model.Item{ID: "I-1", Type: "issue", Status: "queued",
		Title: "x", Priority: nil, LastTouched: fixedTouched}
	out := FormatItemRow(item, ItemRowOpts{})
	if !strings.Contains(out, "[p?]") {
		t.Errorf("nil priority should render [p?]; got: %q", out)
	}
}

// TestFormatItemRow_PlanBadgeOnlyWhenApproved verifies the plan badge
// is gated by opts.PlanApproved (caller passes through item.PlanApproved).
func TestFormatItemRow_PlanBadgeOnlyWhenApproved(t *testing.T) {
	item := &model.Item{ID: "I-1", Type: "issue", Status: "queued",
		Title: "x", Priority: ptrInt(2), LastTouched: fixedTouched}

	noBadge := FormatItemRow(item, ItemRowOpts{PlanApproved: false})
	withBadge := FormatItemRow(item, ItemRowOpts{PlanApproved: true})

	if strings.Contains(noBadge, "󰙅") {
		t.Errorf("badge should be absent when PlanApproved=false; got: %q", noBadge)
	}
	if !strings.Contains(withBadge, "󰙅") {
		t.Errorf("badge should be present when PlanApproved=true; got: %q", withBadge)
	}
}

// TestFormatItemRow_TitleTruncation verifies titles longer than
// TitleWidth get truncated with "..." rather than overflowing the
// column. Pin the exact suffix so future width changes are a known
// test diff.
func TestFormatItemRow_TitleTruncation(t *testing.T) {
	long := strings.Repeat("abcdefghij", 10) // 100 chars
	item := &model.Item{ID: "I-1", Type: "issue", Status: "queued",
		Title: long, Priority: ptrInt(2), LastTouched: fixedTouched}
	out := FormatItemRow(item, ItemRowOpts{TitleWidth: 45})

	if !strings.Contains(out, "...") {
		t.Errorf("over-width title should show ... ellipsis; got: %q", out)
	}
	if strings.Contains(out, long) {
		t.Errorf("untruncated title should not appear in output; got: %q", out)
	}
}

// TestFormatItemRow_FixedWidthAcrossPriorities is the heart of the
// I-440 parity contract: the column layout is byte-identical between
// items with different priorities, so the eye can scan a list without
// columns jittering. Strip ANSI escapes and compare visible byte
// counts up to the title.
func TestFormatItemRow_FixedWidthAcrossPriorities(t *testing.T) {
	mk := func(p *int) string {
		item := &model.Item{
			ID: "I-1", Type: "issue", Status: "active",
			Title: "x", Priority: p, LastTouched: fixedTouched,
		}
		return FormatItemRow(item, ItemRowOpts{TitleWidth: 45})
	}

	// All five priorities + nil should produce the same visible width
	// up to (and including) the title column. ANSI bytes are stripped
	// so color-code length differences (e.g. cOrange = 10 bytes vs cRed
	// = 5) don't pollute the comparison.
	rows := []string{
		mk(ptrInt(0)), mk(ptrInt(1)), mk(ptrInt(2)),
		mk(ptrInt(3)), mk(ptrInt(4)), mk(nil),
	}

	first := stripANSI(rows[0])
	for i, r := range rows[1:] {
		got := stripANSI(r)
		if len(got) != len(first) {
			t.Errorf("row %d visible width %d != row 0 width %d.\n  row 0: %q\n  row %d: %q",
				i+1, len(got), len(first), first, i+1, got)
		}
	}
}

// TestFormatItemRow_BothSurfacesEmitIdenticalBase is the I-440 parity
// contract test: status and run-status MUST share the FormatItemRow
// call. Different TitleWidth between the two surfaces is allowed (45
// for status's tag-grouped lists, 60 for run-status where the data
// row carries no extra title content), but everything BEFORE the
// title column must be byte-identical: ID block, priority tag, status
// tag.
//
// The check is column-prefix-based: drop the title + everything after
// and compare. If a future refactor splits the formatters, this test
// fails — that's the point.
func TestFormatItemRow_BothSurfacesEmitIdenticalBase(t *testing.T) {
	item := &model.Item{
		ID:          "I-440",
		Type:        "issue",
		Status:      "active",
		Title:       "shared row formatter",
		Priority:    ptrInt(1),
		LastTouched: fixedTouched,
	}

	statusRow := FormatItemRow(item, ItemRowOpts{TitleWidth: 45})
	runRow := FormatItemRow(item, ItemRowOpts{TitleWidth: 60})

	// Slice off everything from the title forward — the title column
	// is the first place the two surfaces legitimately diverge (width).
	titleStart := strings.Index(statusRow, item.Title)
	if titleStart < 0 {
		t.Fatalf("title %q not found in statusRow %q", item.Title, statusRow)
	}
	runTitleStart := strings.Index(runRow, item.Title)
	if runTitleStart < 0 {
		t.Fatalf("title %q not found in runRow %q", item.Title, runRow)
	}

	statusBase := statusRow[:titleStart]
	runBase := runRow[:runTitleStart]
	if statusBase != runBase {
		t.Errorf("base prefix differs between surfaces.\n  status: %q\n  run:    %q",
			statusBase, runBase)
	}
}

// TestFormatItemRow_ListSurfaceMatchesBase closes the parity loop over
// the third surface (I-444). `st list` now goes through FormatItemRow,
// so the base prefix (ID block, priority tag, status tag) must remain
// byte-identical to st status's call with the same item. Width of the
// title column is allowed to diverge; everything before the title is
// the shared contract.
func TestFormatItemRow_ListSurfaceMatchesBase(t *testing.T) {
	item := &model.Item{
		ID:          "I-444",
		Type:        "issue",
		Status:      "queued",
		Title:       "list surface row formatter",
		Priority:    ptrInt(3),
		LastTouched: fixedTouched,
	}

	listRow := FormatItemRow(item, ItemRowOpts{TitleWidth: 45})
	statusRow := FormatItemRow(item, ItemRowOpts{TitleWidth: 45})

	listTitle := strings.Index(listRow, item.Title)
	statusTitle := strings.Index(statusRow, item.Title)
	if listTitle < 0 || statusTitle < 0 {
		t.Fatalf("title %q not found in list=%q status=%q", item.Title, listRow, statusRow)
	}
	if listRow[:listTitle] != statusRow[:statusTitle] {
		t.Errorf("base prefix differs between list and status surfaces.\n  list:   %q\n  status: %q",
			listRow[:listTitle], statusRow[:statusTitle])
	}
}
