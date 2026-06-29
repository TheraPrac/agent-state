package command

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// maxSessionSecs caps how many seconds a single session flush can contribute.
// A real Claude Code session rarely exceeds a few hours; anything beyond 12h
// almost certainly means session_started_at was never cleared (write race,
// crash, or stale epoch from a previous deployment).
const maxSessionSecs = 12 * 3600

// cappedSessionElapsedSecs returns the number of elapsed seconds between
// sessStart and now, applying three guards:
//  1. Parse failure or future sessStart (clock skew) → 0
//  2. sessStart predates itemStartedAt (stale epoch residue) → 0
//  3. Elapsed > maxSessionSecs → capped at maxSessionSecs
//
// itemStartedAt is time_tracking.started_at (when the item was first started).
// Pass "" to skip the stale-epoch guard (e.g., when started_at is unavailable).
func cappedSessionElapsedSecs(sessStart, itemStartedAt string, now time.Time) int {
	t0, err := time.Parse(time.RFC3339, sessStart)
	if err != nil {
		return 0
	}
	elapsed := int(now.Sub(t0).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	if itemStartedAt != "" {
		if t1, err2 := time.Parse(time.RFC3339, itemStartedAt); err2 == nil {
			if t0.Before(t1) {
				// session_started_at predates the item's own started_at — stale residue
				// from a prior session; refuse to count it.
				return 0
			}
		}
	}
	if elapsed > maxSessionSecs {
		elapsed = maxSessionSecs
	}
	return elapsed
}

// clampWorkToWallSpan returns workSecs clamped to [0, spanSecs]. Active work
// cannot exceed the in-session wall span (started_at…completed_at), so any
// value above it is contamination from any source. When spanSecs ≤ 0 the span
// is unknown and the value passes through unchanged.
func clampWorkToWallSpan(workSecs, spanSecs int) int {
	if spanSecs <= 0 || workSecs <= spanSecs {
		return workSecs
	}
	return spanSecs
}

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
		itemStartedAt, _ := getNestedField(item, "time_tracking", "started_at")
		capturedElapsed := cappedSessionElapsedSecs(sessStart, itemStartedAt, now)
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
// accumulated_seconds appears to be wall-clock contamination. Two independent
// criteria can trigger a flag (either is sufficient):
//
//  1. Multi-day ratio: wall_time_hours > 24, accumulated > 4h, and
//     accumulated / wall > 50%. Catches long-span items where the timer
//     ran for most of the calendar window.
//
//  2. Short-span gross factor: accumulated_seconds > total_duration_seconds+300
//     (300s margin for git-diff latency at close), regardless of wall_time_hours.
//     Catches I-891/I-1550 class items (wall=14min, work=230h) that the
//     multi-day path missed because wallH ≤ 24.
//
// When dryRun is false and autoNull is true, both work_duration_seconds and
// accumulated_seconds are removed (same treatment as TimerScrub). When autoNull
// is false, items are only reported to stderr as warnings. Returns count flagged;
// err is non-nil if any autoNull mutation fails.
func TimerScrubRatio(s *store.Store, dryRun, autoNull bool) (int, error) {
	flagged, failed := 0, 0
	for id, item := range s.All() {
		accStr, ok := getNestedField(item, "time_tracking", "accumulated_seconds")
		if !ok || accStr == "" {
			continue
		}
		var accSecs int
		if _, err := fmt.Sscanf(accStr, "%d", &accSecs); err != nil {
			continue
		}

		contaminated := false
		var reason string

		// Criterion 1: multi-day ratio path (original).
		wallStr, hasWall := getNestedField(item, "time_tracking", "wall_time_hours")
		var wallH float64
		if hasWall && wallStr != "" {
			fmt.Sscanf(wallStr, "%f", &wallH) //nolint:errcheck
		}
		if wallH > 24 && accSecs > 4*3600 {
			ratio := float64(accSecs) / (wallH * 3600)
			if ratio > 0.5 {
				contaminated = true
				reason = fmt.Sprintf("accumulated=%ds (%.1fh) is %.0f%% of wall_time=%.1fh",
					accSecs, float64(accSecs)/3600, ratio*100, wallH)
			}
		}

		// Criterion 2: short-span gross factor — work > wall span (I-891/I-1550 class).
		if !contaminated {
			if totalStr, ok2 := getNestedField(item, "time_tracking", "total_duration_seconds"); ok2 && totalStr != "" {
				var totalSecs int
				if _, err := fmt.Sscanf(totalStr, "%d", &totalSecs); err == nil && totalSecs > 0 {
					const scrubMargin = 300 // tolerate git-diff latency at close
					if accSecs > totalSecs+scrubMargin {
						contaminated = true
						reason = fmt.Sprintf("accumulated=%ds (%.1fh) exceeds total_duration=%ds (%.1fh) — impossible active > wall span",
							accSecs, float64(accSecs)/3600, totalSecs, float64(totalSecs)/3600)
					}
				}
			}
		}

		if !contaminated {
			continue
		}
		fmt.Fprintf(os.Stderr, "timer scrub (ratio): %s: %s — likely stale-epoch contamination\n", id, reason)
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
