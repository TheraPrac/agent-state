package command

import (
	"fmt"

	"github.com/theraprac/agent-state/internal/model"
)

// I-440: shared per-item row formatter for st status + st run status.
// Both surfaces emit the same base columns (id, priority, status, title)
// in identical widths and color encoding so the operator never has to
// re-learn a different layout when switching between them. Surface-
// specific extras (blocks/blocked-by lines on st status, pipeline
// progress + metrics columns on st run status) get appended by the
// caller AFTER the base row.
//
// Adding a base column = changing one place.

// ItemRowOpts controls per-row rendering choices that can't be derived
// from the item alone. Each field has a sensible zero-value so callers
// only set what they need.
type ItemRowOpts struct {
	// TitleWidth pads/truncates the title column. 45 matches the legacy
	// printIssues / printQueuedTasks width on st status; 50 reads better
	// in run-status sprint groupings where rows have no trailing metrics
	// column on the title line.
	TitleWidth int

	// Blocked turns the ID red instead of green to signal an unmet
	// dependency. Caller already knows from the dep graph; the row
	// formatter just paints.
	Blocked bool

	// PlanApproved appends a small plan badge after the title.
	PlanApproved bool

	// TrailingNewline emits a "\n" at the end of the returned string.
	// Most callers want this; the few that compose multi-row blocks
	// disable it and add their own.
	TrailingNewline bool
}

// FormatItemRow returns one rendered row for `item` ready to write to
// stdout. ANSI color codes are baked in. The base columns are, in
// order, fixed-width:
//
//	{ID:8}  {[pN]:4}  {[status]:10}  {title:TitleWidth}  {touched:10}{planBadge}
//
// Where [pN] / [status] are produced by the existing priorityLabel /
// statusLabel helpers (the unified vocabulary from I-406 + I-433).
func FormatItemRow(item *model.Item, opts ItemRowOpts) string {
	if opts.TitleWidth == 0 {
		opts.TitleWidth = 45
	}

	idColor := cGreen
	if opts.Blocked {
		idColor = cRed
	}

	touched := ""
	if !item.LastTouched.IsZero() {
		touched = item.LastTouched.Format("2006-01-02")
	}

	planBadge := ""
	if opts.PlanApproved {
		planBadge = fmt.Sprintf("  %s󰙅%s", cGreen, cReset)
	}

	// statusLabel embeds [status] tag colorized and bracketed; pad to a
	// fixed column width AFTER the label so the ANSI bytes don't throw
	// off width math. statusColumnWidth is the visible column width
	// (10 chars) — long enough for "[abandoned]" (11) only barely; we
	// use 11 so the longest legitimate status doesn't push subsequent
	// columns out of alignment.
	const statusColumnWidth = 11
	statusTag := statusLabel(item.Status)
	statusVisible := visibleWidth(item.Status) + 2 // brackets
	statusPad := statusColumnWidth - statusVisible
	if statusPad < 0 {
		statusPad = 0
	}

	out := fmt.Sprintf("%s%-8s%s  %s  %s%*s  %s  %s%s%s%s",
		idColor, item.ID, cReset,
		priorityLabel(item.Priority),
		statusTag, statusPad, "",
		padRight(truncate(item.Title, opts.TitleWidth), opts.TitleWidth),
		cDim, touched, cReset,
		planBadge,
	)
	if opts.TrailingNewline {
		out += "\n"
	}
	return out
}

// visibleWidth returns the on-screen column count of a status string
// (used to pad statusLabel output to a fixed visible width without
// counting ANSI escape bytes). Status strings are plain ASCII in the
// unified vocabulary, so len() suffices — the helper exists so a
// future status name with multi-byte runes routes through a shared
// width calc.
func visibleWidth(s string) int {
	return len([]rune(s))
}
