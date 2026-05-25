package command

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)


// GoalCreate creates a new goal with the given title and weight.
func GoalCreate(s *store.Store, cfg *config.Config, title string, weight int) int {
	if weight <= 0 || weight > 100 {
		fmt.Fprintf(os.Stderr, "goal create: --weight must be 1-100 (got %d)\n", weight)
		return 2
	}

	id, err := s.NextID("goal")
	if err != nil {
		fmt.Fprintf(os.Stderr, "allocating ID: %v\n", err)
		return 1
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	doc := &model.ParsedDocument{}
	lines := []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id},
		{Raw: "type: goal", Key: "type", Value: "goal"},
		{Raw: "status: draft", Key: "status", Value: "draft"},
		{Raw: "created: " + nowStr, Key: "created", Value: nowStr},
		{Raw: "last_touched: " + nowStr, Key: "last_touched", Value: nowStr},
		{Raw: ""},
		{Raw: "completed: null", Key: "completed", Value: "null"},
		{Raw: ""},
	}

	titleLine := "title: " + title
	if strings.ContainsAny(title, ":`\"") {
		titleLine = fmt.Sprintf("title: %q", title)
	}
	lines = append(lines,
		model.Line{Raw: titleLine, Key: "title", Value: title},
		model.Line{Raw: ""},
		model.Line{Raw: fmt.Sprintf("weight: %d", weight), Key: "weight", Value: fmt.Sprintf("%d", weight)},
		model.Line{Raw: "must_do:", Key: "must_do"},
		model.Line{Raw: ""},
		model.Line{Raw: "sbar:", Key: "sbar"},
	)
	for _, key := range []string{"situation", "background", "assessment", "recommendation"} {
		lines = append(lines,
			model.Line{Raw: "  " + key + ": |-", Key: key, Indent: 2, BlockKey: "sbar"},
			model.Line{Raw: "    " + model.SBARPlaceholders[key], IsBlock: true, BlockKey: key, Indent: 4},
		)
	}
	doc.Lines = lines

	w := weight
	item := &model.Item{
		ID:              id,
		Type:            "goal",
		Status:          "draft",
		Title:           title,
		Created:         now,
		LastTouched:     now,
		Weight:          &w,
		WorkTracking:    make(map[string]interface{}),
		Delivery:        make(map[string]interface{}),
		TestingEvidence: make(map[string]interface{}),
		TimeTracking:    make(map[string]interface{}),
		Manifest:        make(map[string]interface{}),
		Doc:             doc,
	}

	if err := s.Create(item); err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("Created goal %s — %s (weight %d, status draft)\n", id, title, weight)
	return 0
}

// GoalActivate transitions a goal from draft to active, enforcing the ≤100 weight sum.
func GoalActivate(s *store.Store, cfg *config.Config, id string) int {
	goal, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "%s is not a goal\n", id)
		return 1
	}
	if goal.Status != "draft" {
		fmt.Fprintf(os.Stderr, "goal activate: %s is %s (must be draft)\n", id, goal.Status)
		return 1
	}

	// Weight check and status update are combined in the Mutate closure so
	// they are atomic within a single process. Multi-process TOCTOU is
	// theoretical for this CLI tool (sequential single-agent usage); a
	// coordinating lock file would be needed for full protection.
	var finalSum int
	if err := s.Mutate(id, func(it *model.Item) error {
		sum := 0
		for _, g := range s.All() {
			if g.Type == "goal" && g.Status == "active" && g.ID != id && g.Weight != nil {
				sum += *g.Weight
			}
		}
		w := 0
		if it.Weight != nil {
			w = *it.Weight
		}
		if sum+w > 100 {
			return fmt.Errorf("active weight sum would be %d/100 (current active=%d, this=%d); reduce another goal's weight first", sum+w, sum, w)
		}
		finalSum = sum + w
		it.Status = "active"
		it.Doc.SetField("status", "active")
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "goal activate: %s: %v\n", id, err)
		return 1
	}
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("%s activated (active weight: %d/100)\n", id, finalSum)
	return 0
}

// GoalMarkMet transitions a goal from active to met (terminal).
func GoalMarkMet(s *store.Store, cfg *config.Config, id string) int {
	goal, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "%s is not a goal\n", id)
		return 1
	}
	if goal.Status != "active" {
		fmt.Fprintf(os.Stderr, "goal mark-met: %s is %s (must be active)\n", id, goal.Status)
		return 1
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Status = "met"
		it.Doc.SetField("status", "met")
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "marking %s met: %v\n", id, err)
		return 1
	}
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("%s marked met\n", id)
	return 0
}

// GoalDrop transitions a goal to dropped with an enumerated reason.
func GoalDrop(s *store.Store, cfg *config.Config, id, reason string) int {
	if !model.IsValidDropReason(reason) {
		fmt.Fprintf(os.Stderr,
			"goal drop: --reason %q not valid; must be one of: %s\n",
			reason, model.ValidDropReasonsJoined())
		return 2
	}

	goal, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "%s is not a goal\n", id)
		return 1
	}
	if goal.Status != "active" {
		fmt.Fprintf(os.Stderr, "goal drop: %s is %s (must be active)\n", id, goal.Status)
		return 1
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Status = "dropped"
		it.Doc.SetField("status", "dropped")
		it.SetNested("delivery", "dropped_reason", reason)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "dropping %s: %v\n", id, err)
		return 1
	}
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("%s dropped (%s)\n", id, reason)
	return 0
}

// GoalList prints all goals grouped by lifecycle bucket.
func GoalList(s *store.Store, cfg *config.Config) int {
	return goalListTo(os.Stdout, s, cfg)
}

// goalListTo renders the goal list to w. Used by GoalList (to os.Stdout) and tests.
func goalListTo(w io.Writer, s *store.Store, cfg *config.Config) int {
	all := s.List(store.TypeFilter("goal"))
	if len(all) == 0 {
		fmt.Fprintln(w, "(no goals)")
		return 0
	}

	buckets := map[string][]*model.Item{
		"draft":    {},
		"active":   {},
		"met":      {},
		"dropped":  {},
		"archived": {},
	}
	for _, g := range all {
		if _, ok := buckets[g.Status]; ok {
			buckets[g.Status] = append(buckets[g.Status], g)
		}
	}

	order := []string{"active", "draft", "met", "dropped", "archived"}
	for _, status := range order {
		goals := buckets[status]
		if len(goals) == 0 {
			continue
		}
		sort.Slice(goals, func(i, j int) bool { return goals[i].ID < goals[j].ID })
		fmt.Fprintf(w, "\n%s:\n", strings.ToUpper(status))
		for _, g := range goals {
			wt := "—"
			if g.Weight != nil {
				wt = fmt.Sprintf("%d", *g.Weight)
			}
			fmt.Fprintf(w, "  %-6s  wt:%-3s  %s\n", g.ID, wt, g.Title)
		}
	}

	// Active weight sum footer.
	activeSum := 0
	for _, g := range buckets["active"] {
		if g.Weight != nil {
			activeSum += *g.Weight
		}
	}
	fmt.Fprintf(w, "\nactive weight: %d / 100\n", activeSum)
	return 0
}
