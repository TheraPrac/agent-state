package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// validPhases is the canonical set of phase names. Enforced at PhaseStart so
// typos surface immediately rather than creating orphan by_phase entries.
var validPhases = map[string]bool{
	"plan":   true,
	"code":   true,
	"test":   true,
	"pr-fix": true,
}

// PhaseStart marks the beginning of a named phase on the given item. It sets
// time_tracking.active_phase and seeds a by_phase entry with started_at.
// Returns 1 on error, 0 on success.
func PhaseStart(s *store.Store, cfg *config.Config, id, phase string) int {
	if !validPhases[phase] {
		fmt.Fprintf(os.Stderr, "phase start: unknown phase %q (valid: plan, code, test, pr-fix)\n", phase)
		return 1
	}
	_, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	now := time.Now().Format(time.RFC3339)
	if err := s.Mutate(id, func(item *model.Item) error {
		// Auto-close any in-progress phase before starting the new one so its
		// ended_at is stamped rather than orphaned.
		if prior := activePhase(item); prior != "" && prior != phase {
			existing := readByPhase(item, prior)
			if existing.Phase == "" {
				existing.Phase = prior
			}
			existing.EndedAt = now
			line := formatByPhaseLine(existing)
			if !updateListLine(item, "time_tracking", "by_phase",
				func(raw string) bool { return byPhaseLineMatches(raw, prior) },
				line) {
				item.Doc.AppendToNestedList("time_tracking", "by_phase", line)
			}
		}
		item.SetNested("time_tracking", "active_phase", phase)
		seedByPhase(item, phase, now)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "phase start: %v\n", err)
		return 1
	}
	fmt.Printf("Phase %q started on %s\n", phase, id)
	if err := autoSync(s, fmt.Sprintf("as phase start: %s %s", id, phase)); err != nil {
		return 1
	}
	return 0
}

// PhaseDone clears the active_phase on the given item and stamps the ended_at
// of the current phase's by_phase entry. Returns 1 on error, 0 on success.
func PhaseDone(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	phase := activePhase(item)
	if phase == "" {
		fmt.Fprintf(os.Stderr, "phase done: no active phase on %s\n", id)
		return 1
	}
	now := time.Now().Format(time.RFC3339)
	if err := s.Mutate(id, func(item *model.Item) error {
		// Re-read active_phase from the fresh locked item to avoid a TOCTOU
		// race: the outer s.Get snapshot may be stale if a concurrent PhaseStart
		// ran between s.Get and Mutate acquiring the file lock.
		currentPhase := activePhase(item)
		if currentPhase == "" {
			return fmt.Errorf("no active phase on %s", id)
		}
		item.SetNested("time_tracking", "active_phase", "")
		// Stamp ended_at on the by_phase entry without crediting a new turn.
		existing := readByPhase(item, currentPhase)
		if existing.Phase == "" {
			existing.Phase = currentPhase
		}
		existing.EndedAt = now
		line := formatByPhaseLine(existing)
		if !updateListLine(item, "time_tracking", "by_phase",
			func(raw string) bool { return byPhaseLineMatches(raw, currentPhase) },
			line) {
			item.Doc.AppendToNestedList("time_tracking", "by_phase", line)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "phase done: %v\n", err)
		return 1
	}
	fmt.Printf("Phase %q done on %s\n", phase, id)
	if err := autoSync(s, fmt.Sprintf("as phase done: %s %s", id, phase)); err != nil {
		return 1
	}
	return 0
}

// PhaseStatus prints the current active phase for the given item.
func PhaseStatus(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	phase := activePhase(item)
	if phase == "" {
		fmt.Printf("%s: no active phase\n", id)
	} else {
		fmt.Printf("%s: active_phase=%s\n", id, phase)
	}
	return 0
}

// activePhase reads time_tracking.active_phase from the item's parsed doc.
func activePhase(item *model.Item) string {
	if item == nil || item.Doc == nil {
		return ""
	}
	v, _ := item.Doc.GetNestedField("time_tracking.active_phase")
	return v
}
