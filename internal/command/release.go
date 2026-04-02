package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Release clears the session claim and agent assignment on an item,
// allowing another session/agent to work on it.
func Release(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.AssignedTo == "" && item.ClaimedBy == "" {
		fmt.Fprintf(os.Stderr, "%s is not assigned or claimed\n", id)
		return 1
	}

	oldAgent := item.AssignedTo
	oldClaim := item.ClaimedBy

	// Clear agent assignment
	if item.AssignedTo != "" {
		item.AssignedTo = ""
		item.Doc.SetField("assigned_to", "")
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

	// Release item lock
	store.UnlockItem(cfg, id)

	changelog.Append(cfg, id, changelog.Entry{
		Op: "release", Field: "assigned_to", OldValue: oldAgent,
	})

	if oldAgent != "" && oldClaim != "" {
		fmt.Printf("Released %s — was assigned to %s, claimed by session %s\n", id, oldAgent, oldClaim)
	} else if oldAgent != "" {
		fmt.Printf("Released %s — was assigned to %s\n", id, oldAgent)
	} else {
		fmt.Printf("Released %s — was claimed by session %s\n", id, oldClaim)
	}
	return 0
}
