package store

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// SessionLive reports whether the given session id corresponds to a
// live process. Implementations typically check the agent registry
// first (canonical PID liveness) and fall back to a session-manager
// TTL when the registry has no entry. Split out as a function type so
// internal/store stays decoupled from internal/agent and internal/session.
type SessionLive func(sessionID string) bool

// SweepStaleClaims releases items whose claimed_by names a session that
// is no longer running. The caller supplies the SessionLive probe so
// this package can stay decoupled from internal/agent.
//
// Mechanism per item with non-empty claimed_by:
//  1. sessionLive(claimed_by). If true, leave the claim alone.
//  2. If false, release via Mutate. The Mutate re-reads claimed_by
//     under the lock, so a sweep racing with a fresh claim simply
//     observes the new owner and skips.
//
// Returns the ids whose claims were released and the first error
// surfaced (subsequent errors are logged to stderr — one bad item
// shouldn't stop the sweep). T-310.
func SweepStaleClaims(s *Store, cfg *config.Config, sessionLive SessionLive) ([]string, error) {
	if s == nil {
		return nil, errors.New("SweepStaleClaims: nil store")
	}
	if sessionLive == nil {
		return nil, errors.New("SweepStaleClaims: sessionLive probe required")
	}

	var released []string
	var firstErr error
	for _, item := range s.All() {
		if item.ClaimedBy == "" {
			continue
		}
		if sessionLive(item.ClaimedBy) {
			continue
		}
		err := s.Mutate(item.ID, func(it *model.Item) error {
			// Re-check under the lock. A live process may have refreshed
			// or a fresh claim may have landed since we surveyed.
			if it.ClaimedBy == "" {
				return nil
			}
			if sessionLive(it.ClaimedBy) {
				return nil // someone took ownership; respect it
			}
			it.ClaimedBy = ""
			it.ClaimedAt = ""
			it.Doc.SetField("claimed_by", "")
			it.Doc.SetField("claimed_at", "")
			it.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "SweepStaleClaims: %s: %v\n", item.ID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		released = append(released, item.ID)
	}
	return released, firstErr
}
