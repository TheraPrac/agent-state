package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// PrimeOpts holds flags for the prime command.
type PrimeOpts struct {
	Format  string // "markdown" (default) or "json"
	Compact bool   // compact output for hook injection (~50 lines)
}

type primeData struct {
	Active           []primeItem    `json:"active"`
	Ready            []primeItem    `json:"ready"`
	Issues           int            `json:"open_issues"`
	IssuesByPriority map[int]int `json:"issues_by_priority"` // I-406: priority-bucketed open issues
	Queued           int            `json:"queued_tasks"`
	Archive          int            `json:"archived"`
	Guidance         string         `json:"guidance,omitempty"`
	Sprint           *sprintContext `json:"sprint,omitempty"`
}

type sprintContext struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Total      int    `json:"total"`
	Complete   int    `json:"complete"`
	InProgress int    `json:"in_progress"`
	Blocked    int    `json:"blocked"`
	CrossDeps  []crossDep `json:"cross_deps,omitempty"`
}

type crossDep struct {
	ItemID string `json:"item_id"`
	DepID  string `json:"dep_id"`
	DepSprint string `json:"dep_sprint,omitempty"`
}

type primeItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Stage    string `json:"stage,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Assigned string `json:"assigned,omitempty"`
}

func Prime(s *store.Store, cfg *config.Config, opts PrimeOpts) int {
	// Check if session is bound to a sprint
	sprintID := resolveSessionSprint(cfg)
	if sprintID != "" {
		return sprintScopedPrime(s, cfg, opts, sprintID)
	}
	return globalPrime(s, cfg, opts)
}

// resolveSessionSprint returns the sprint ID if the current session is joined to one.
func resolveSessionSprint(cfg *config.Config) string {
	sessionID := cfg.SessionID()
	if sessionID == "" {
		return ""
	}
	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sess, err := mgr.Load(sessionID)
	if err != nil || sess == nil {
		return ""
	}
	return sess.Sprint
}

// sprintScopedPrime outputs context scoped to a sprint's items.
func sprintScopedPrime(s *store.Store, cfg *config.Config, opts PrimeOpts, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		// Fall back to global if registry fails
		return globalPrime(s, cfg, opts)
	}

	sp, err := r.SprintByID(sprintID)
	if err != nil {
		// Sprint gone — fall back to global
		return globalPrime(s, cfg, opts)
	}

	g := deps.Build(s.All(), cfg)

	// Build sprint item set for lookups
	sprintItems := make(map[string]bool)
	for _, id := range sp.Items {
		sprintItems[id] = true
	}

	// Categorize sprint items
	var active, ready []primeItem
	complete, inProgress, blocked := 0, 0, 0
	var crossDeps []crossDep

	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}

		p := 2
		if item.Priority != nil {
			p = *item.Priority
		}

		pi := primeItem{
			ID:       item.ID,
			Title:    item.Title,
			Stage:    deliveryStage(item),
			Priority: p,
			Assigned: item.AssignedTo,
		}

		if cfg.IsTerminalStatus(item.Type, item.Status) {
			complete++
			continue
		}

		tc, ok := cfg.Types[item.Type]
		if ok && item.Status == tc.ActiveStatus {
			active = append(active, pi)
			inProgress++
		} else if !g.IsBlocked(itemID) && item.ClaimedBy == "" {
			ready = append(ready, pi)
		}

		if g.IsBlocked(itemID) {
			blocked++
		}

		// Detect cross-sprint deps
		for _, depID := range item.DependsOn {
			if sprintItems[depID] {
				continue // intra-sprint dep
			}
			depItem, ok := s.Get(depID)
			if ok && !cfg.IsTerminalStatus(depItem.Type, depItem.Status) {
				cd := crossDep{ItemID: itemID, DepID: depID, DepSprint: depItem.Sprint}
				crossDeps = append(crossDeps, cd)
			}
		}
	}

	// Sort ready by priority
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority < ready[j].Priority
		}
		return ready[i].ID < ready[j].ID
	})

	data := primeData{
		Active:           active,
		Ready:            ready,
		IssuesByPriority: make(map[int]int),
		Sprint: &sprintContext{
			ID:         sp.ID,
			Title:      sp.Title,
			Total:      len(sp.Items),
			Complete:   complete,
			InProgress: inProgress,
			Blocked:    blocked,
			CrossDeps:  crossDeps,
		},
	}

	if opts.Format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(data)
		return 0
	}

	// Markdown output — sprint-scoped
	var b strings.Builder
	b.WriteString("=== AS CONTEXT ===\n\n")

	// Sprint header
	b.WriteString(fmt.Sprintf("## Sprint: %s — %s\n", sp.ID, sp.Title))
	b.WriteString(fmt.Sprintf("  Progress: %d/%d complete, %d in-progress, %d blocked\n\n",
		complete, len(sp.Items), inProgress, blocked))

	// Active work in sprint
	b.WriteString("## Active Work\n")
	if len(active) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range active {
			line := fmt.Sprintf("  %-8s %s", item.ID, item.Title)
			if item.Stage != "" {
				line += fmt.Sprintf("  stage: %s", item.Stage)
			}
			if item.Assigned != "" {
				line += fmt.Sprintf("  [%s]", item.Assigned)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n")

	// Work stack (still relevant within sprint)
	stackEntries := LoadStack(cfg)
	if len(stackEntries) > 0 {
		b.WriteString("## Stack\n")
		for i := len(stackEntries) - 1; i >= 0; i-- {
			e := stackEntries[i]
			item, ok := s.Get(e.ID)
			title := "(not found)"
			if ok {
				title = truncate(item.Title, 45)
			}
			resolved := ""
			if ok && cfg.IsTerminalStatus(item.Type, item.Status) {
				resolved = " resolved"
			}
			marker := ""
			if i == len(stackEntries)-1 {
				marker = " <- current"
			}
			b.WriteString(fmt.Sprintf("  %d: %-8s %s%s%s\n", i, e.ID, title, resolved, marker))
		}
		b.WriteString("\n")
	}

	// Next claimable item
	if len(ready) > 0 {
		b.WriteString("## Next Action\n")
		next := ready[0]
		b.WriteString(fmt.Sprintf("  Next claimable: %s — %s\n", next.ID, next.Title))
		b.WriteString(fmt.Sprintf("  -> st start %s\n", next.ID))
		b.WriteString("\n")
	} else if len(active) > 0 {
		activeID := active[0].ID
		action := NextAction(s, cfg, activeID)
		if action != "" {
			b.WriteString("## Next Action\n")
			b.WriteString(fmt.Sprintf("  Current: %s\n", activeID))
			b.WriteString(fmt.Sprintf("  -> %s\n", action))
			b.WriteString("\n")
		}
	}

	// Ready items in sprint
	readyLimit := 5
	if opts.Compact {
		readyLimit = 3
	}
	b.WriteString(fmt.Sprintf("## Ready (top %d)\n", readyLimit))
	shown := ready
	if len(shown) > readyLimit {
		shown = shown[:readyLimit]
	}
	if len(shown) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range shown {
			b.WriteString(fmt.Sprintf("  %-8s p%d  %s\n", item.ID, item.Priority, item.Title))
		}
	}
	b.WriteString("\n")

	// Cross-sprint blockers
	if len(crossDeps) > 0 {
		b.WriteString("## Cross-Sprint Blockers\n")
		for _, cd := range crossDeps {
			depItem, ok := s.Get(cd.DepID)
			depTitle := cd.DepID
			if ok {
				depTitle = depItem.Title
			}
			sprintNote := ""
			if cd.DepSprint != "" {
				sprintNote = fmt.Sprintf(" (sprint: %s)", cd.DepSprint)
			} else {
				sprintNote = " (unsprinted)"
			}
			b.WriteString(fmt.Sprintf("  %s blocked by %s — %s%s\n", cd.ItemID, cd.DepID, depTitle, sprintNote))
		}
		b.WriteString("\n")
	}

	// Summary
	b.WriteString("## Summary\n")
	b.WriteString(fmt.Sprintf("  Sprint %d/%d complete\n\n", complete, len(sp.Items)))

	fmt.Print(b.String())
	return 0
}

// globalPrime is the original global prime output (no sprint scope).
func globalPrime(s *store.Store, cfg *config.Config, opts PrimeOpts) int {
	g := deps.Build(s.All(), cfg)
	data := buildPrimeData(s, cfg, g)

	if opts.Format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(data)
		return 0
	}

	// Markdown output
	var b strings.Builder
	b.WriteString("=== AS CONTEXT ===\n\n")

	// Active work
	b.WriteString("## Active Work\n")
	if len(data.Active) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range data.Active {
			line := fmt.Sprintf("  %-8s %s", item.ID, item.Title)
			if item.Stage != "" {
				line += fmt.Sprintf("  stage: %s", item.Stage)
			}
			if item.Assigned != "" {
				line += fmt.Sprintf("  [%s]", item.Assigned)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n")

	// Work stack
	stackEntries := LoadStack(cfg)
	if len(stackEntries) > 0 {
		b.WriteString("## Stack\n")
		for i := len(stackEntries) - 1; i >= 0; i-- {
			e := stackEntries[i]
			item, ok := s.Get(e.ID)
			title := "(not found)"
			if ok {
				title = truncate(item.Title, 45)
			}
			resolved := ""
			if ok && cfg.IsTerminalStatus(item.Type, item.Status) {
				resolved = " ✓ resolved"
			}
			marker := ""
			if i == len(stackEntries)-1 {
				marker = " ← current"
			}
			b.WriteString(fmt.Sprintf("  %d: %-8s %s%s%s\n", i, e.ID, title, resolved, marker))
		}
		b.WriteString("\n")
	}

	// Work queue — filter out sprint-assigned items
	queueEntries := LoadQueue(cfg)
	var filteredQueue []QueueEntry
	for _, e := range queueEntries {
		item, ok := s.Get(e.ID)
		if ok && item.Sprint != "" {
			continue // skip sprint-assigned items in queue
		}
		filteredQueue = append(filteredQueue, e)
	}
	if len(filteredQueue) > 0 {
		b.WriteString("## Queue\n")
		limit := 5
		if opts.Compact {
			limit = 3
		}
		shown := filteredQueue
		if len(shown) > limit {
			shown = shown[:limit]
		}
		for i, e := range shown {
			item, ok := s.Get(e.ID)
			title := "(not found)"
			marker := ""
			if ok {
				title = truncate(item.Title, 45)
				if item.Status == "active" {
					marker = " ← ACTIVE"
				}
			}
			if !e.Approved {
				marker += " (pending approval)"
			}
			b.WriteString(fmt.Sprintf("  %d. %-8s %s%s\n", i+1, e.ID, title, marker))
		}
		if len(filteredQueue) > limit {
			b.WriteString(fmt.Sprintf("  ... +%d more\n", len(filteredQueue)-limit))
		}
		b.WriteString("\n")
	}

	// Next action directive — stack beats queue beats other active
	activeID := ""
	// 1. Top of stack (interrupted work takes priority)
	if len(stackEntries) > 0 {
		top := stackEntries[len(stackEntries)-1]
		if item, ok := s.Get(top.ID); ok && !cfg.IsTerminalStatus(item.Type, item.Status) {
			activeID = top.ID
		}
	}
	// 2. Queue item if no stack
	if activeID == "" {
		for _, e := range filteredQueue {
			if item, ok := s.Get(e.ID); ok && item.Status == "active" {
				activeID = e.ID
				break
			}
		}
	}
	// 3. Any active item as fallback
	if activeID == "" && len(data.Active) > 0 {
		activeID = data.Active[0].ID
	}
	if activeID != "" {
		action := NextAction(s, cfg, activeID)
		if action != "" {
			b.WriteString("## Next Action\n")
			b.WriteString(fmt.Sprintf("  Current: %s\n", activeID))
			b.WriteString(fmt.Sprintf("  → %s\n", action))
			b.WriteString("\n")
		}
	} else if activeID == "" && len(filteredQueue) > 0 {
		// No active work — suggest starting the first queue item
		nextID := ""
		for _, e := range filteredQueue {
			if e.Approved {
				nextID = e.ID
				break
			}
		}
		if nextID != "" {
			b.WriteString("## Next Action\n")
			b.WriteString(fmt.Sprintf("  No active work. Next in queue: %s\n", nextID))
			b.WriteString(fmt.Sprintf("  → st start %s\n", nextID))
			b.WriteString("\n")
		}
	}

	// Ready queue
	readyLimit := 5
	if opts.Compact {
		readyLimit = 3
	}
	readyLabel := fmt.Sprintf("## Ready (top %d)\n", readyLimit)
	b.WriteString(readyLabel)
	shown := data.Ready
	if len(shown) > readyLimit {
		shown = shown[:readyLimit]
	}
	if len(shown) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, item := range shown {
			b.WriteString(fmt.Sprintf("  %-8s p%d  %s\n", item.ID, item.Priority, item.Title))
		}
	}
	b.WriteString("\n")

	// Open issues by severity
	b.WriteString("## Open Issues\n")
	// I-406: bucket open issues by priority (0-4) instead of legacy
	// severity. The labels keep operator orientation: p0/p1 are urgent,
	// p2 is the default, p3/p4 are deferred / tech-debt territory.
	b.WriteString(fmt.Sprintf("  p0: %d  p1: %d  p2: %d  p3: %d  p4: %d\n\n",
		data.IssuesByPriority[0], data.IssuesByPriority[1], data.IssuesByPriority[2],
		data.IssuesByPriority[3], data.IssuesByPriority[4]))

	// Summary
	b.WriteString("## Summary\n")
	b.WriteString(fmt.Sprintf("  %d open issues  %d queued tasks  %d archived\n\n", data.Issues, data.Queued, data.Archive))

	// Guidance
	if data.Guidance != "" {
		b.WriteString("## Guidance\n")
		b.WriteString(fmt.Sprintf("  %s\n\n", data.Guidance))
	}

	// Command reference (omit in compact mode)
	if !opts.Compact {
		b.WriteString("## Commands\n")
		b.WriteString("  as show <id>     — item detail\n")
		b.WriteString("  as start <id>    — claim and activate\n")
		b.WriteString("  as close <id> <resolution> — close with gates\n")
		b.WriteString("  as status        — dashboard\n")
		b.WriteString("  as ready         — unblocked items\n")
		b.WriteString("  as check         — validate all files\n")
	}

	fmt.Print(b.String())
	return 0
}

func buildPrimeData(s *store.Store, cfg *config.Config, g *deps.Graph) primeData {
	data := primeData{
		IssuesByPriority: make(map[int]int),
	}

	// Active work
	active := s.List(store.StatusFilter("active"))
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	for _, item := range active {
		data.Active = append(data.Active, primeItem{
			ID:       item.ID,
			Title:    item.Title,
			Stage:    deliveryStage(item),
			Assigned: item.AssignedTo,
		})
	}

	// Ready queue (all of them — caller trims for display)
	ready := g.Ready()
	for _, item := range ready {
		p := 2
		if item.Priority != nil {
			p = *item.Priority
		}
		data.Ready = append(data.Ready, primeItem{
			ID:       item.ID,
			Title:    item.Title,
			Priority: p,
		})
	}

	// I-406: counts + open-issues-by-priority. Priority field replaces
	// severity; items missing priority bucket as p2.
	for _, item := range s.All() {
		if item.Type == "issue" && item.Status == "open" {
			data.Issues++
			p := 2
			if item.Priority != nil {
				p = *item.Priority
			}
			data.IssuesByPriority[p]++
		}
		if isStartStatus(item, cfg) {
			data.Queued++
		}
		if isTerminal(item, cfg) {
			data.Archive++
		}
	}

	// Guidance from config
	data.Guidance = cfg.Guidance

	return data
}
