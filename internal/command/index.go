package command

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Index(s *store.Store, cfg *config.Config) int {
	g := deps.Build(s.All(), cfg)
	reg, _ := registry.Load(cfg.EpicsPath())
	noteReg, _ := registry.Load(cfg.NotesPath())

	var b strings.Builder
	b.WriteString("# Agent State Index\n")
	b.WriteString("generated: auto\n\n")

	writeActiveWork(&b, s)
	writeQueuedTasks(&b, s, cfg, g, reg)
	writeBlocked(&b, s, cfg, g)
	writeOpenIssues(&b, s)
	writePendingUAT(&b, s, cfg)
	writeCompleted(&b, s, cfg)
	writeNotes(&b, noteReg)

	indexPath := cfg.IndexPath()
	if err := os.WriteFile(indexPath, []byte(b.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing index: %v\n", err)
		return 1
	}

	fmt.Printf("Generated %s (%d items)\n", indexPath, len(s.All()))
	return 0
}

func writeActiveWork(b *strings.Builder, s *store.Store) {
	active := s.List(store.StatusFilter("active"))
	b.WriteString("## Active Work\n")
	if len(active) == 0 {
		b.WriteString("(none)\n")
	}
	for _, item := range active {
		line := fmt.Sprintf("- %s — %s", item.ID, item.Title)
		if label := formatAssignment(item); label != "" {
			line += fmt.Sprintf(" [%s]", label)
		}
		if stage := deliveryStage(item); stage != "" {
			line += fmt.Sprintf(" stage: %s", stage)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

// itemGroup represents a bucket in the epic→sprint→tag hierarchy.
type itemGroup struct {
	EpicID    string
	EpicTitle string
	SprintID  string
	SprintTitle string
	Tag       string
	Items     []*model.Item
}

func writeQueuedTasks(b *strings.Builder, s *store.Store, cfg *config.Config, g *deps.Graph, reg *registry.Registry) {
	// Collect queued, unblocked tasks
	var queued []*model.Item
	for typeName := range cfg.Types {
		tc := cfg.Types[typeName]
		items := s.List(store.TypeFilter(typeName), store.StatusFilter(tc.StartStatus))
		for _, item := range items {
			if !g.IsBlocked(item.ID) {
				queued = append(queued, item)
			}
		}
	}
	if len(queued) == 0 {
		return
	}

	// Build groups: epic → sprint → tag
	groups := buildGroups(queued, reg)

	b.WriteString("## Queued Tasks\n")

	// Track current epic/sprint to emit headers
	currentEpic := ""
	currentSprint := ""
	for _, grp := range groups {
		if grp.EpicID != currentEpic {
			currentEpic = grp.EpicID
			currentSprint = ""
			if grp.EpicID != "" {
				b.WriteString(fmt.Sprintf("### Epic: %s — %s\n", grp.EpicID, grp.EpicTitle))
			}
		}
		if grp.SprintID != currentSprint {
			currentSprint = grp.SprintID
			if grp.SprintID != "" {
				prefix := "####"
				if grp.EpicID == "" {
					prefix = "###"
				}
				b.WriteString(fmt.Sprintf("%s Sprint: %s — %s\n", prefix, grp.SprintID, grp.SprintTitle))
			}
		}

		// Tag header
		if grp.Tag != "" {
			depth := "###"
			if grp.EpicID != "" && grp.SprintID != "" {
				depth = "#####"
			} else if grp.EpicID != "" || grp.SprintID != "" {
				depth = "####"
			}
			b.WriteString(fmt.Sprintf("%s [%s]\n", depth, grp.Tag))
		}

		for _, item := range grp.Items {
			p := priorityOf(item)
			b.WriteString(fmt.Sprintf("- %s — %s (p%d)\n", item.ID, item.Title, p))
		}
	}
	b.WriteString("\n")
}

func buildGroups(items []*model.Item, reg *registry.Registry) []itemGroup {
	// Key: "epicID|sprintID|tag"
	type groupKey struct {
		Epic, Sprint, Tag string
	}

	groupMap := make(map[groupKey]*itemGroup)
	var keys []groupKey

	for _, item := range items {
		epic := item.Epic
		sprint := item.Sprint
		tag := "uncategorized"
		if len(item.Tags) > 0 {
			tag = item.Tags[0]
		}

		k := groupKey{Epic: epic, Sprint: sprint, Tag: tag}
		if _, ok := groupMap[k]; !ok {
			epicTitle := ""
			if epic != "" && reg != nil {
				if e, ok := reg.GetEpic(epic); ok {
					epicTitle = e.Title
				}
			}
			sprintTitle := ""
			if sprint != "" && reg != nil {
				if s, ok := reg.GetSprint(sprint); ok {
					sprintTitle = s.Title
				}
			}
			groupMap[k] = &itemGroup{
				EpicID:      epic,
				EpicTitle:   epicTitle,
				SprintID:    sprint,
				SprintTitle: sprintTitle,
				Tag:         tag,
			}
			keys = append(keys, k)
		}
		groupMap[k].Items = append(groupMap[k].Items, item)
	}

	// Sort groups: epic title ASC → sprint title ASC → tag ASC
	// Items with epic before items without; "uncategorized" last
	sort.SliceStable(keys, func(i, j int) bool {
		gi, gj := groupMap[keys[i]], groupMap[keys[j]]
		// Epic ordering: items with epic first
		if (gi.EpicID != "") != (gj.EpicID != "") {
			return gi.EpicID != ""
		}
		if gi.EpicTitle != gj.EpicTitle {
			return gi.EpicTitle < gj.EpicTitle
		}
		// Sprint ordering: items with sprint first
		if (gi.SprintID != "") != (gj.SprintID != "") {
			return gi.SprintID != ""
		}
		if gi.SprintTitle != gj.SprintTitle {
			return gi.SprintTitle < gj.SprintTitle
		}
		// Tag ordering: "uncategorized" last
		if gi.Tag == "uncategorized" && gj.Tag != "uncategorized" {
			return false
		}
		if gi.Tag != "uncategorized" && gj.Tag == "uncategorized" {
			return true
		}
		return gi.Tag < gj.Tag
	})

	// Sort items within each group by priority then ID
	var result []itemGroup
	for _, k := range keys {
		grp := groupMap[k]
		sort.Slice(grp.Items, func(i, j int) bool {
			pi, pj := priorityOf(grp.Items[i]), priorityOf(grp.Items[j])
			if pi != pj {
				return pi < pj
			}
			return grp.Items[i].ID < grp.Items[j].ID
		})
		result = append(result, *grp)
	}
	return result
}

func writeBlocked(b *strings.Builder, s *store.Store, cfg *config.Config, g *deps.Graph) {
	var blocked []*model.Item
	for _, item := range s.All() {
		if isTerminal(item, cfg) {
			continue
		}
		if g.IsBlocked(item.ID) {
			blocked = append(blocked, item)
		}
	}
	if len(blocked) == 0 {
		return
	}

	sort.Slice(blocked, func(i, j int) bool { return blocked[i].ID < blocked[j].ID })

	b.WriteString("## Blocked\n")
	for _, item := range blocked {
		unresolved := g.UnresolvedDeps(item.ID)
		b.WriteString(fmt.Sprintf("- %s — %s (blocked by %s)\n", item.ID, item.Title, strings.Join(unresolved, ", ")))
	}
	b.WriteString("\n")
}

func writeOpenIssues(b *strings.Builder, s *store.Store) {
	issues := s.List(store.TypeFilter("issue"), store.StatusFilter("open"))
	if len(issues) == 0 {
		return
	}

	// Group by severity category
	type sevGroup struct {
		Label string
		Items []*model.Item
	}

	groups := map[string]*sevGroup{
		"p0": {Label: "p0 (blocking)"},
		"p1": {Label: "p1 (high)"},
		"p2": {Label: "p2 (medium)"},
		"p3": {Label: "p3 (deferred)"},
		"p4": {Label: "p4 (low)"},
	}
	order := []string{"p0", "p1", "p2", "p3", "p4"}

	// I-406: bucket issues by priority instead of the legacy severity
	// categories. Items missing priority bucket as p2 (medium).
	for _, item := range issues {
		key := "p2"
		if item.Priority != nil {
			key = fmt.Sprintf("p%d", *item.Priority)
		}
		if _, ok := groups[key]; !ok {
			key = "p2"
		}
		groups[key].Items = append(groups[key].Items, item)
	}

	b.WriteString("## Open Issues\n")
	for _, key := range order {
		grp := groups[key]
		if len(grp.Items) == 0 {
			continue
		}
		sort.Slice(grp.Items, func(i, j int) bool { return grp.Items[i].ID < grp.Items[j].ID })
		b.WriteString(fmt.Sprintf("### %s (%d)\n", grp.Label, len(grp.Items)))
		for _, item := range grp.Items {
			b.WriteString(fmt.Sprintf("- %s [%s] — %s\n", item.ID, key, item.Title))
		}
	}
	b.WriteString("\n")
}

func writePendingUAT(b *strings.Builder, s *store.Store, cfg *config.Config) {
	if cfg.Delivery == nil {
		return
	}

	pendingStages := map[string]bool{
		"merged": true, "deployed_dev": true, "smoke_passed": true,
	}

	var pending []*model.Item
	for _, item := range s.All() {
		stage := deliveryStage(item)
		if pendingStages[stage] && !isTerminal(item, cfg) {
			pending = append(pending, item)
		}
	}
	if len(pending) == 0 {
		return
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })

	b.WriteString("## Pending Deploy/UAT\n")
	for _, item := range pending {
		b.WriteString(fmt.Sprintf("- %s — %s (stage: %s)\n", item.ID, item.Title, deliveryStage(item)))
	}
	b.WriteString("\n")
}

func writeCompleted(b *strings.Builder, s *store.Store, cfg *config.Config) {
	var taskCount, issueCount int
	var ids []string
	for _, item := range s.All() {
		if isTerminal(item, cfg) {
			ids = append(ids, item.ID)
			switch item.Type {
			case "task":
				taskCount++
			case "issue":
				issueCount++
			}
		}
	}

	sort.Strings(ids)

	b.WriteString("## Completed\n")
	b.WriteString(fmt.Sprintf("%d tasks, %d issues archived\n", taskCount, issueCount))
	if len(ids) > 0 {
		b.WriteString(strings.Join(ids, ", ") + "\n")
	}
	b.WriteString("\n")
}

func writeNotes(b *strings.Builder, reg *registry.Registry) {
	if reg == nil {
		return
	}
	notes := reg.ListNotes(10)
	if len(notes) == 0 {
		return
	}

	b.WriteString("## Notes\n")
	// Show most recent first
	for i := len(notes) - 1; i >= 0; i-- {
		n := notes[i]
		ts := n.Timestamp.Format("2006-01-02")
		author := n.Author
		if author == "" {
			author = "unknown"
		}
		b.WriteString(fmt.Sprintf("### %s — %s\n", ts, author))
		b.WriteString(n.Message + "\n\n")
	}
}

func priorityOf(item *model.Item) int {
	if item.Priority != nil {
		return *item.Priority
	}
	return 2
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
