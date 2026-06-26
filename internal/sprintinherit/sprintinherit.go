// Package sprintinherit implements the I-681 mid-sprint follow-up rule:
// a blocker filed while working a sprint item (the
// `st push <new> --reason "blocks <parent>"` pattern) must join the
// in-progress sprint, so the sprint's burndown is never blind to work it
// depends on.
//
// The motivating incident: I-676 was filed as a blocker of T-203 (active
// sprint promptly-amazed-asp), worked, and merged — yet carried no sprint,
// so `st sprint show` and the burndown never saw it. Prose discipline had
// already documented the intent in I-676's own plan and still wasn't
// followed; this package is the machine enforcement.
//
// It is a pure lookup library: push auto-inherits, start hard-gates, and
// check/reconcile surface drift, all from the single Resolve/Drift pair so
// the rule is single-sourced and unit-testable in isolation.
package sprintinherit

import (
	"fmt"
	"sort"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/deps"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/validate"
)

// Target describes the in-progress sprint a mid-sprint follow-up should
// inherit, and the in-sprint item it blocks (for human-readable messaging).
type Target struct {
	SprintID string
	EpicID   string
	Via      string // an in-sprint item that `id` blocks
}

// blockedItems returns the de-duplicated set of item IDs that `id` blocks,
// unioning the dependency-graph inverse edges (built from depends_on) with
// the item's own blocks: field. validate.ReciprocalDeps normally keeps the
// two sides in sync, but a freshly-filed follow-up may have set only one
// direction, so both are considered to avoid missing the parent.
func blockedItems(id string, it *model.Item, g *deps.Graph) []string {
	seen := map[string]bool{}
	var out []string
	add := func(x string) {
		if x == "" || x == id || seen[x] {
			return
		}
		seen[x] = true
		out = append(out, x)
	}
	for _, x := range g.BlocksItems(id) {
		add(x)
	}
	if it != nil {
		for _, x := range it.Blocks {
			add(x)
		}
	}
	sort.Strings(out)
	return out
}

// Resolve computes the in-progress sprint(s) item `id` should inherit
// because it blocks a member of an active (Status=="active") sprint.
//
//   - target != nil  → exactly one active sprint; inherit it.
//   - ambiguous != nil → `id` blocks members of >1 active sprint; callers
//     must NOT auto-pick — surface the choice instead.
//   - both nil       → nothing to inherit.
//
// Resolve deliberately does not inspect `id`'s own current sprint; the
// "skip when already placed" policy is applied by callers so this stays a
// pure relationship lookup.
func Resolve(id string, all map[string]*model.Item, g *deps.Graph, reg *registry.Registry) (target *Target, ambiguous []string) {
	it := all[id]
	bySprint := map[string]*Target{}
	var order []string // distinct active sprint IDs, first-seen order
	for _, blockedID := range blockedItems(id, it, g) {
		y := all[blockedID]
		if y == nil || y.Sprint == "" {
			continue
		}
		sp, err := reg.SprintByID(y.Sprint)
		if err != nil || sp.Status != "active" {
			continue
		}
		if _, ok := bySprint[sp.ID]; !ok {
			bySprint[sp.ID] = &Target{SprintID: sp.ID, EpicID: sp.Epic, Via: blockedID}
			order = append(order, sp.ID)
		}
	}
	switch len(order) {
	case 0:
		return nil, nil
	case 1:
		return bySprint[order[0]], nil
	default:
		ambiguous = append([]string(nil), order...)
		sort.Strings(ambiguous)
		return nil, ambiguous
	}
}

// Drift returns one validate.Error per non-terminal item that has no
// sprint of its own yet blocks an active-sprint member — i.e. work that is
// (or will be) done off the in-progress sprint it belongs to. Items
// already assigned to any sprint are treated as intentional cross-sprint
// work and are not flagged. Delivery stage is intentionally NOT a skip
// reason: an already-merged-but-sprintless blocker (the I-676 case) is
// precisely the drift this surfaces.
func Drift(all map[string]*model.Item, g *deps.Graph, reg *registry.Registry, cfg *config.Config) []validate.Error {
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var errs []validate.Error
	for _, id := range ids {
		it := all[id]
		if it == nil || it.Sprint != "" {
			continue
		}
		if cfg.IsTerminalStatus(it.Type, it.Status) {
			continue
		}
		t, ambiguous := Resolve(id, all, g, reg)
		switch {
		case t != nil:
			errs = append(errs, validate.Error{
				ItemID: id, Field: "sprint",
				Message: fmt.Sprintf("blocks %s in active sprint %s but is not in that sprint — run `st sprint add %s %s`",
					t.Via, t.SprintID, t.SprintID, id),
			})
		case len(ambiguous) > 0:
			errs = append(errs, validate.Error{
				ItemID: id, Field: "sprint",
				Message: fmt.Sprintf("blocks members of multiple active sprints %v but is in none — add it to the correct one with `st sprint add <sprint> %s`",
					ambiguous, id),
			})
		}
	}
	return errs
}
