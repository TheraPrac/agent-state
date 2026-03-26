package command

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ANSI color constants
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cMagenta = "\033[35m"
	cCyan   = "\033[36m"
	cWhite  = "\033[37m"
	cBlue   = "\033[34m"
	cBoldW  = "\033[1m\033[37m"
	cBoldC  = "\033[1m\033[36m"
	cBoldM  = "\033[1m\033[35m"
	cBoldB  = "\033[1m\033[34m"
)

// StatusOpts holds flags for the status command.
type StatusOpts struct {
	Issues    bool
	Tasks     bool
	Recent    bool
	All       bool
	Completed bool
	Check     bool
}

func Status(s *store.Store, cfg *config.Config, id string, opts StatusOpts) int {
	if id != "" {
		return statusSingle(s, cfg, id)
	}
	if opts.Check {
		return Check(s, cfg, false, false)
	}
	if opts.All {
		opts.Issues = true
		opts.Tasks = true
		opts.Recent = true
		opts.Completed = true
	}
	return statusDashboard(s, cfg, opts)
}

func statusDashboard(s *store.Store, cfg *config.Config, opts StatusOpts) int {
	g := deps.Build(s.All(), cfg)

	// Count items by category
	var activeCount, issueCount, queuedCount, archivedCount int
	for _, item := range s.All() {
		switch {
		case item.Status == "active":
			activeCount++
		case item.Type == "issue" && item.Status == "open":
			issueCount++
		case isStartStatus(item, cfg):
			queuedCount++
		case isTerminal(item, cfg):
			archivedCount++
		}
	}

	// Header
	fmt.Printf("%s%s Agent State%s  ", cBoldW, cfg.Project.Name, cReset)
	fmt.Printf("%s%d active%s  ", cGreen, activeCount, cReset)
	fmt.Printf("%s%d issues%s  ", cRed, issueCount, cReset)
	fmt.Printf("%s%d queued%s  ", cCyan, queuedCount, cReset)
	fmt.Printf("%s%d archived%s\n\n", cDim, archivedCount, cReset)

	// Active work
	active := s.List(store.StatusFilter("active"))
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	fmt.Printf("%s━━━ ACTIVE WORK ━━━%s\n", cBoldW, cReset)
	if len(active) == 0 {
		fmt.Printf("  %s(none)%s\n", cDim, cReset)
	} else {
		for _, item := range active {
			stage := deliveryStage(item)
			assigned := ""
			if item.AssignedTo != "" {
				assigned = fmt.Sprintf("  [%s]", item.AssignedTo)
			}
			stageStr := ""
			if stage != "" {
				stageStr = fmt.Sprintf("  (%s)", stage)
			}
			fmt.Printf("  %s%-8s%s %s%s%s\n", cBold, item.ID, cReset, item.Title, stageStr, assigned)
		}
	}
	fmt.Println()

	// Pending UAT
	uatPending := findUATPending(s, cfg)
	if len(uatPending) > 0 {
		fmt.Printf("%s━━━ PENDING UAT ━━━%s\n", cBoldW, cReset)
		for _, item := range uatPending {
			stage := deliveryStage(item)
			deployed := ""
			if d, ok := item.Delivery["deployed_at"]; ok {
				if ds, ok := d.(string); ok && ds != "" {
					deployed = fmt.Sprintf("  (deployed %s)", ds[:10])
				}
			}
			stageStr := ""
			if stage != "" {
				stageStr = fmt.Sprintf("  (%s)", stage)
			}
			fmt.Printf("  %s▶ on DEV%s  %s%-8s%s %s%s%s\n",
				cMagenta, cReset, cBold, item.ID, cReset, truncate(item.Title, 55), stageStr, deployed)
		}
		fmt.Println()
	}

	// Issues section
	if opts.Issues {
		printIssues(s)
	}

	// Tasks section
	if opts.Tasks {
		printQueuedTasks(s, cfg, g)
	}

	// Recent closures
	if opts.Recent {
		printRecent(s, cfg)
	}

	// Completed
	if opts.Completed {
		printCompleted(s, cfg)
	}

	// Summary footer (only when sections are collapsed)
	if !opts.Issues && !opts.Tasks && !opts.Recent {
		sevCounts := map[string]int{}
		for _, item := range s.All() {
			if item.Type == "issue" && item.Status == "open" {
				sev := item.Severity
				if sev == "" {
					sev = "medium"
				}
				sevCounts[sev]++
			}
		}
		sevSummary := ""
		for _, sev := range []string{"critical", "high", "medium", "low"} {
			if n, ok := sevCounts[sev]; ok && n > 0 {
				sevSummary += fmt.Sprintf("  %s%d %s%s", cYellow, n, sevAbbrev(sev), cReset)
			}
		}

		cutoff := time.Now().AddDate(0, 0, -7)
		var recentCount int
		for _, item := range s.All() {
			if isTerminal(item, cfg) && item.Completed != nil && item.Completed.After(cutoff) {
				recentCount++
			}
		}

		fmt.Printf("  %sIssues:%s %d open%s  %s(status -i)%s\n", cBold, cReset, issueCount, sevSummary, cDim, cReset)
		fmt.Printf("  %sTasks:%s  %d queued  %s(status -t)%s\n", cBold, cReset, queuedCount, cDim, cReset)
		fmt.Printf("  %sRecent:%s %d closed (7d)  %s(status -r)%s\n", cBold, cReset, recentCount, cDim, cReset)
	}

	return 0
}

