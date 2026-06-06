package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// recommend.go is the IMPERATIVE SHELL for `st recommend` (T-369): it
// gathers the graph/registry-derived inputs and hands them to the pure
// scorer (internal/coordinator.Recommend). It exists so dispatch is
// "with an inspectable rationale — never an opaque choice"
// (operating-contract §4.2): both this command and the coordinator's
// selectNext read the SAME scoring brain, surfaced as text.

// RecommendOpts are the `st recommend` flags.
type RecommendOpts struct {
	JSON  bool   // machine output (the future T-348 TUI panel consumer)
	Top   int    // max rows to print; <=0 ⇒ 10
	Scope string // "all" (default) | "sprint" (active-sprint members only)
	Queue bool   // accepted for backward compatibility; has no effect (T-461: candidates always derive from item properties)
	Brief bool   // one-line render: "<ID> p<N>  <title> — <rationale>"
	Goal  string // explicit goal filter (overrides agent focus_goal when set)
}

// recommendJSON is the STABLE machine contract (documented for the T-348
// TUI planning panel). Field names are part of that contract — additive
// changes only.
type recommendJSON struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Priority  int          `json:"priority"`
	Score     float64      `json:"score"`
	Rationale string       `json:"rationale"`
	Factors   []factorJSON `json:"factors"`
}

type factorJSON struct {
	Name   string  `json:"name"`
	Points float64 `json:"points"`
	Detail string  `json:"detail"`
}

// Recommend ranks workable items with an inspectable per-item "why".
// Public API preserved (cobra + existing tests stay green); the body
// delegates to recommendTo so T-372 can compose the same renderer into a
// TUI panel without duplicating logic (the §7 maintainability invariant).
func Recommend(s *store.Store, cfg *config.Config, opts RecommendOpts) int {
	return recommendTo(os.Stdout, s, cfg, opts)
}

