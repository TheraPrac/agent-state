package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintJoin binds the current session to a sprint.
func SprintJoin(cfg *config.Config, sprintID string) int {
	sessionID := cfg.SessionID()
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "no session ID — set $AS_SESSION_ID")
		return 1
	}

	// Validate sprint exists
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Update session file
	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sess, err := mgr.EnsureSession(sessionID, cfg.AgentID())
	if err != nil {
		fmt.Fprintf(os.Stderr, "session error: %v\n", err)
		return 1
	}

	if sess.Sprint == sprintID {
		fmt.Printf("Already joined sprint %s — %s\n", sp.ID, sp.Title)
		return 0
	}

	sess.Sprint = sprintID
	sess.LastActive = time.Now()
	if err := mgr.Save(sess); err != nil {
		fmt.Fprintf(os.Stderr, "saving session: %v\n", err)
		return 1
	}

	fmt.Printf("Joined sprint %s — %s\n", sp.ID, sp.Title)
	return 0
}

// SprintLeave unbinds the current session from its sprint and releases all claims.
func SprintLeave(s *store.Store, cfg *config.Config) int {
	sessionID := cfg.SessionID()
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "no session ID — set $AS_SESSION_ID")
		return 1
	}

	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sess, err := mgr.Load(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading session: %v\n", err)
		return 1
	}
	if sess == nil {
		fmt.Fprintln(os.Stderr, "no active session")
		return 1
	}

	if sess.Sprint == "" {
		fmt.Println("Not joined to any sprint")
		return 0
	}

	oldSprint := sess.Sprint

	// Release all claims held by this session
	for _, itemID := range sess.ClaimedItems {
		if _, ok := s.Get(itemID); !ok {
			continue
		}
		capturedSessionID := sessionID
		_ = s.Mutate(itemID, func(item *model.Item) error {
			if item.ClaimedBy == capturedSessionID {
				item.ClaimedBy = ""
				item.ClaimedAt = ""
				item.Doc.SetField("claimed_by", "")
				item.Doc.SetField("claimed_at", "")
			}
			return nil
		})
	}
	sess.ClaimedItems = nil

	sess.Sprint = ""
	sess.LastActive = time.Now()
	if err := mgr.Save(sess); err != nil {
		fmt.Fprintf(os.Stderr, "saving session: %v\n", err)
		return 1
	}

	fmt.Printf("Left sprint %s\n", oldSprint)
	autoSync(s, fmt.Sprintf("st sprint leave: %s", oldSprint))
	return 0
}