func printIssues(s *store.Store) {
	openIssues := s.List(store.TypeFilter("issue"), store.StatusFilter("open"))
	sort.Slice(openIssues, func(i, j int) bool {
		ri, rj := severityRank(openIssues[i].Severity), severityRank(openIssues[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return openIssues[i].ID < openIssues[j].ID
	})

	// Count by severity
	sevCounts := map[string]int{}
	for _, item := range openIssues {
		sev := item.Severity
		if sev == "" {
			sev = "medium"
		}
		sevCounts[sev]++
	}
	summary := fmt.Sprintf("  %s%d open%s", cBold, len(openIssues), cReset)
	for _, sev := range []string{"critical", "high", "medium", "normal", "low"} {
		if n, ok := sevCounts[sev]; ok && n > 0 {
			summary += fmt.Sprintf("  %s%d %s%s", cYellow, n, sevAbbrev(sev), cReset)
		}
	}

	fmt.Printf("%s━━━ OPEN ISSUES ━━━%s\n", cBoldW, cReset)
	fmt.Println(summary)
	fmt.Println()

	for _, item := range openIssues {
		sev := item.Severity
		if sev == "" {
			sev = "medium"
		}
		label := sevLabel(sev)
		work := workStatus(item)
		fmt.Printf("  %s  %s%-8s%s %s  %s\n",
			label, cBold, item.ID, cReset, truncate(item.Title, 65), work)
	}
	fmt.Println()
}

func printQueuedTasks(s *store.Store, cfg *config.Config, g *deps.Graph) {
	queuedTasks := s.List(store.TypeFilter("task"), store.StatusFilter("queued"))
	if len(queuedTasks) == 0 {
		return
	}

	// Try to load epics registry for grouping
	reg, _ := registry.Load(cfg.EpicsPath())

	// Group by epic → sprint → tag
	type group struct {
		epic    string
		eTitle  string
		sprint  string
		sTitle  string
		tag     string
		items   []*model.Item
	}
	type gkey struct{ epic, sprint, tag string }

	groupMap := map[gkey]*group{}
	var keys []gkey

	for _, item := range queuedTasks {
		epic := item.Epic
		sprint := item.Sprint
		tag := "uncategorized"
		if len(item.Tags) > 0 {
			tag = item.Tags[0]
		}
		k := gkey{epic, sprint, tag}
		if _, ok := groupMap[k]; !ok {
			eTitle := ""
			if epic != "" && reg != nil {
				if e, ok := reg.GetEpic(epic); ok {
					eTitle = e.Title
				}
			}
			sTitle := ""
			if sprint != "" && reg != nil {
				if sp, ok := reg.GetSprint(sprint); ok {
					sTitle = sp.Title
				}
			}
			groupMap[k] = &group{epic: epic, eTitle: eTitle, sprint: sprint, sTitle: sTitle, tag: tag}
			keys = append(keys, k)
		}
		groupMap[k].items = append(groupMap[k].items, item)
	}

	// Sort groups: epic first, then sprint, then tag; "uncategorized" last
	sort.SliceStable(keys, func(i, j int) bool {
		gi, gj := groupMap[keys[i]], groupMap[keys[j]]
		if (gi.epic != "") != (gj.epic != "") {
			return gi.epic != ""
		}
		if gi.eTitle != gj.eTitle {
			return gi.eTitle < gj.eTitle
		}
		if (gi.sprint != "") != (gj.sprint != "") {
			return gi.sprint != ""
		}
		if gi.sTitle != gj.sTitle {
			return gi.sTitle < gj.sTitle
		}
		if gi.tag == "uncategorized" {
			return false
		}
		if gj.tag == "uncategorized" {
			return true
		}
		return gi.tag < gj.tag
	})

	// Sort items within each group by priority
	for _, grp := range groupMap {
		sort.Slice(grp.items, func(i, j int) bool {
			pi, pj := priorityOf(grp.items[i]), priorityOf(grp.items[j])
			if pi != pj {
				return pi < pj
			}
			return grp.items[i].ID < grp.items[j].ID
		})
	}

	fmt.Printf("%s━━━ QUEUED TASKS ━━━%s\n", cBoldW, cReset)
	fmt.Printf("  %s%d queued%s\n\n", cBold, len(queuedTasks), cReset)

	currentEpic := ""
	currentSprint := ""
	for _, k := range keys {
		grp := groupMap[k]

		// Epic header (bold magenta)
		if grp.epic != currentEpic {
			currentEpic = grp.epic
			currentSprint = ""
			if grp.epic != "" {
				fmt.Printf("\n  %s◆ %s — %s%s\n", cBoldM, grp.epic, grp.eTitle, cReset)
			}
		}

		// Sprint header (bold cyan)
		if grp.sprint != currentSprint {
			currentSprint = grp.sprint
			if grp.sprint != "" {
				fmt.Printf("   %s▸ %s — %s%s\n", cBoldC, grp.sprint, grp.sTitle, cReset)
			}
		}

		// Tag subheader (blue with icon)
		if grp.tag != "uncategorized" || grp.epic != "" {
			fmt.Printf("    %s◇ %s%s\n", cBoldB, grp.tag, cReset)
		}

		for _, item := range grp.items {
			p := priorityOf(item)
			blocksItems := g.BlocksItems(item.ID)
			blocked := g.IsBlocked(item.ID)
			blockedBy := ""
			if blocked {
				unresolved := g.UnresolvedDeps(item.ID)
				blockedBy = fmt.Sprintf("  %s⊘ blocked by %s%s", cRed, strings.Join(unresolved, ", "), cReset)
			}
			blocksStr := ""
			if len(blocksItems) > 0 {
				blocksStr = fmt.Sprintf(" %s▶ blocks %s%s", cYellow, strings.Join(blocksItems, ", "), cReset)
			}
			idColor := cGreen
			if blocked {
				idColor = cRed
			}
			fmt.Printf("    %s%-8s%s %s (p%d)%s%s\n",
				idColor, item.ID, cReset, truncate(item.Title, 55), p, blocksStr, blockedBy)
		}
	}
	fmt.Println()
}

func printRecent(s *store.Store, cfg *config.Config) {
	fmt.Printf("%s━━━ RECENTLY CLOSED (7d) ━━━%s\n", cBoldW, cReset)
	cutoff := time.Now().AddDate(0, 0, -7)
	var recent []*model.Item
	for _, item := range s.All() {
		if !isTerminal(item, cfg) {
			continue
		}
		if item.Completed != nil && item.Completed.After(cutoff) {
			recent = append(recent, item)
		}
	}
	sort.Slice(recent, func(i, j int) bool {
		if recent[i].Completed == nil || recent[j].Completed == nil {
			return false
		}
		return recent[i].Completed.After(*recent[j].Completed)
	})

	if len(recent) == 0 {
		fmt.Printf("  %s(none)%s\n", cDim, cReset)
	} else {
		for _, item := range recent {
			completed := ""
			if item.Completed != nil {
				completed = item.Completed.Format("2006-01-02")
			}
			fmt.Printf("  %-8s  %-10s  %s  %s\n", item.ID, item.Status, completed, item.Title)
		}
	}
	fmt.Println()
}

func printCompleted(s *store.Store, cfg *config.Config) {
	fmt.Printf("%s━━━ COMPLETED ━━━%s\n", cBoldW, cReset)
	var completed []*model.Item
	for _, item := range s.All() {
		if isTerminal(item, cfg) {
			completed = append(completed, item)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].ID < completed[j].ID })

	for _, item := range completed {
		fmt.Printf("  %-8s  %-10s  %s\n", item.ID, item.Status, item.Title)
	}
	fmt.Printf("\n  %d items\n\n", len(completed))
}

// --- Single item view ---

func statusSingle(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	fmt.Printf("%s%s%s — %s\n", cBold, item.ID, cReset, item.Title)
	fmt.Printf("  Type:     %s\n", item.Type)
	fmt.Printf("  Status:   %s\n", item.Status)

	if item.AssignedTo != "" {
		fmt.Printf("  Assigned: %s\n", item.AssignedTo)
	}
	if item.Severity != "" {
		fmt.Printf("  Severity: %s\n", item.Severity)
	}
	if item.Priority != nil {
		fmt.Printf("  Priority: %d\n", *item.Priority)
	}

	stage := deliveryStage(item)
	if stage != "" {
		fmt.Printf("  Stage:    %s\n", stage)
	}

	if branch, ok := item.WorkTracking["branch"]; ok {
		if bs, ok := branch.(string); ok && bs != "" && bs != "null" {
			fmt.Printf("  Branch:   %s\n", bs)
		}
	}
	if pr, ok := item.WorkTracking["pr"]; ok {
		if ps, ok := pr.(string); ok && ps != "" && ps != "null" {
			fmt.Printf("  PR:       %s\n", ps)
		}
	}

	if len(item.DependsOn) > 0 {
		fmt.Printf("  Depends:  %s\n", strings.Join(item.DependsOn, ", "))
	}

	g := deps.Build(s.All(), cfg)
	blocks := g.BlocksItems(id)
	if len(blocks) > 0 {
		fmt.Printf("  Blocks:   %s\n", strings.Join(blocks, ", "))
	}

	if len(item.Tags) > 0 {
		fmt.Printf("  Tags:     %s\n", strings.Join(item.Tags, ", "))
	}

	fmt.Printf("  Created:  %s\n", item.Created.Format("2006-01-02"))
	fmt.Printf("  Touched:  %s\n", item.LastTouched.Format("2006-01-02"))
	if item.Completed != nil {
		fmt.Printf("  Completed: %s\n", item.Completed.Format("2006-01-02"))
	}

	if path, ok := s.Path(id); ok {
		rel, err := filepath.Rel(cfg.Root(), path)
		if err == nil {
			path = rel
		}
		fmt.Printf("  File:     %s\n", path)
	}

	if item.Summary != "" {
		fmt.Printf("\n  Summary:\n    %s\n", item.Summary)
	}

	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("\n  Acceptance criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}

	if len(item.NextActions) > 0 {
		fmt.Println("\n  Next actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	return 0
}

// --- Helpers ---

func findUATPending(s *store.Store, cfg *config.Config) []*model.Item {
	var pending []*model.Item
	for _, item := range s.All() {
		stage := deliveryStage(item)
		if stage == "" {
			continue
		}
		if stage == "deployed_dev" || stage == "smoke_passed" {
			pending = append(pending, item)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	return pending
}

func deliveryStage(item *model.Item) string {
	if stage, ok := item.Delivery["stage"]; ok {
		if s, ok := stage.(string); ok && s != "" && s != "null" {
			return s
		}
	}
	return ""
}

func isStartStatus(item *model.Item, cfg *config.Config) bool {
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	return item.Status == tc.StartStatus
}

func isTerminal(item *model.Item, cfg *config.Config) bool {
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	for _, ts := range tc.TerminalStatuses {
		if item.Status == ts {
			return true
		}
	}
	return false
}

func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "normal":
		return 3
	case "low":
		return 4
	default:
		return 5
	}
}

func sevAbbrev(sev string) string {
	switch sev {
	case "critical":
		return "crit"
	case "high":
		return "hig"
	case "medium":
		return "med"
	case "normal":
		return "norm"
	case "low":
		return "low"
	default:
		return sev
	}
}

func sevLabel(sev string) string {
	switch sev {
	case "critical":
		return fmt.Sprintf("%sCRIT%s", cRed, cReset)
	case "high":
		return fmt.Sprintf("%sHIG %s", cYellow, cReset)
	case "medium":
		return fmt.Sprintf("%sMED %s", cYellow, cReset)
	case "normal":
		return fmt.Sprintf("%sNORM%s", cYellow, cReset)
	case "low":
		return fmt.Sprintf("%sLOW %s", cDim, cReset)
	default:
		return fmt.Sprintf("%-4s", sev)
	}
}

func workStatus(item *model.Item) string {
	if pr, ok := item.WorkTracking["pr"]; ok {
		if ps, ok := pr.(string); ok && ps != "" && ps != "null" && ps != "[]" {
			return fmt.Sprintf("[%sPR%s]", cGreen, cReset)
		}
	}
	if branch, ok := item.WorkTracking["branch"]; ok {
		if bs, ok := branch.(string); ok && bs != "" && bs != "null" {
			return fmt.Sprintf("[%sbranch%s]", cCyan, cReset)
		}
	}
	return fmt.Sprintf("[%sno work%s]", cDim, cReset)
}

func severityColor(sev string) string {
	switch sev {
	case "critical":
		return cRed
	case "high":
		return cYellow
	default:
		return ""
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
