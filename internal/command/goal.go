package command

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)


// GoalCreateOpts holds optional flags for st goal create.
type GoalCreateOpts struct {
	SuccessCriterion string
	NoValidate       bool
}

// GoalCreate creates a new goal with the given title and weight.
func GoalCreate(s *store.Store, cfg *config.Config, title string, weight int, opts GoalCreateOpts) int {
	if weight <= 0 || weight > 100 {
		fmt.Fprintf(os.Stderr, "goal create: --weight must be 1-100 (got %d)\n", weight)
		return 2
	}

	if opts.SuccessCriterion == "" && !opts.NoValidate {
		fmt.Fprintf(os.Stderr, "goal create: --success-criterion is required (use --no-validate to skip)\n")
		return 2
	}

	w := weight
	var allocatedGoalID string

	createdGoal, err := s.AllocateAndCreate("goal", func(id string) (*model.Item, error) {
		allocatedGoalID = id
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
			model.Line{Raw: ""},
		)
		if opts.SuccessCriterion != "" {
			criterionLine := "success_criterion: " + opts.SuccessCriterion
			if strings.ContainsAny(opts.SuccessCriterion, ":`\"#") {
				criterionLine = fmt.Sprintf("success_criterion: %q", opts.SuccessCriterion)
			}
			lines = append(lines,
				model.Line{Raw: criterionLine, Key: "success_criterion", Value: opts.SuccessCriterion},
				model.Line{Raw: ""},
			)
		}
		lines = append(lines, model.Line{Raw: "sbar:", Key: "sbar"})
		for _, key := range []string{"situation", "background", "assessment", "recommendation"} {
			lines = append(lines,
				model.Line{Raw: "  " + key + ": |-", Key: key, Indent: 2, BlockKey: "sbar"},
				model.Line{Raw: "    " + model.SBARPlaceholders[key], IsBlock: true, BlockKey: key, Indent: 4},
			)
		}
		doc.Lines = lines

		return &model.Item{
			ID:               id,
			Type:             "goal",
			Status:           "draft",
			Title:            title,
			Created:          now,
			LastTouched:      now,
			Weight:           &w,
			SuccessCriterion: opts.SuccessCriterion,
			WorkTracking:     make(map[string]interface{}),
			Delivery:         make(map[string]interface{}),
			TestingEvidence:  make(map[string]interface{}),
			TimeTracking:     make(map[string]interface{}),
			Manifest:         make(map[string]interface{}),
			Doc:              doc,
		}, nil
	})
	if err != nil {
		if allocatedGoalID != "" {
			fmt.Fprintf(os.Stderr, "creating %s: %v\n", allocatedGoalID, err)
		} else {
			fmt.Fprintf(os.Stderr, "creating goal: %v\n", err)
		}
		return 1
	}

	fmt.Printf("Created goal %s — %s (weight %d, status draft)\n", createdGoal.ID, title, weight)
	return 0
}

// activeGoalWeightSumExcluding returns the summed weight of every ACTIVE goal
// except excludeID. Shared by GoalActivate and CheckGoalWeightSum so the
// weight-budget logic has a single definition.
func activeGoalWeightSumExcluding(s *store.Store, excludeID string) int {
	sum := 0
	for _, g := range s.All() {
		if g.Type == "goal" && g.Status == "active" && g.ID != excludeID && g.Weight != nil {
			sum += *g.Weight
		}
	}
	return sum
}

