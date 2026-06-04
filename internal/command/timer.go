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
