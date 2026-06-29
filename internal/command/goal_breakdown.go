package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/coordinator"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// GoalBreakdownOpts are the flags for st goal breakdown.
type GoalBreakdownOpts struct {
	Top  int  // max items per goal; <=0 defaults to 3
	JSON bool // machine-readable output
}

// goalBreakdownGoalJSON is the stable machine contract for one goal's section.
// Field names are part of the T-348 contract — additive changes only.
type goalBreakdownGoalJSON struct {
	GoalID   string          `json:"goal_id"`
	Title    string          `json:"title"`
	Weight   int             `json:"weight"`
	Items    []recommendJSON `json:"items"`
	PeerNote string          `json:"peer_note,omitempty"`
}

// GoalBreakdown prints the top-N workable items for each active goal,
// grouped by goal and sorted within each group by the recommend scorer.
func GoalBreakdown(s *store.Store, cfg *config.Config, opts GoalBreakdownOpts) int {
	return goalBreakdownTo(os.Stdout, s, cfg, opts)
}

// goalBreakdownTo is the io.Writer-parameterised implementation.
func goalBreakdownTo(w io.Writer, s *store.Store, cfg *config.Config, opts GoalBreakdownOpts) int {
	top := opts.Top
	if top <= 0 {
		top = 3
	}

	// Collect active goals sorted by weight descending.
	allGoals := s.List(store.TypeFilter("goal"))
	active := allGoals[:0:len(allGoals)]
	for _, g := range allGoals {
		if g.Status == "active" {
			active = append(active, g)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		wi, wj := 0, 0
		if active[i].Weight != nil {
			wi = *active[i].Weight
		}
		if active[j].Weight != nil {
			wj = *active[j].Weight
		}
		return wi > wj
	})

	if len(active) == 0 {
		fmt.Fprintln(w, "No active goals.")
		return 0
	}

	// Build shared infrastructure once — same pipeline as recommendTo.
	g := deps.Build(s.All(), cfg)
	sprints := loadSprintInfo(cfg, g)
	pins := loadQueuePins(cfg)
	goalWeights := loadGoalWeights(s)
	now := time.Now()

	if opts.JSON {
		return goalBreakdownJSON(w, s, cfg, active, g, sprints, pins, goalWeights, now, top)
	}
	return goalBreakdownText(w, s, cfg, active, g, sprints, pins, goalWeights, now, top)
}

func goalBreakdownText(w io.Writer, s *store.Store, cfg *config.Config,
	goals []*model.Item, g *deps.Graph,
	sprints map[string]coordinator.SprintInfo,
	pins map[string]bool, goalWeights map[string]float64,
	now time.Time, top int) int {

	for _, goal := range goals {
		wt := 0
		if goal.Weight != nil {
			wt = *goal.Weight
		}
		fmt.Fprintf(w, "\n%s  wt:%d  %s\n", goal.ID, wt, goal.Title)

		cands := recommendCandidates(s, cfg, g, RecommendOpts{Goal: goal.ID}, sprints)
		if len(cands) == 0 {
			fmt.Fprintln(w, "  (no workable items)")
			// I-1657: surface peer-assigned items so the operator knows why
			// this goal has no candidates, matching the recommendTo pattern (I-1435).
			if peers := peerAssignedReady(g, cfg.AgentID()); len(peers) > 0 {
				printPeerNote(w, peers)
			}
			continue
		}

		leverage, names := unblockLeverage(g, cands)
		overrides := buildPriorityOverrides(g, cands, pins)
		recs := coordinator.Recommend(cands, leverage, sprints, goalWeights, overrides, now, pins)
		enrichUnblockDetail(recs, names)
		enrichPriorityDetail(recs, overrides, g.Items, pins)

		if len(recs) > top {
			recs = recs[:top]
		}
		for _, r := range recs {
			fmt.Fprintf(w, "  %-8s p%d  %s — %s\n", r.Item.ID, r.Priority, r.Item.Title, r.Rationale())
		}
	}
	fmt.Fprintln(w)
	return 0
}

func goalBreakdownJSON(w io.Writer, s *store.Store, cfg *config.Config,
	goals []*model.Item, g *deps.Graph,
	sprints map[string]coordinator.SprintInfo,
	pins map[string]bool, goalWeights map[string]float64,
	now time.Time, top int) int {

	out := make([]goalBreakdownGoalJSON, 0, len(goals))
	for _, goal := range goals {
		wt := 0
		if goal.Weight != nil {
			wt = *goal.Weight
		}

		cands := recommendCandidates(s, cfg, g, RecommendOpts{Goal: goal.ID}, sprints)
		if len(cands) == 0 {
			// I-1657: short-circuit — skip scoring pipeline and emit empty items,
			// matching the goalBreakdownText guard. Surface peer note if applicable.
			entry := goalBreakdownGoalJSON{
				GoalID: goal.ID, Title: goal.Title, Weight: wt,
				Items: []recommendJSON{},
			}
			if peers := peerAssignedReady(g, cfg.AgentID()); len(peers) > 0 {
				parts := make([]string, 0, len(peers))
				for _, p := range peers {
					parts = append(parts, fmt.Sprintf("%s [%s]", p.ID, p.AssignedTo))
				}
				entry.PeerNote = fmt.Sprintf("(%d item(s) not shown — assigned to peers: %s)",
					len(peers), strings.Join(parts, ", "))
			}
			out = append(out, entry)
			continue
		}

		leverage, names := unblockLeverage(g, cands)
		overrides := buildPriorityOverrides(g, cands, pins)
		recs := coordinator.Recommend(cands, leverage, sprints, goalWeights, overrides, now, pins)
		enrichUnblockDetail(recs, names)
		enrichPriorityDetail(recs, overrides, g.Items, pins)

		if len(recs) > top {
			recs = recs[:top]
		}

		items := make([]recommendJSON, 0, len(recs))
		for _, r := range recs {
			fjs := make([]factorJSON, 0, len(r.Factors))
			for _, f := range r.Factors {
				fjs = append(fjs, factorJSON{Name: f.Name, Points: f.Points, Detail: f.Detail})
			}
			own := r.Item.ResolvedPriority()
			jrec := recommendJSON{
				ID: r.Item.ID, Title: r.Item.Title, Priority: own,
				Score: r.Score, Rationale: r.Rationale(), Factors: fjs,
			}
			if r.Priority != own {
				eff := r.Priority
				jrec.EffectivePriority = &eff
			}
			items = append(items, jrec)
		}

		out = append(out, goalBreakdownGoalJSON{
			GoalID: goal.ID, Title: goal.Title, Weight: wt, Items: items,
		})
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(w, "[]")
		return 1
	}
	fmt.Fprintln(w, string(b))
	return 0
}
