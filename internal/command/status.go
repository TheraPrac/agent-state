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
	"github.com/jfinlinson/agent-state/internal/store"
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
	// Single-entity mode
	if id != "" {
		return statusSingle(s, cfg, id)
	}

	// Check mode
	if opts.Check {
		return Check(s, cfg, false, false)
	}

	// Expand all sections if --all
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
	fmt.Printf("\033[1m\033[37m%s Agent State\033[0m  ", cfg.Project.Name)
	fmt.Printf("\033[32m%d active\033[0m  ", activeCount)
	fmt.Printf("\033[31m%d issues\033[0m  ", issueCount)
	fmt.Printf("\033[36m%d queued\033[0m  ", queuedCount)
	fmt.Printf("\033[2m%d archived\033[0m\n\n", archivedCount)

	// Active work
	active := s.List(store.StatusFilter("active"))
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	fmt.Println("\033[1m\033[37m━━━ ACTIVE WORK ━━━\033[0m")
	if len(active) == 0 {
		fmt.Println("  \033[2m(none)\033[0m")
	} else {
		for _, item := range active {
			stage := deliveryStage(item)
			assigned := ""
			if item.AssignedTo != "" {
				assigned = fmt.Sprintf("  [%s]", item.AssignedTo)
			}
			fmt.Printf("  \033[1m%-8s\033[0m %s", item.ID, item.Title)
			if stage != "" {
				fmt.Printf("  (%s)", stage)
			}
			fmt.Print(assigned)
			fmt.Println()
		}
	}
	fmt.Println()

	// Pending UAT
	uatPending := findUATPending(s, cfg)
	if len(uatPending) > 0 {
		fmt.Println("\033[1m\033[37m━━━ PENDING UAT ━━━\033[0m")
		fmt.Println("  \033[35mItems below need manual UAT on DEV before closure\033[0m")
		fmt.Println()
		for _, item := range uatPending {
			stage := deliveryStage(item)
			deployed := ""
			if d, ok := item.Delivery["deployed_at"]; ok {
				if ds, ok := d.(string); ok && ds != "" {
					deployed = fmt.Sprintf("  (deployed %s)", ds[:10])
				}
			}
			fmt.Printf("  \033[35m▶ on DEV\033[0m  \033[1m%-8s\033[0m %s", item.ID, truncate(item.Title, 60))
			if stage != "" {
				fmt.Printf("  (%s)", stage)
			}
			fmt.Print(deployed)
			fmt.Println()
		}
		fmt.Println()
	}

	// Issues section
	if opts.Issues {
		openIssues := s.List(store.TypeFilter("issue"), store.StatusFilter("open"))
		sort.Slice(openIssues, func(i, j int) bool {
			return severityRank(openIssues[i].Severity) < severityRank(openIssues[j].Severity)
		})

		fmt.Println("\033[1m\033[37m━━━ OPEN ISSUES ━━━\033[0m")
		for _, item := range openIssues {
			sev := item.Severity
			if sev == "" {
				sev = "medium"
			}
			sevColor := severityColor(sev)
			fmt.Printf("  %s%-8s\033[0m  [%s]  %s\n", sevColor, item.ID, sev, item.Title)
		}
		fmt.Println()
	}

	// Tasks section
	if opts.Tasks {
		queuedTasks := s.List(store.TypeFilter("task"), store.StatusFilter("queued"))
		sort.Slice(queuedTasks, func(i, j int) bool {
			pi, pj := priorityOf(queuedTasks[i]), priorityOf(queuedTasks[j])
			if pi != pj {
				return pi < pj
			}
			return queuedTasks[i].ID < queuedTasks[j].ID
		})

		fmt.Println("\033[1m\033[37m━━━ QUEUED TASKS ━━━\033[0m")
		for _, item := range queuedTasks {
			p := priorityOf(item)
			blocked := ""
			if g.IsBlocked(item.ID) {
				unresolved := g.UnresolvedDeps(item.ID)
				blocked = fmt.Sprintf("  \033[2m(blocked by %s)\033[0m", strings.Join(unresolved, ", "))
			}
			fmt.Printf("  %-8s  p%d  %s%s\n", item.ID, p, item.Title, blocked)
		}
		fmt.Println()
	}

	// Recent closures
	if opts.Recent {
		fmt.Println("\033[1m\033[37m━━━ RECENTLY CLOSED (7d) ━━━\033[0m")
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
			fmt.Println("  \033[2m(none)\033[0m")
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

	// Completed/archived items
	if opts.Completed {
		fmt.Println("\033[1m\033[37m━━━ COMPLETED ━━━\033[0m")
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

	// Summary footer (only when sections are collapsed)
	if !opts.Issues && !opts.Tasks && !opts.Recent {
		// Count by severity
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
				sevSummary += fmt.Sprintf("  \033[33m%d %s\033[0m", n, sev[:3])
			}
		}

		// Count recent
		cutoff := time.Now().AddDate(0, 0, -7)
		var recentCount int
		for _, item := range s.All() {
			if isTerminal(item, cfg) && item.Completed != nil && item.Completed.After(cutoff) {
				recentCount++
			}
		}

		fmt.Printf("  \033[1mIssues:\033[0m %d open%s  \033[2m(status -i)\033[0m\n", issueCount, sevSummary)
		fmt.Printf("  \033[1mTasks:\033[0m  %d queued  \033[2m(status -t)\033[0m\n", queuedCount)
		fmt.Printf("  \033[1mRecent:\033[0m %d closed (7d)  \033[2m(status -r)\033[0m\n", recentCount)
	}

	return 0
}

