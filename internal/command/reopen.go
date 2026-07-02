package command

import (
	"fmt"
	"os"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// Reopen is the reverse of close (close.go): it returns a terminal item to its
// type's configured active status and moves the file back from archive/ to the
// active directory. A non-empty --reason is required for auditability (Inv 8).
//
// Only a genuinely terminal item can be reopened — calling reopen on an
// already-active (non-terminal) item is an error, and only task/issue items
// are eligible (goals have their own lifecycle). The active status is resolved
// from the type's config (TypeConfig.ActiveStatus), never hardcoded, so it
// stays correct if the vocabulary changes. The close-time markers are undone
// symmetrically: `completed` is blanked, `delivery.stage=closed` is removed,
// and the remaining completion stamps are cleared via clearCloseMarkers so the
// item is not double-counted as completed. I-1599.
func Reopen(s *store.Store, cfg *config.Config, id, reason string) int {
	if reason == "" {
		fmt.Fprintln(os.Stderr, "reopen: --reason is required")
		return 2
	}

	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// I-1599: reopen is for the task/issue lifecycle only. Goals have their
	// own terminal vocabulary (met/dropped) and an active-weight budget that
	// reactivation would have to re-satisfy; un-terminal of goals is
	// explicitly out of scope, so refuse here rather than silently flip a
	// goal to active with no weight check.
	if item.Type != "task" && item.Type != "issue" {
		fmt.Fprintf(os.Stderr,
			"reopen supports task/issue items; %s items use their own lifecycle (goals: st goal activate/mark-met/drop)\n", item.Type)
		return 1
	}

	tc, ok := cfg.Types[item.Type]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", item.Type)
		return 1
	}

	if !cfg.IsTerminalStatus(item.Type, item.Status) {
		fmt.Fprintf(os.Stderr,
			"reopen: %s is %s — not terminal; nothing to reopen\n", id, item.Status)
		return 1
	}

	activeStatus := tc.ActiveStatus
	if activeStatus == "" {
		fmt.Fprintf(os.Stderr,
			"reopen: type %q has no active status configured — cannot reopen\n", item.Type)
		return 1
	}

	oldStatus := item.Status
	nowStr := time.Now().Format(time.RFC3339)

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Status = activeStatus
		it.Doc.SetField("status", activeStatus)
		// Reverse the close-time `completed: <ts>` marker.
		it.Doc.SetField("completed", "null")
		it.Doc.SetField("last_touched", nowStr)
		// Reverse the close-time `delivery.stage=closed` marker so the item
		// reads as freshly-active rather than closed (no-op if absent).
		it.Doc.RemoveNestedField("delivery.stage")
		// Clear the remaining completion stamps Close wrote so st show /
		// metrics don't double-count a reopened item as completed.
		clearCloseMarkers(it)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	// Move the file back to the active directory for its new status.
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op: "reopen", Field: "status",
		OldValue: oldStatus, NewValue: activeStatus,
		Reason: reason,
	})

	fmt.Printf("Reopened %s — %s (%s → %s)\n", id, item.Title, oldStatus, activeStatus)

	// I-1719: s.Move renames the file across status directories (archive/ ->
	// the active dir), which git sees as delete-old + untracked-new — GitSync's
	// `git add -u` only stages the deletion, never the new path (I-1715/I-442).
	// Pass the post-Move path explicitly, same as GoalCreate/GoalMarkMet/GoalDrop.
	path, _ := s.Path(id)
	if err := autoSync(s, fmt.Sprintf("st reopen: %s (%s)", id, reason), path); err != nil {
		return 1
	}
	return 0
}

// clearCloseMarkers reverses the completion stamps that Close writes
// (close.go ~250-372) so a reopened item is not double-counted as completed by
// st show / metrics. DRIFT COUPLING: this set must mirror the fields Close
// stamps on close — if Close adds a new completion marker, add its inverse
// here. time_tracking.accumulated_seconds is deliberately KEPT: it is the
// cumulative active-work total and must survive the close/reopen cycle.
// RemoveNestedField handles dotted nested paths; top-level scalar fields use
// SetField(key,"null") (there is no top-level remove on ParsedDocument).
func clearCloseMarkers(it *model.Item) {
	// Top-level completion fields → blank to null.
	it.Doc.SetField("resolution", "null")
	it.Doc.SetField("dropped_reason", "null")
	// I-1486: clear the UAT verification marker so a reopened+reworked item must
	// pass `st uat` again before it can re-close `done` (a stale pre-rework pass
	// must not silently gate the new work).
	it.Doc.RemoveNestedField("testing_evidence.uat")
	// Nested time_tracking completion totals → remove (no-op if absent).
	for _, path := range []string{
		"time_tracking.completed_at",
		"time_tracking.total_duration_seconds",
		"time_tracking.total_wall_time",
		"time_tracking.wall_time_hours",
		"time_tracking.work_duration_seconds",
		"time_tracking.total_ai_time",
		"time_tracking.total_ai_cost_usd",
		"time_tracking.total_input_tokens",
		"time_tracking.total_output_tokens",
		"time_tracking.total_tokens_final",
	} {
		it.Doc.RemoveNestedField(path)
	}
}
