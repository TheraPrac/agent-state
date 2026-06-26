package command

import (
	"fmt"
	"os"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/session"
	"github.com/theraprac/agent-state/internal/store"
)

// SprintStatus shows a coordinator view of sprints.
// Without args: all active sprints with progress/sessions.
// With sprint ID: single-sprint detail with sessions, claims, blockers.
func SprintStatus(s *store.Store, cfg *config.Config, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sessions, _ := mgr.ListSessions()

	if sprintID != "" {
		return sprintStatusDetail(s, cfg, r, mgr, sessions, sprintID)
	}
	return sprintStatusOverview(s, cfg, r, sessions)
}

func sprintStatusOverview(s *store.Store, cfg *config.Config, r *registry.Registry, sessions []*session.Session) int {
	activeSprints := r.ListSprints("")
	if len(activeSprints) == 0 {
		fmt.Println("No active sprints")
		return 0
	}

	g := deps.Build(s.All(), cfg)

	fmt.Printf("%sActive Sprints%s\n\n", cBold, cReset)
	fmt.Printf("  %-30s %-30s %8s %8s %s\n", "ID", "Title", "Progress", "Sessions", "Blockers")

	for _, sp := range activeSprints {
		if sp.Status != "active" {
			continue
		}

		complete, total, blocked := 0, len(sp.Items), 0
		for _, itemID := range sp.Items {
			item, ok := s.Get(itemID)
			if !ok {
				continue
			}
			if cfg.IsTerminalStatus(item.Type, item.Status) {
				complete++
			}
			if g.IsBlocked(itemID) {
				blocked++
			}
		}

		// Count sessions joined to this sprint
		sessionCount := 0
		for _, sess := range sessions {
			if sess.Sprint == sp.ID {
				sessionCount++
			}
		}

		blockerStr := "none"
		if blocked > 0 {
			blockerStr = fmt.Sprintf("%d blocked", blocked)
		}

		title := sp.Title
		if len(title) > 30 {
			title = title[:27] + "..."
		}

		fmt.Printf("  %-30s %-30s %3d/%-4d %5d    %s\n",
			sp.ID, title, complete, total, sessionCount, blockerStr)
	}

	fmt.Println()
	return 0
}

func sprintStatusDetail(s *store.Store, cfg *config.Config, r *registry.Registry, mgr *session.Manager, sessions []*session.Session, sprintID string) int {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	g := deps.Build(s.All(), cfg)

	// Header
	epicTitle := ""
	if ep, ok := r.GetEpic(sp.Epic); ok {
		epicTitle = ep.Title
	}
	fmt.Printf("Sprint: %s — %s\n", sp.ID, sp.Title)
	if epicTitle != "" {
		fmt.Printf("Epic:   %s — %s\n", sp.Epic, epicTitle)
	}
	fmt.Println()

	// Sessions on this sprint
	var sprintSessions []*session.Session
	for _, sess := range sessions {
		if sess.Sprint == sp.ID {
			sprintSessions = append(sprintSessions, sess)
		}
	}

	if len(sprintSessions) > 0 {
		fmt.Printf("Sessions (%d):\n", len(sprintSessions))
		for _, sess := range sprintSessions {
			stale := ""
			if mgr.IsStale(sess) {
				stale = " (stale)"
			}
			claims := "none"
			if len(sess.ClaimedItems) > 0 {
				claims = fmt.Sprintf("%v", sess.ClaimedItems)
			}
			fmt.Printf("  %s  agent: %s  claims: %s%s\n", sess.ID, sess.AgentID, claims, stale)
		}
		fmt.Println()
	}

	// Items
	complete, inProgress, blocked := 0, 0, 0
	fmt.Printf("  %-3s %-8s %-30s %-10s %-10s %s\n", "#", "ID", "Title", "Status", "Claimed", "Blockers")
	for i, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			fmt.Printf("  %-3d %-8s (not found)\n", i+1, itemID)
			continue
		}

		title := item.Title
		if len(title) > 30 {
			title = title[:27] + "..."
		}

		claimed := ""
		if item.ClaimedBy != "" {
			claimed = item.ClaimedBy[:8] + "..."
			if len(item.ClaimedBy) <= 8 {
				claimed = item.ClaimedBy
			}
		}

		blockerStr := ""
		if g.IsBlocked(itemID) {
			unresolved := g.UnresolvedDeps(itemID)
			blockerStr = fmt.Sprintf("blocked by %v", unresolved)
			blocked++
		}

		if cfg.IsTerminalStatus(item.Type, item.Status) {
			complete++
		} else {
			tc, ok := cfg.Types[item.Type]
			if ok && item.Status == tc.ActiveStatus {
				inProgress++
			}
		}

		fmt.Printf("  %-3d %-8s %-30s %-10s %-10s %s\n", i+1, item.ID, title, item.Status, claimed, blockerStr)
	}

	fmt.Println()
	fmt.Printf("Progress: %d/%d complete, %d in-progress, %d blocked\n",
		complete, len(sp.Items), inProgress, blocked)

	return 0
}
