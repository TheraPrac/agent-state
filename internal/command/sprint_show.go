package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SprintShow displays detailed information about a sprint and its items.
func SprintShow(s *store.Store, cfg *config.Config, sprintID string) int {
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

	// Get epic info
	epicTitle := ""
	if ep, ok := r.GetEpic(sp.Epic); ok {
		epicTitle = ep.Title
	}

	// Plan status
	planStatus := "not approved"
	planDate := ""
	if sp.PlanApproved {
		planStatus = "approved"
		if sp.PlanApprovedAt != "" {
			planDate = " (" + sp.PlanApprovedAt + ")"
		}
	}

	// Header
	fmt.Printf("Sprint: %s — %s\n", sp.ID, sp.Title)
	if epicTitle != "" {
		fmt.Printf("Epic:   %s — %s\n", sp.Epic, epicTitle)
	} else {
		fmt.Printf("Epic:   %s\n", sp.Epic)
	}
	fmt.Printf("Status: %s   Plan: %s%s\n", sp.Status, planStatus, planDate)
	// I-405: show the description above the items table when set so
	// the next person (or agent) opening the sprint sees the goal/
	// intent without having to infer it from the included items.
	if sp.Description != "" {
		fmt.Printf("Goal:   %s\n", sp.Description)
	}
	fmt.Println()

	if len(sp.Items) == 0 {
		fmt.Println("  (no items)")
		return 0
	}

	// Build dep graph for blocking info
	g := deps.Build(s.All(), cfg)

	// I-488: queue is the source of execution order. Render members in the
	// order they appear in the queue, with any orphans (in sprint registry
	// but missing from queue — e.g. closed and pruned, or pre-migration
	// data) appended below a "(not queued)" subheader.
	memberSet := make(map[string]bool, len(sp.Items))
	for _, id := range sp.Items {
		memberSet[id] = true
	}

	queue := LoadQueue(cfg)
	var ordered []string
	queuedSet := make(map[string]bool, len(memberSet))
	for _, e := range queue {
		if memberSet[e.ID] {
			ordered = append(ordered, e.ID)
			queuedSet[e.ID] = true
		}
	}
	var orphans []string
	for _, id := range sp.Items {
		if !queuedSet[id] {
			orphans = append(orphans, id)
		}
	}

	// Table header
	fmt.Printf("  %-3s %-8s %-35s %-12s %-8s\n", "#", "ID", "Title", "Status", "Priority")

	complete := 0
	inProgress := 0
	blocked := 0
	row := 0

	renderRow := func(itemID string) {
		row++
		item, ok := s.Get(itemID)
		if !ok {
			fmt.Printf("  %-3d %-8s (not found)\n", row, itemID)
			return
		}

		title := item.Title
		if len(title) > 35 {
			title = title[:32] + "..."
		}

		prio := "p2"
		if item.Priority != nil {
			prio = fmt.Sprintf("p%d", *item.Priority)
		}

		fmt.Printf("  %-3d %-8s %-35s %-12s %-8s\n", row, item.ID, title, item.Status, prio)

		blocksIDs := g.BlocksItems(itemID)
		if len(blocksIDs) > 0 {
			fmt.Printf("  %s blocks %s\n", strings.Repeat(" ", 13), strings.Join(blocksIDs, ", "))
		}

		if cfg.IsTerminalStatus(item.Type, item.Status) {
			complete++
			return
		}
		tc, ok := cfg.Types[item.Type]
		if ok && item.Status == tc.ActiveStatus {
			inProgress++
		}
		if g.IsBlocked(itemID) {
			blocked++
		}
	}

	for _, id := range ordered {
		renderRow(id)
	}
	if len(orphans) > 0 {
		fmt.Println()
		fmt.Println("  (not queued — sprint members missing from the queue)")
		for _, id := range orphans {
			renderRow(id)
		}
	}

	fmt.Println()
	fmt.Printf("Progress: %d/%d complete, %d in-progress, %d blocked\n",
		complete, len(sp.Items), inProgress, blocked)

	return 0
}
