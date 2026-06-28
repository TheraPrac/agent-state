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
// already-active (non-terminal) item is an error. The active status is
// resolved from the type's config (TypeConfig.ActiveStatus), never hardcoded,
// so it stays correct if the vocabulary changes. The close-time markers are
// undone symmetrically: `completed` is blanked and the `delivery.stage=closed`
// marker (set by Close) is removed. I-1599.
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

	if err := autoSync(s, fmt.Sprintf("st reopen: %s (%s)", id, reason)); err != nil {
		return 1
	}
	return 0
}