// CheckGoalWeightSum verifies that setting goal `id` to newWeight would not push
// the sum of ALL active goals' weights above 100. The target goal is excluded
// from the existing-sum tally (newWeight is the proposed value for it). Returns
// the existing active-weight sum (excluding `id`) and a nil error when within
// budget, otherwise the same sum and a descriptive error. Returning the sum
// lets callers (GoalActivate) reuse it without a second store scan. Reused by
// both `goal activate` and the `st update` / batch weight-write paths so the
// ≤100 invariant is enforced at every active-goal weight mutation.
func CheckGoalWeightSum(s *store.Store, id string, newWeight int) (int, error) {
	sum := activeGoalWeightSumExcluding(s, id)
	if sum+newWeight > 100 {
		return sum, fmt.Errorf("active weight sum would be %d/100 (current active=%d, this=%d); reduce another goal's weight first", sum+newWeight, sum, newWeight)
	}
	return sum, nil
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
		w := 0
		if it.Weight != nil {
			w = *it.Weight
		}
		sum, err := CheckGoalWeightSum(s, id, w)
		if err != nil {
			return err
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

// redistributeGoalWeight proportionally adds closingWeight to all remaining
// active goals (excluding closingID). Peers are mutated in-place; the last
// peer absorbs any integer remainder so the total is exact. No-op when there
// are no active peers or their total weight is zero.
func redistributeGoalWeight(s *store.Store, cfg *config.Config, closingID string, closingWeight int) {
	if closingWeight <= 0 {
		return
	}

	var peers []*model.Item
	totalPeerWeight := 0
	for _, g := range s.All() {
		if g.Type == "goal" && g.Status == "active" && g.ID != closingID && g.Weight != nil && *g.Weight > 0 {
			peers = append(peers, g)
			totalPeerWeight += *g.Weight
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })

	if len(peers) == 0 || totalPeerWeight == 0 {
		fmt.Printf("note: no active peers to redistribute %d weight from %s\n", closingWeight, closingID)
		return
	}

	distributed := 0
	updated := 0
	for i, peer := range peers {
		var delta int
		if i == len(peers)-1 {
			delta = closingWeight - distributed
		} else {
			delta = closingWeight * (*peer.Weight) / totalPeerWeight
		}
		// Always advance distributed before the mutation attempt so that a
		// failed Mutate does not cause the last peer to absorb extra weight
		// beyond its proportional integer remainder.
		distributed += delta
		if delta == 0 {
			continue
		}
		peerID := peer.ID
		oldW := *peer.Weight
		newW := oldW + delta
		if err := s.Mutate(peerID, func(it *model.Item) error {
			it.Weight = &newW
			it.Doc.SetField("weight", fmt.Sprintf("%d", newW))
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: updating weight for %s: %v\n", peerID, err)
			continue
		}
		_ = changelog.Append(cfg, peerID, changelog.Entry{
			Op: "goal_weight_redistributed", Field: "weight",
			OldValue: fmt.Sprintf("%d", oldW), NewValue: fmt.Sprintf("%d", newW),
			Reason: fmt.Sprintf("redistributed from %s (closed)", closingID),
		})
		fmt.Printf("  %s: wt %d → %d (+%d)\n", peerID, oldW, newW, delta)
		updated++
	}
	fmt.Printf("redistributed %d weight from %s to %d goal(s)\n", closingWeight, closingID, updated)
}

// GoalMarkMetOpts holds optional flags for st goal mark-met.
type GoalMarkMetOpts struct {
	NoValidate bool
}

// GoalMarkMet transitions a goal from active to met (terminal).
func GoalMarkMet(s *store.Store, cfg *config.Config, id string, opts GoalMarkMetOpts) int {
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
	if goal.SuccessCriterion == "" && !opts.NoValidate {
		fmt.Fprintf(os.Stderr, "goal mark-met: %s has no success_criterion — set one first or use --no-validate\n", id)
		return 2
	}

	closingWeight := 0
	if goal.Weight != nil {
		closingWeight = *goal.Weight
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

	redistributeGoalWeight(s, cfg, id, closingWeight)

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op: "goal_mark_met", Field: "status",
		OldValue: "active", NewValue: "met",
	})
	if opts.NoValidate {
		_ = changelog.Append(cfg, id, changelog.Entry{
			Op:     "goal_mark_met_no_validate",
			Reason: "bypassed success_criterion gate via --no-validate",
		})
	}

	fmt.Printf("%s marked met\n", id)
	if cleared, err := agent.ClearGoalFocusForAllAgents(cfg, id); err != nil {
		fmt.Fprintf(os.Stderr, "warning: clearing goal focus: %v\n", err)
	} else if len(cleared) > 0 {
		fmt.Printf("cleared goal focus for: %s\n", strings.Join(cleared, ", "))
	}
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

	closingWeight := 0
	if goal.Weight != nil {
		closingWeight = *goal.Weight
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

	redistributeGoalWeight(s, cfg, id, closingWeight)

	fmt.Printf("%s dropped (%s)\n", id, reason)
	if cleared, err := agent.ClearGoalFocusForAllAgents(cfg, id); err != nil {
		fmt.Fprintf(os.Stderr, "warning: clearing goal focus: %v\n", err)
	} else if len(cleared) > 0 {
		fmt.Printf("cleared goal focus for: %s\n", strings.Join(cleared, ", "))
	}
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
			crit := ""
			if status == "active" && g.SuccessCriterion == "" {
				crit = "  ⚠ no success_criterion"
			}
			fmt.Fprintf(w, "  %-6s  wt:%-3s  %s%s\n", g.ID, wt, g.Title, crit)
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
