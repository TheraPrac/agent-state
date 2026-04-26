package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintRecover releases stale claims for items in a sprint.
func SprintRecover(s *store.Store, cfg *config.Config, sprintID string) int {
	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	sp, ok := reg.GetSprint(sprintID)
	if !ok {
		fmt.Fprintf(os.Stderr, "sprint not found: %s\n", sprintID)
		return 1
	}

	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)

	released := 0
	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok || item.ClaimedBy == "" {
			continue
		}

		// Check if the claiming session is stale
		claimSession, _ := mgr.Load(item.ClaimedBy)
		if claimSession == nil || mgr.IsStale(claimSession) {
			age := "unknown"
			if claimSession != nil {
				age = fmt.Sprintf("%.0f minutes ago", time.Since(claimSession.LastActive).Minutes())
			}

			fmt.Printf("  Released stale claim: %s (claimed by %s, last active %s)\n",
				itemID, item.ClaimedBy, age)

			// Read the live claimed_by under the flock so a re-claim
			// between the cached read above and the lock acquisition
			// doesn't make us release the wrong session.
			var claimedBy string
			if err := s.Mutate(itemID, func(item *model.Item) error {
				claimedBy = item.ClaimedBy
				item.ClaimedBy = ""
				item.ClaimedAt = ""
				item.Doc.SetField("claimed_by", "")
				item.Doc.SetField("claimed_at", "")
				return nil
			}); err != nil {
				fmt.Fprintf(os.Stderr, "writing %s: %v\n", itemID, err)
				continue
			}
			if claimedBy != "" {
				_ = mgr.RemoveClaim(claimedBy, itemID)
			}

			changelog.Append(cfg, itemID, changelog.Entry{
				Op: "recover", Field: "claimed_by", OldValue: claimedBy,
				Reason: "stale claim released by sprint recover",
			})
			released++
		}
	}

	if released == 0 {
		fmt.Println("No stale claims found")
	} else {
		fmt.Printf("Released %d stale claim(s)\n", released)
	}

	// Prune dead sessions (stale + no remaining claims)
	pruned, err := mgr.PruneStaleSessions()
	if err == nil && pruned > 0 {
		fmt.Printf("Pruned %d dead session(s)\n", pruned)
	}

	if released > 0 || pruned > 0 {
		autoSync(s, fmt.Sprintf("st sprint recover: %s (released %d, pruned %d)", sprintID, released, pruned))
	}
	return 0
}
