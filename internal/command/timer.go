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
		// Cap implausibly long sessions. A real Claude Code session rarely
		// exceeds a few hours; anything beyond 12 h almost certainly means
		// session_started_at was never cleared (write race, crash, or stale
		// epoch from a previous deployment). Capping at 12 h bounds the
		// damage while preserving legitimate long sessions.
		const maxSessionSecs = 12 * 3600
		if elapsed > maxSessionSecs {
			fmt.Fprintf(os.Stderr,
				"timer pause: %s: session_started_at %s is %dh ago — capping at 12h (stale epoch?)\n",
				id, sessStart, elapsed/3600)
			elapsed = maxSessionSecs
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

// TimerScrubRatio flags (and optionally nulls) items whose
// accumulated_seconds appears to be wall-clock contamination of the NEW
// shape: both work_duration_seconds and accumulated_seconds are present
// (so TimerScrub's original discriminator passes them), but the timer
// ran for an implausibly large fraction of the item's total wall-clock
// span — characteristic of session_started_at being left set across idle
// days (e.g., by a write race that restored a stale epoch).
//
// Criteria (all must hold):
//   - wall_time_hours > 24 (item spanned multiple days)
//   - accumulated_seconds > 4 h  (enough to matter)
//   - accumulated_seconds / (wall_time_hours × 3600) > 0.5
//     (timer ran more than half the calendar window)
//
// When dryRun is false and autoNull is true, the field is removed (same
// treatment as TimerScrub). When autoNull is false, items are only
// reported to stderr as warnings. Returns count flagged; err is non-nil
// if any autoNull mutation fails.
func TimerScrubRatio(s *store.Store, dryRun, autoNull bool) (int, error) {
	flagged, failed := 0, 0
	for id, item := range s.All() {
		accStr, ok := getNestedField(item, "time_tracking", "accumulated_seconds")
		if !ok || accStr == "" {
			continue
		}
		wallStr, ok2 := getNestedField(item, "time_tracking", "wall_time_hours")
		if !ok2 || wallStr == "" {
			continue
		}
		var accSecs int
		var wallH float64
		if _, err := fmt.Sscanf(accStr, "%d", &accSecs); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(wallStr, "%f", &wallH); err != nil {
			continue
		}
		if wallH <= 24 || accSecs <= 4*3600 {
			continue
		}
		ratio := float64(accSecs) / (wallH * 3600)
		if ratio <= 0.5 {
			continue
		}
		fmt.Fprintf(os.Stderr,
			"timer scrub (ratio): %s: accumulated=%ds (%.1fh) is %.0f%% of wall_time=%.1fh — likely stale-epoch contamination\n",
			id, accSecs, float64(accSecs)/3600, ratio*100, wallH)
		flagged++
		if dryRun || !autoNull {
			continue
		}
		removed := false
		mutErr := s.Mutate(id, func(it *model.Item) error {
			wd, _ := getNestedField(it, "time_tracking", "work_duration_seconds")
			if wd != "" {
				it.Doc.RemoveNestedField("time_tracking.work_duration_seconds")
				if it.TimeTracking != nil {
					delete(it.TimeTracking, "work_duration_seconds")
				}
			}
			it.Doc.RemoveNestedField("time_tracking.accumulated_seconds")
			if it.TimeTracking != nil {
				delete(it.TimeTracking, "accumulated_seconds")
			}
			removed = true
			return nil
		})
		if mutErr != nil {
			fmt.Fprintf(os.Stderr, "timer scrub (ratio): %s: %v\n", id, mutErr)
			failed++
			continue
		}
		if removed {
			fmt.Printf("  %s: nulled accumulated_seconds + work_duration_seconds\n", id)
		}
	}
	if failed > 0 {
		return flagged, fmt.Errorf("timer scrub (ratio): %d item(s) failed to mutate (%d flagged)", failed, flagged)
	}
	return flagged, nil
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
