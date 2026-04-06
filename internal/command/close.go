package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

// CloseOpts holds flags for the close command.
type CloseOpts struct {
	Reason string
	Force  bool // bypass gate enforcement
}

func Close(s *store.Store, cfg *config.Config, id, resolution string, opts CloseOpts) int {
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

	// Resolution must be a valid terminal status
	validTerminal := false
	for _, ts := range tc.TerminalStatuses {
		if resolution == ts {
			validTerminal = true
			break
		}
	}
	if !validTerminal {
		fmt.Fprintf(os.Stderr, "invalid resolution %q — valid: %v\n", resolution, tc.TerminalStatuses)
		return 2
	}

	// Must be in active status (or start status for abandoned)
	if item.Status != tc.ActiveStatus && item.Status != tc.StartStatus {
		fmt.Fprintf(os.Stderr, "%s is %s — cannot close\n", id, item.Status)
		return 1
	}

	// If abandoning, require reason
	if resolution == "abandoned" || resolution == "wontfix" || resolution == "declined" {
		if opts.Reason == "" {
			fmt.Fprintln(os.Stderr, "--reason is required when abandoning")
			return 2
		}
	}

	// Gate enforcement (skip for abandon/wontfix — those bypass gates by design)
	if !opts.Force && resolution != "abandoned" && resolution != "wontfix" && resolution != "declined" {
		results := validate.EvaluateGates(item, "close", cfg, s.All())
		if !validate.GatesPassed(results) {
			failure := validate.FirstFailure(results)
			fmt.Fprintf(os.Stderr, "gate %q failed: %s\n", failure.Gate, failure.Message)
			fmt.Fprintln(os.Stderr, "use --force to bypass gates")
			return 1
		}
	}

	// Transition
	oldStatus := item.Status
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	item.Doc.SetField("status", resolution)
	item.Status = resolution
	item.Doc.SetField("completed", nowStr)
	item.Doc.SetField("last_touched", nowStr)

	// Record completion time tracking
	setNestedField(item, "time_tracking", "completed_at", nowStr)
	if startedAt, ok := getNestedField(item, "time_tracking", "started_at"); ok && startedAt != "" {
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			wallDur := now.Sub(t)
			setNestedField(item, "time_tracking", "wall_time_hours", fmt.Sprintf("%.1f", wallDur.Hours()))
			setNestedField(item, "time_tracking", "total_wall_time", formatDuration(wallDur))
		}
	}

	// Total AI time from st run metrics (ai_duration_seconds)
	if aiSec, ok := getNestedField(item, "time_tracking", "ai_duration_seconds"); ok && aiSec != "" {
		var secs int
		fmt.Sscanf(aiSec, "%d", &secs)
		if secs > 0 {
			setNestedField(item, "time_tracking", "total_ai_time", formatDuration(time.Duration(secs)*time.Second))
		}
	}

	// AI cost summary
	if aiCost, ok := getNestedField(item, "time_tracking", "ai_cost_usd"); ok && aiCost != "" {
		setNestedField(item, "time_tracking", "total_ai_cost_usd", aiCost)
	}

	// Token totals
	if v, ok := getNestedField(item, "time_tracking", "input_tokens"); ok && v != "" {
		setNestedField(item, "time_tracking", "total_input_tokens", v)
	}
	if v, ok := getNestedField(item, "time_tracking", "output_tokens"); ok && v != "" {
		setNestedField(item, "time_tracking", "total_output_tokens", v)
	}
	if v, ok := getNestedField(item, "time_tracking", "total_tokens"); ok && v != "" {
		setNestedField(item, "time_tracking", "total_tokens_final", v)
	}

	if opts.Reason != "" {
		item.Doc.SetField("resolution", opts.Reason)
	}

	// Clear session claim
	if item.ClaimedBy != "" {
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		_ = mgr.RemoveClaim(item.ClaimedBy, id)
		item.ClaimedBy = ""
		item.ClaimedAt = ""
		item.Doc.SetField("claimed_by", "")
		item.Doc.SetField("claimed_at", "")
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	// Move to correct directory
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "close", Field: "status",
		OldValue: oldStatus, NewValue: resolution,
		Reason: opts.Reason,
	})

	fmt.Printf("Closed %s — %s (%s)\n", id, item.Title, resolution)

	// Auto-pop stack if this item is on top
	stack := LoadStack(cfg)
	if len(stack) > 0 && stack[len(stack)-1].ID == id {
		stack = stack[:len(stack)-1]
		// Skip any resolved items below
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if topItem, ok := s.Get(top.ID); ok && cfg.IsTerminalStatus(topItem.Type, topItem.Status) {
				fmt.Printf("  %s also resolved — skipping\n", top.ID)
				stack = stack[:len(stack)-1]
				continue
			}
			break
		}
		SaveStack(cfg, stack)
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			if topItem, ok := s.Get(top.ID); ok {
				fmt.Printf("Returning to %s — %s\n", top.ID, topItem.Title)
			}
		} else {
			fmt.Println("Stack is now empty")
		}
	}

	// Commit + push the close to git immediately. Previously the move to
	// archive/ and status change sat uncommitted until the caller happened
	// to run `st sync` or until `st run`'s deferred sync caught it. That
	// gap allowed silent-revert incidents (e.g. I-164): a subsequent st
	// command's PersistentPreRunE → GitPull destroyed the uncommitted
	// move, and "Closed" turned out to be a lie. GitSync is best-effort —
	// a failure here only warns, because the filesystem mutation already
	// succeeded and a later sync will carry the commit forward.
	if err := s.GitSync(fmt.Sprintf("st close: %s (%s)", id, resolution)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after close failed: %v\n", err)
	}

	// Release item lock AFTER GitSync so that sibling items in the same
	// st run batch see the committed terminal status when they reload the
	// store and recheck dependencies.
	store.UnlockItem(cfg, id)

	// Auto-archive sprint and epic when all items are terminal.
	autoArchiveSprintAndEpic(s, cfg, item.Sprint)

	return 0
}

// autoArchiveSprintAndEpic checks if all items in the sprint are terminal.
// If so, archives the sprint. Then checks if all sprints in the epic are
// archived, and if so, archives the epic. This runs after st close so that
// completed sprints/epics are automatically cleaned up without manual
// st sprint archive / st epic archive commands.
func autoArchiveSprintAndEpic(s *store.Store, cfg *config.Config, sprintID string) {
	if sprintID == "" {
		return
	}

	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return
	}

	sp, err := reg.SprintByID(sprintID)
	if err != nil || sp.Status != "active" {
		return
	}

	// Check if all items in the sprint are terminal.
	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		if !cfg.IsTerminalStatus(item.Type, item.Status) {
			return // at least one item still active — don't archive
		}
	}

	// All items terminal — archive the sprint.
	sp.Status = "archived"
	fmt.Printf("[auto-archive] All items in sprint %q complete — archived\n", sp.Title)

	// Check if all sprints in the parent epic are now archived.
	epicID := sp.Epic
	if epicID != "" {
		allDone := true
		for _, es := range reg.Sprints {
			if es.Epic == epicID && es.Status != "archived" {
				allDone = false
				break
			}
		}
		if allDone {
			for i := range reg.Epics {
				if reg.Epics[i].ID == epicID {
					reg.Epics[i].Status = "archived"
					fmt.Printf("[auto-archive] All sprints in epic %q complete — archived\n", reg.Epics[i].Title)
					break
				}
			}
		}
	}

	if err := reg.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-archive save failed: %v\n", err)
	}
}