func statusSingle(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// Full detailed view
	fmt.Printf("\033[1m%s\033[0m — %s\n", item.ID, item.Title)
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

	// Delivery info
	stage := deliveryStage(item)
	if stage != "" {
		fmt.Printf("  Stage:    %s\n", stage)
	}

	// Work tracking
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

	// Dependencies
	if len(item.DependsOn) > 0 {
		fmt.Printf("  Depends:  %s\n", strings.Join(item.DependsOn, ", "))
	}

	// Computed blocks
	g := deps.Build(s.All(), cfg)
	blocks := g.BlocksItems(id)
	if len(blocks) > 0 {
		fmt.Printf("  Blocks:   %s\n", strings.Join(blocks, ", "))
	}

	if len(item.Tags) > 0 {
		fmt.Printf("  Tags:     %s\n", strings.Join(item.Tags, ", "))
	}

	// Timestamps
	fmt.Printf("  Created:  %s\n", item.Created.Format("2006-01-02"))
	fmt.Printf("  Touched:  %s\n", item.LastTouched.Format("2006-01-02"))
	if item.Completed != nil {
		fmt.Printf("  Completed: %s\n", item.Completed.Format("2006-01-02"))
	}

	// File path
	if path, ok := s.Path(id); ok {
		rel, err := filepath.Rel(cfg.Root(), path)
		if err == nil {
			path = rel
		}
		fmt.Printf("  File:     %s\n", path)
	}

	// Summary
	if item.Summary != "" {
		fmt.Printf("\n  Summary:\n    %s\n", item.Summary)
	}

	// Acceptance criteria
	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("\n  Acceptance criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}

	// Next actions
	if len(item.NextActions) > 0 {
		fmt.Println("\n  Next actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	return 0
}

func findUATPending(s *store.Store, cfg *config.Config) []*model.Item {
	var pending []*model.Item
	for _, item := range s.All() {
		stage := deliveryStage(item)
		if stage == "" {
			continue
		}
		// Items deployed to dev but not yet UAT approved
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
	case "low":
		return 3
	default:
		return 4
	}
}

func severityColor(sev string) string {
	switch sev {
	case "critical":
		return "\033[31m" // red
	case "high":
		return "\033[33m" // yellow
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
