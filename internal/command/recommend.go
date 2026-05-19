package command

import (
	"encoding/json"
	"fmt"
	"time"

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
	Queue bool   // candidate set = the DISPATCH view (queue + EligibleForDispatch)
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
func Recommend(s *store.Store, cfg *config.Config, opts RecommendOpts) int {
	top := opts.Top
	if top <= 0 {
		top = 10
	}
	g := deps.Build(s.All(), cfg)

	cands := recommendCandidates(s, cfg, g, opts)
	leverage, names := unblockLeverage(g, cands)
	sprints := loadSprintInfo(cfg, g)

	recs := coordinator.Recommend(cands, leverage, sprints, time.Now())
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
			fmt.Println("[]")
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	if len(recs) == 0 {
		fmt.Println("No recommendable items (none workable in scope).")
		return 0
	}
	for _, r := range recs {
		fmt.Printf("%-8s p%d  %s\n", r.Item.ID, r.Priority, r.Item.Title)
		fmt.Printf("      why: %s\n", r.Rationale())
	}
	return 0
}

// recommendCandidates resolves the candidate set:
//   - --queue ⇒ the DISPATCH view: queue entries that pass
//     coordinator.EligibleForDispatch (exactly what selectNext sees), so
//     the operator and the coordinator read the identical rationale.
//   - default ⇒ the PLANNING view: g.Ready() (unblocked + start-status +
//     unassigned) — the established "what's workable" primitive.
//
// --scope sprint further restricts to members of an ACTIVE sprint.
func recommendCandidates(s *store.Store, cfg *config.Config, g *deps.Graph,
	opts RecommendOpts) []*model.Item {

	var cands []*model.Item
	if opts.Queue {
		for _, e := range LoadQueue(cfg) {
			it, ok := s.Get(e.ID)
			if !ok {
				continue
			}
			terminal := cfg.IsTerminalStatus(it.Type, it.Status)
			if ok2, _ := coordinator.EligibleForDispatch(
				it, e.Approved, g.IsBlocked(e.ID), terminal); ok2 {
				cands = append(cands, it)
			}
		}
	} else {
		cands = g.Ready()
	}

	if opts.Scope == "sprint" {
		active := activeSprintIDs(cfg)
		filtered := cands[:0]
		for _, it := range cands {
			if it.Sprint != "" && active[it.Sprint] {
				filtered = append(filtered, it)
			}
		}
		cands = filtered
	}
	return cands
}

// unblockLeverage counts, for each candidate, how many NON-resolved items
// it unblocks (g.IsResolved ⇒ terminal or merged+), and records their IDs
// for the rationale. Freeing downstream work is the strongest in-band
// coordinator signal.
func unblockLeverage(g *deps.Graph, cands []*model.Item) (map[string]int, map[string][]string) {
	lev := make(map[string]int, len(cands))
	names := make(map[string][]string, len(cands))
	for _, it := range cands {
		for _, downID := range g.BlocksItems(it.ID) {
			if !g.IsResolved(downID) {
				lev[it.ID]++
				names[it.ID] = append(names[it.ID], downID)
			}
		}
	}
	return lev, names
}

// enrichUnblockDetail rewrites the "unblock" factor (and the cached
// rationale) to name the concrete unblocked IDs — kept out of the pure
// scorer so that core stays map-only (it never needs the ID list).
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

// activeSprintIDs is the --scope sprint membership filter. Resilient like
// loadSprintInfo: no registry ⇒ empty set ⇒ no items match.
func activeSprintIDs(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil || r == nil {
		return out
	}
	for _, sp := range r.Sprints {
		if sp.Status == "active" {
			out[sp.ID] = true
		}
	}
	return out
}
