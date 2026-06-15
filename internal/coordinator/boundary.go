// Package coordinator implements the Shape-3 coordinator loop (T-363):
// agent-A picks the next unblocked item, spawns ONE budget-capped
// reasoning worker via the merged command.Spawn (T-360), supervises it
// through the observability substrate on ground truth only, applies the
// B1/C2/D2 stall heuristics against the autonomy boundary, and on any
// contract-§7 predicate emits a deduped, substrate-durable escalation and
// STOPS rather than exceeding the boundary.
//
// This file is the boundary loader: the typed read of
// .as/coordinator.yaml (the §11 autonomy boundary).
// The coordinator READS it and NEVER writes it (contract §11).
//
// LOAD-BEARING (contract §11/§13): the as repo carries no YAML dependency
// by policy — every YAML surface is a hand-rolled, scoped parser. This is
// the broader read; internal/spawn.ParsePerItemBudget keeps its narrow
// single-key read for `st spawn`. The parser is intentionally STRICT: a
// missing file, a missing required key, or a non-positive numeric is a
// HARD error so the coordinator NEVER runs with a silent default — an
// unbounded coordinator is the worst case (contract §11).
package coordinator

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/spawn"
)

// CoordinatorYAMLPath is re-exported from internal/spawn so the coordinator
// resolves the boundary file the exact same way `st spawn` does (one
// definition of where the boundary lives).
func CoordinatorYAMLPath(workspaceRoot string) string {
	return spawn.CoordinatorYAMLPath(workspaceRoot)
}

// Boundary is the typed projection of coordinator.yaml. Only the knobs the
// loop consumes are modelled. blast_radius_gate is intentionally NOT
// modelled: it is enforced by the per-worker plan-before-code/blast-radius
// hooks (contract §9.1), not by this loop.
type Boundary struct {
	RespawnLimit      int      // escalation.respawn_limit (B1/C2)
	PerItemUSD        float64  // escalation.budget_cap_usd.per_item (D1)
	PerObjectiveUSD   float64  // escalation.budget_cap_usd.per_objective (D1)
	StuckMultiplier   float64  // escalation.stuck_multiplier (D2)
	ParallelismCap    int      // escalation.parallelism_cap (D3)
	TripwireList      []string // escalation.tripwire_list (E2)
	DedupeWindowMin   int      // dedupe.window_minutes
	ActivePingClasses []string // escalation_channel.active_ping (K6)
}

// IsTripwire reports whether op is on the always-escalate tripwire list.
func (b *Boundary) IsTripwire(op string) bool {
	for _, t := range b.TripwireList {
		if t == op {
			return true
		}
	}
	return false
}

// ActivePings reports whether the given escalation class (e.g.
// "category_E", "budget_cap") should additionally fire the active-ping
// notification (contract §11-K6 / escalation_channel.active_ping).
func (b *Boundary) ActivePings(class string) bool {
	for _, c := range b.ActivePingClasses {
		if c == class {
			return true
		}
	}
	return false
}

// scanLevel is one open mapping level while scanning the YAML.
type scanLevel struct {
	indent int
	key    string
}

// LoadBoundary parses coordinator.yaml at path into a validated Boundary.
//
// Every required scalar must be present and sane or LoadBoundary returns a
// hard error naming the offending key — there is no silent default
// (contract §11: an unbounded coordinator is never allowed). The list
// keys (tripwire_list, active_ping) are allowed to be empty (a boundary
// with no tripwires is a deliberate, valid operator choice — unlike a
// missing budget cap).
func LoadBoundary(path string) (*Boundary, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("autonomy boundary not found at %s — refusing to run the coordinator unbounded (contract §11)", path)
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	b := &Boundary{}
	// Track which required scalars we actually saw, so a missing key is a
	// hard error (not a zero-value silent default).
	seen := map[string]bool{}

	// scalarPaths maps a dotted key path to its setter+validator.
	scalarPaths := map[string]func(string) error{
		"escalation.respawn_limit":                func(v string) error { return setPosInt(v, &b.RespawnLimit) },
		"escalation.budget_cap_usd.per_item":      func(v string) error { return setPosFloat(v, &b.PerItemUSD) },
		"escalation.budget_cap_usd.per_objective": func(v string) error { return setPosFloat(v, &b.PerObjectiveUSD) },
		"escalation.stuck_multiplier":             func(v string) error { return setPosFloat(v, &b.StuckMultiplier) },
		"escalation.parallelism_cap":              func(v string) error { return setPosInt(v, &b.ParallelismCap) },
		"dedupe.window_minutes":                   func(v string) error { return setPosInt(v, &b.DedupeWindowMin) },
	}
	// listPaths maps a dotted key path to the slice it appends `- ` items
	// into while that mapping level is open.
	listPaths := map[string]*[]string{
		"escalation.tripwire_list":       &b.TripwireList,
		"escalation_channel.active_ping": &b.ActivePingClasses,
	}

	var stack []scanLevel
	var listTarget *[]string // non-nil while inside a recognised list key
	listIndent := -1

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		// Strip trailing inline comment. coordinator.yaml values are bare
		// (unquoted) so '#' unambiguously starts a comment.
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			raw = raw[:i]
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		trimmed := strings.TrimSpace(raw)

		// A `- value` line: a list element. Collect it iff we're inside a
		// recognised list key and still indented under it.
		if strings.HasPrefix(trimmed, "- ") {
			if listTarget != nil && indent > listIndent {
				*listTarget = append(*listTarget, strings.TrimSpace(trimmed[2:]))
			}
			continue
		}

		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])

		// Pop levels this line is not nested under, then push this one.
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, scanLevel{indent: indent, key: key})

		// A new key at or above the list's indent closes the list.
		if listTarget != nil && indent <= listIndent {
			listTarget = nil
			listIndent = -1
		}

		dotted := dottedPath(stack)

		if lt, ok := listPaths[dotted]; ok && value == "" {
			// A mapping key whose value is empty AND whose path is a known
			// list: subsequent `- ` lines belong to it.
			listTarget = lt
			listIndent = indent
			continue
		}

		if set, ok := scalarPaths[dotted]; ok {
			if value == "" {
				return nil, fmt.Errorf("%s: %s is present but has no value", path, dotted)
			}
			if err := set(value); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, dotted, err)
			}
			seen[dotted] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Every required scalar must have been seen — a missing one is a hard
	// error, never a zero-value default (contract §11).
	for dotted := range scalarPaths {
		if !seen[dotted] {
			return nil, fmt.Errorf("%s: required key %s not found — refusing to run the coordinator unbounded (contract §11)", path, dotted)
		}
	}
	return b, nil
}

func dottedPath(stack []scanLevel) string {
	parts := make([]string, len(stack))
	for i, l := range stack {
		parts[i] = l.key
	}
	return strings.Join(parts, ".")
}

func setPosInt(v string, dst *int) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not an integer", v)
	}
	if n <= 0 {
		return fmt.Errorf("%d must be > 0", n)
	}
	*dst = n
	return nil
}

func setPosFloat(v string, dst *float64) error {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%q is not a number", v)
	}
	if n <= 0 {
		return fmt.Errorf("%v must be > 0", n)
	}
	*dst = n
	return nil
}
