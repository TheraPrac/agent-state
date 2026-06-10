package command

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TimerPauseAll flushes the elapsed session time for every active item owned
// by agentID into time_tracking.accumulated_seconds and clears
// session_started_at. Items with no live session_started_at are skipped (timer
// is already paused). Returns count of items modified.
func TimerPauseAll(s *store.Store, cfg *config.Config, agentID string) (int, error) {
	if agentID == "" {
		agentID = cfg.AgentID()
	}
	now := time.Now()
	paused := 0
	for id, item := range s.All() {
		if !isActiveForAgent(item, cfg, agentID) {
			continue
		}
		sessStart, ok := getNestedField(item, "time_tracking", "session_started_at")
		if !ok || sessStart == "" {
			continue
		}
		t0, err := time.Parse(time.RFC3339, sessStart)
		if err != nil {
			fmt.Fprintf(os.Stderr, "timer pause: %s: bad session_started_at %q: %v\n", id, sessStart, err)
			continue
		}
		elapsed := int(now.Sub(t0).Seconds())
		if elapsed < 0 {
			elapsed = 0 // clock-skew guard
		}
		capturedElapsed := elapsed
		mutErr := s.Mutate(id, func(it *model.Item) error {
			// Re-read session_started_at under the lock: a concurrent st timer pause
			// may have already cleared it. If so, skip to avoid double-counting.
			if ss, _ := getNestedField(it, "time_tracking", "session_started_at"); ss == "" {
				return nil
			}
			// Re-read accumulated_seconds from the freshly parsed item to avoid
			// overwriting a value written by a concurrent process between s.All()
			// and this Mutate lock acquisition.
			acc := 0
			if v, ok2 := getNestedField(it, "time_tracking", "accumulated_seconds"); ok2 && v != "" {
				fmt.Sscanf(v, "%d", &acc) //nolint:errcheck
			}
			it.SetNested("time_tracking", "accumulated_seconds", strconv.Itoa(acc+capturedElapsed))
			it.SetNested("time_tracking", "session_started_at", "")
			return nil
		})
		if mutErr != nil {
			fmt.Fprintf(os.Stderr, "timer pause: %s: %v\n", id, mutErr)
			continue
		}
		paused++
	}
	return paused, nil
}

// TimerResumeAll writes session_started_at = now for every active item owned
// by agentID that does not already have a session_started_at set. Idempotent:
// items with an existing session_started_at are left untouched (no double-count).
// Returns count of items modified.
func TimerResumeAll(s *store.Store, cfg *config.Config, agentID string) (int, error) {
	if agentID == "" {
		agentID = cfg.AgentID()
	}
	now := time.Now().Format(time.RFC3339)
	resumed := 0
	for id, item := range s.All() {
		if !isActiveForAgent(item, cfg, agentID) {
			continue
		}
		sessStart, _ := getNestedField(item, "time_tracking", "session_started_at")
		if sessStart != "" {
			continue // already running — leave untouched
		}
		mutErr := s.Mutate(id, func(it *model.Item) error {
			it.SetNested("time_tracking", "session_started_at", now)
			return nil
		})
		if mutErr != nil {
			fmt.Fprintf(os.Stderr, "timer resume: %s: %v\n", id, mutErr)
			continue
		}
		resumed++
	}
	return resumed, nil
}

// TimerScrub removes wall-clock-contaminated work_duration_seconds values
// (I-1335). Before the fix, st close substituted the started_at wall-clock
// span when no session timer data existed, storing it indistinguishably from
// measured values. Discriminator: closes made after I-1335 always re-persist
// the measured total into accumulated_seconds, a fallback close never wrote
// it — so any item with work_duration_seconds set but accumulated_seconds
// absent/empty holds a wall-clock span, and the field is removed (null =
// unknown). Known conservative edge: items closed in the I-1318→I-1335
// window with a running-but-never-paused timer also lack accumulated_seconds,
// so their measured values are nulled too; in-file they are byte-identical to
// fallbacks (old close wrote work==total on every path), and null/unknown is
// the safe direction. Returns count of items scrubbed; err is non-nil when
// any item failed to mutate (partial scrubs must not exit 0).
func TimerScrub(s *store.Store, cfg *config.Config, dryRun bool) (int, error) {
	scrubbed, failed := 0, 0
	for id, item := range s.All() {
		workDur, ok := getNestedField(item, "time_tracking", "work_duration_seconds")
		if !ok || workDur == "" {
			continue
		}
		if acc, ok := getNestedField(item, "time_tracking", "accumulated_seconds"); ok && acc != "" {
			continue // measured — keep
		}
		if dryRun {
			fmt.Printf("  %s: would remove work_duration_seconds=%s\n", id, workDur)
			scrubbed++
			continue
		}
		removed := false
		mutErr := s.Mutate(id, func(it *model.Item) error {
			// Re-read both fields under the lock (same pattern as
			// TimerPauseAll): a concurrent st close may have written a
			// measured value between the s.All() snapshot and this lock.
			wd, ok := getNestedField(it, "time_tracking", "work_duration_seconds")
			if !ok || wd == "" {
				return nil
			}
			if acc, ok := getNestedField(it, "time_tracking", "accumulated_seconds"); ok && acc != "" {
				return nil // now measured — keep
			}
			it.Doc.RemoveNestedField("time_tracking.work_duration_seconds")
			if it.TimeTracking != nil {
				delete(it.TimeTracking, "work_duration_seconds")
			}
			removed = true
			return nil
		})
		if mutErr != nil {
			fmt.Fprintf(os.Stderr, "timer scrub: %s: %v\n", id, mutErr)
			failed++
			continue
		}
		if removed {
			fmt.Printf("  %s: removed work_duration_seconds=%s\n", id, workDur)
			scrubbed++
		}
	}
	if failed > 0 {
		return scrubbed, fmt.Errorf("timer scrub: %d item(s) failed to mutate (%d scrubbed)", failed, scrubbed)
	}
	return scrubbed, nil
}

// isActiveForAgent returns true when item is in its type's active status and
// is assigned to agentID.
func isActiveForAgent(item *model.Item, cfg *config.Config, agentID string) bool {
	if item.AssignedTo != agentID {
		return false
	}
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	return item.Status == tc.ActiveStatus
}
