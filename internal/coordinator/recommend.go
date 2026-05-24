package coordinator

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

// recommend.go is part of the pure DECISION CORE (see loop.go's header).
// It scores candidate items into a ranked, inspectable recommendation so
// dispatch is "with an inspectable rationale — never an opaque choice"
// (operating-contract §4.2). Like the rest of this package it does ZERO
// I/O: the imperative shell (internal/command/recommend.go) computes the
// graph/registry-derived inputs (unblock leverage, sprint info) and hands
// them in as plain maps, so every ranking rule is unit-testable with
// synthetic data and never an exec'd worker (the T-360/§13 discipline).

// SprintInfo is the I/O-free sprint context the scorer needs. The shell
// loads registry.Sprint and computes CompletionFrac; the core never reads
// disk. NOTE: registry.Sprint has no deadline field (ground truth, T-369
// plan) — sprint pressure is completion-driven, not deadline-driven.
type SprintInfo struct {
	Active         bool    // sprint Status == "active"
	CompletionFrac float64 // terminal items / total items, clamped 0..1
}

// Factor is one labelled contribution to a recommendation. The rationale
// is the sum of its parts so the "why" is never an opaque number.
type Factor struct {
	Name   string  // "priority" | "unblock" | "sprint" | "age"
	Points float64 // contribution to Score (0 for priority — it is the
	//                 lexicographic primary key, not an additive term)
	Detail string // human text, e.g. "priority p1", "unblocks 2 (T-364,T-365)"
}

// Recommendation is a scored candidate plus its decomposed why.
type Recommendation struct {
	Item     *model.Item
	Priority int     // resolved (nil → 2); the PRIMARY sort key, ascending
	Score    float64 // SECONDARY composite (unblock+sprint+age), descending
	Factors  []Factor
}

// Rationale renders the factors as one inspectable line, e.g.
// "priority p1 · unblocks 2 (T-364,T-365) · sprint foo 60% · age 9d".
func (r Recommendation) Rationale() string {
	parts := make([]string, 0, len(r.Factors))
	for _, f := range r.Factors {
		parts = append(parts, f.Detail)
	}
	return strings.Join(parts, " · ")
}

// Scoring constants. Ordering is LEXICOGRAPHIC — (Priority asc, Score
// desc, ID asc) — so priority dominates *by construction*: no accumulation
// of secondary factors can leapfrog a strictly-higher-priority item.
// recommend therefore only *reasons*; it never silently re-ranks past the
// project's existing priority primitive. The secondary weights only order
// items WITHIN one priority band, where unblock leverage (free downstream
// work) is the strongest coordinator signal, then sprint-closure pressure,
// then a bounded age tiebreak against starvation.
const (
	unblockWeight   = 10.0 // per non-terminal item this one unblocks
	sprintWeight    = 5.0  // × completion fraction of its ACTIVE sprint (≤ 5)
	agePerDay       = 0.05 // per day since Created, capped → ≤ 1.5
	ageCapDays      = 30.0
	goalWeightFactor = 0.5 // per weight-point contributed by item's active goals (max +50)
	maxRationaleIDs = 3    // how many unblocked-item IDs to name before "+k"
)

func resolvePriority(item *model.Item) int {
	if item.Priority != nil {
		return *item.Priority
	}
	return 2 // project default (mirrors deps.priorityOf / SizeClassBaseline)
}

// Recommend ranks cands. leverage[id] is the number of NON-terminal items
// id unblocks (computed by the shell from the dep graph). sprints maps an
// item's Sprint id to its SprintInfo. goalWeights[id] is the sum of active
// Goal.weight values for every goal the item belongs to (built by the shell
// from item.Goals; nil or missing key → zero contribution). now anchors the
// age factor (passed in, not time.Now(), so tests are deterministic). The
// returned slice is stably ordered: priority asc, then composite score desc,
// then ID asc.
func Recommend(cands []*model.Item, leverage map[string]int,
	sprints map[string]SprintInfo, goalWeights map[string]float64,
	now time.Time) []Recommendation {

	recs := make([]Recommendation, 0, len(cands))
	for _, it := range cands {
		if it == nil {
			continue
		}
		pri := resolvePriority(it)
		factors := []Factor{
			{Name: "priority", Points: 0, Detail: fmt.Sprintf("priority p%d", pri)},
		}
		var score float64

		// Unblock leverage — only surfaced when it actually drives rank.
		if n := leverage[it.ID]; n > 0 {
			pts := unblockWeight * float64(n)
			score += pts
			factors = append(factors, Factor{
				Name: "unblock", Points: pts,
				Detail: fmt.Sprintf("unblocks %d", n),
			})
		}

		// Sprint completion pressure — only when in an ACTIVE sprint (the
		// only case it contributes). No deadline term: the model has none.
		if it.Sprint != "" {
			if si, ok := sprints[it.Sprint]; ok && si.Active {
				frac := si.CompletionFrac
				if frac < 0 {
					frac = 0
				} else if frac > 1 {
					frac = 1
				}
				pts := sprintWeight * frac
				score += pts
				factors = append(factors, Factor{
					Name: "sprint", Points: pts,
					Detail: fmt.Sprintf("sprint %s %d%%", it.Sprint, int(frac*100+0.5)),
				})
			}
		}

		// Goal weight — only when the item belongs to at least one active goal.
		if w := goalWeights[it.ID]; w > 0 {
			pts := goalWeightFactor * w
			score += pts
			factors = append(factors, Factor{
				Name: "goal", Points: pts,
				Detail: fmt.Sprintf("goal-weight %.0f", w),
			})
		}

		// Age — bounded anti-starvation tiebreak, always shown.
		days := 0.0
		if !it.Created.IsZero() {
			days = now.Sub(it.Created).Hours() / 24
			if days < 0 {
				days = 0
			}
		}
		capped := days
		if capped > ageCapDays {
			capped = ageCapDays
		}
		agePts := agePerDay * capped
		score += agePts
		factors = append(factors, Factor{
			Name: "age", Points: agePts,
			Detail: fmt.Sprintf("age %dd", int(days)),
		})

		recs = append(recs, Recommendation{
			Item: it, Priority: pri, Score: score, Factors: factors,
		})
	}

	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Priority != recs[j].Priority {
			return recs[i].Priority < recs[j].Priority // lower p = better
		}
		if recs[i].Score != recs[j].Score {
			return recs[i].Score > recs[j].Score // higher composite = better
		}
		return recs[i].Item.ID < recs[j].Item.ID // stable, deterministic
	})
	return recs
}

// NamedUnblocked appends the concrete unblocked-item IDs to the "unblock"
// factor detail (e.g. "unblocks 2 (T-364,T-365)"). Kept separate from
// Recommend so the core stays map-only: the shell knows the IDs from the
// graph and enriches the rendered line without the scorer needing them.
func NamedUnblocked(detail string, ids []string) string {
	if len(ids) == 0 {
		return detail
	}
	shown := ids
	extra := 0
	if len(shown) > maxRationaleIDs {
		extra = len(shown) - maxRationaleIDs
		shown = shown[:maxRationaleIDs]
	}
	s := detail + " (" + strings.Join(shown, ",")
	if extra > 0 {
		s += fmt.Sprintf(",+%d", extra)
	}
	return s + ")"
}