// recommendTo is the io.Writer-parameterised implementation. The cobra
// path uses os.Stdout via Recommend; the TUI passes a bytes.Buffer.
func recommendTo(w io.Writer, s *store.Store, cfg *config.Config, opts RecommendOpts) int {
	top := opts.Top
	if top <= 0 {
		top = 10
	}
	g := deps.Build(s.All(), cfg)

	// Load sprint info ONCE and thread it through both the --scope filter
	// and the scorer (a single registry read, not one per concern).
	sprints := loadSprintInfo(cfg, g)
	cands := recommendCandidates(s, cfg, g, opts, sprints)
	leverage, names := unblockLeverage(g, cands)

	recs := coordinator.Recommend(cands, leverage, sprints, loadGoalWeights(s), loadQueuePins(cfg), time.Now())
	enrichUnblockDetail(recs, names)

	if len(recs) > top {
		recs = recs[:top]
	}

	if opts.JSON {
		out := make([]recommendJSON, 0, len(recs))
		for _, r := range recs {
			fjs := make([]factorJSON, 0, len(r.Factors))
			for _, f := range r.Factors {
				fjs = append(fjs, factorJSON{Name: f.Name, Points: f.Points, Detail: f.Detail})
			}
			out = append(out, recommendJSON{
				ID: r.Item.ID, Title: r.Item.Title, Priority: r.Priority,
				Score: r.Score, Rationale: r.Rationale(), Factors: fjs,
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

	if len(recs) == 0 {
		fmt.Fprintln(w, "No recommendable items (none workable in scope).")
		return 0
	}
	if opts.Brief && !opts.JSON {
		r := recs[0]
		fmt.Fprintf(w, "%-8s p%d  %s — %s\n", r.Item.ID, r.Priority, r.Item.Title, r.Rationale())
		return 0
	}
	for _, r := range recs {
		fmt.Fprintf(w, "%-8s p%d  %s\n", r.Item.ID, r.Priority, r.Item.Title)
		fmt.Fprintf(w, "      why: %s\n", r.Rationale())
	}
	return 0
}

// recommendCandidates resolves the candidate set from item properties:
// g.Ready() (unblocked + start-status + unassigned), further filtered to
// exclude items already claimed by a running session (ClaimedBy != "").
// queue.yaml is no longer the candidate source; it is an optional pin layer
// applied at scoring time via loadQueuePins (pinned items get a score boost
// but cannot leapfrog a strictly-higher-priority item).
//
// The Queue field on opts is accepted for backward compatibility but has no
// effect on the candidate set — both planning and dispatch views now derive
// candidates from item properties.
//
// --scope sprint further restricts to members of an ACTIVE sprint.
// --goal (or the calling agent's focus_goal when no explicit flag is set)
// restricts to items linked to that goal; if the focused goal is terminal
// or missing, the focus is auto-cleared and the global set is used instead.
func recommendCandidates(s *store.Store, cfg *config.Config, g *deps.Graph,
	opts RecommendOpts, sprints map[string]coordinator.SprintInfo) []*model.Item {

	ready := g.Ready()
	cands := ready[:0:len(ready)]
	for _, it := range ready {
		if it.ClaimedBy != "" {
			continue
		}
		cands = append(cands, it)
	}

	if opts.Scope == "sprint" {
		filtered := cands[:0]
		for _, it := range cands {
			if it.Sprint == "" {
				continue
			}
			if si, ok := sprints[it.Sprint]; ok && si.Active {
				filtered = append(filtered, it)
			}
		}
		cands = filtered
	}

	// Goal filter: explicit --goal flag takes precedence; fall back to the
	// calling agent's focus_goal. If the focused goal is terminal or missing,
	// auto-clear it and skip the filter (defensive — handles the window
	// between GoalMarkMet/GoalDrop and the agent noticing).
	goalID := opts.Goal
	if goalID == "" {
		goalID = agent.GetGoalFocus(cfg, cfg.Identity().ID)
	}
	if goalID != "" {
		goal, ok := s.Get(goalID)
		if !ok || goal.Type != "goal" || goal.Status != "active" {
			if opts.Goal == "" {
				// focus_goal is stale — auto-clear so the agent isn't
				// silently stuck with an empty result set, and return
				// the unfiltered candidates (degraded-but-useful behaviour).
				_ = agent.ClearGoalFocus(cfg, cfg.Identity().ID)
			} else {
				// Explicit --goal named a non-existent or terminal goal.
				// Return empty rather than silently ignoring the filter —
				// the operator asked for a specific goal and must see that
				// there are no eligible items, not a misleading full list.
				// I-896.
				return nil
			}
		} else {
			filtered := cands[:0]
			for _, it := range cands {
				for _, gid := range it.Goals {
					if gid == goalID {
						filtered = append(filtered, it)
						break
					}
				}
			}
			cands = filtered
		}
	}

	return cands
}

// unblockLeverage counts, for each candidate, how many downstream items it
// unblocks that are still waiting to start (not resolved and not already active).
// Active items are excluded: they are already in-flight and completing the
// upstream dep does not free them — the work is already running. Only
// genuinely blocked, not-yet-started items represent real future work the
// candidate would free. ID lists are SORTED for deterministic rationale output.
func unblockLeverage(g *deps.Graph, cands []*model.Item) (map[string]int, map[string][]string) {
	lev := make(map[string]int, len(cands))
	names := make(map[string][]string, len(cands))
	for _, it := range cands {
		for _, downID := range g.BlocksItems(it.ID) {
			down, ok := g.Items[downID]
			if !ok {
				continue
			}
			if g.IsResolved(downID) || down.Status == "active" {
				continue
			}
			lev[it.ID]++
			names[it.ID] = append(names[it.ID], downID)
		}
		sort.Strings(names[it.ID])
	}
	return lev, names
}

// enrichUnblockDetail rewrites the "unblock" factor's Detail to name the
// concrete unblocked IDs (Rationale() recomputes from Factors on demand —
// there is no cache to invalidate). Kept out of the pure scorer so the
// core stays map-only: it never needs the ID list.
func enrichUnblockDetail(recs []coordinator.Recommendation, names map[string][]string) {
	for i := range recs {
		ids := names[recs[i].Item.ID]
		if len(ids) == 0 {
			continue
		}
		for j := range recs[i].Factors {
			if recs[i].Factors[j].Name == "unblock" {
				recs[i].Factors[j].Detail =
					coordinator.NamedUnblocked(recs[i].Factors[j].Detail, ids)
			}
		}
	}
}

// loadGoalWeights sums active-goal weights per item ID. Resilient: an empty
// or missing goal corpus yields an empty map (zero contribution), matching
// the loadSprintInfo precedent — recommend must never fail because of I/O.
func loadGoalWeights(s *store.Store) map[string]float64 {
	out := map[string]float64{}
	goals := s.List(store.TypeFilter("goal"))
	for _, g := range goals {
		if g.Status != "active" || g.Weight == nil {
			continue
		}
		w := float64(*g.Weight)
		for _, itemID := range g.Goals {
			out[itemID] += w
		}
	}
	return out
}

// loadQueuePins returns the set of item IDs that are operator-pinned in
// queue.yaml. An entry is a pin when its Source is NOT QueueSourceSprint
// (i.e., "manual", empty/legacy, or any future manual variant). Sprint-
// sourced entries are NOT pins — they were legacy auto-queue artefacts.
// Resilient: an unreadable queue.yaml yields an empty set (no boost).
func loadQueuePins(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, e := range LoadQueue(cfg) {
		if e.Source != QueueSourceSprint {
			out[e.ID] = true
		}
	}
	return out
}

// loadSprintInfo builds sprintID → SprintInfo. Resilient: a missing /
// unreadable registry yields an empty map (the sprint factor simply does
// not contribute) — recommend must never fail because of registry I/O.
func loadSprintInfo(cfg *config.Config, g *deps.Graph) map[string]coordinator.SprintInfo {
	out := map[string]coordinator.SprintInfo{}
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil || r == nil {
		return out
	}
	for _, sp := range r.Sprints {
		total := len(sp.Items)
		done := 0
		for _, id := range sp.Items {
			if g.IsResolved(id) {
				done++
			}
		}
		frac := 0.0
		if total > 0 {
			frac = float64(done) / float64(total)
		}
		out[sp.ID] = coordinator.SprintInfo{
			Active: sp.Status == "active", CompletionFrac: frac,
		}
	}
	return out
}
