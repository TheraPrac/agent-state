// Package freshness implements the I-711 plan/SBAR freshness gate
// that fires inside `command.Start` BEFORE worktree creation (so a
// refusal can't leave orphan filesystem state). Re-validates plan
// premises against the current code state so a stale plan can't be
// silently activated.
//
// Current implementation is heuristics-only:
//
//   - File existence on plan-referenced paths.
//   - Age threshold (Drift after 7 days, Stale after 14 days; see
//     DefaultThresholds for the production cutoffs).
//   - Dependency-closure keyword match against the plan's Approach
//     section: if a depends_on item closed since plan_approved_at
//     and its recommendation/assessment shares a keyword with the
//     plan's Approach, that's a Drift candidate.
//   - Git churn on touched paths (>= 10 commits since approval on
//     any plan-touched path inside a scope repo → Drift candidate).
//
// A Claude sub-agent phase that would promote/demote heuristic
// verdicts is the natural next iteration, but is NOT wired in this
// package — CategoryClaude exists as a stable enum value for that
// follow-up. CheckOpts has no Engine field today.
//
// Verdict is one of Fresh / Drift / Stale. Cache the verdict at
// `<workspace>/.as/cache/freshness/<id>-<sha256(plan)>-<head>.json`
// so a same-state re-start is instant. Stale verdicts are NOT
// cached (so re-prep is always re-evaluated).
package freshness

import (
	"time"
)

// Verdict classifies whether a plan still matches current state.
type Verdict int

const (
	// VerdictFresh means: plan premises hold; activation should
	// proceed silently.
	VerdictFresh Verdict = iota

	// VerdictDrift means: plan is mostly current but at least one
	// premise has shifted (touched file changed substantially, a
	// depended-on item closed with conflicting resolution, age >
	// soft threshold). Activation should refuse without an
	// explicit operator ack via `--ack-drift "<note>"`.
	VerdictDrift

	// VerdictStale means: plan premises are clearly invalidated
	// (referenced file no longer exists, age > hard threshold).
	// Activation should refuse; operator must re-prep before
	// retrying. No --ack-stale opt-out.
	VerdictStale
)

func (v Verdict) String() string {
	switch v {
	case VerdictFresh:
		return "fresh"
	case VerdictDrift:
		return "drift"
	case VerdictStale:
		return "stale"
	}
	return "unknown"
}

// FindingCategory tags a Finding's signal source so the operator
// can quickly understand what tripped the gate.
type FindingCategory string

const (
	CategoryFileMissing      FindingCategory = "file-missing"
	CategoryDependencyClosed FindingCategory = "dependency-closed"
	CategoryAgeThreshold     FindingCategory = "age-threshold"
	CategoryGitChurn         FindingCategory = "git-churn"
	CategoryClaude           FindingCategory = "claude-review"
)

// Finding describes one signal that contributed to the verdict.
type Finding struct {
	Category FindingCategory
	Message  string
}

func (f Finding) String() string {
	return string(f.Category) + ": " + f.Message
}

// Result is the freshness verdict for a single item.
type Result struct {
	Verdict     Verdict
	Findings    []Finding
	PlanHash    string // sha256 of the plan body that was evaluated
	Head        string // workspace HEAD sha at evaluation time
	EvaluatedAt time.Time
}

// Thresholds bundles the heuristic age cutoffs in one place so
// tests can override deterministically.
type Thresholds struct {
	DriftAfter time.Duration // age > this → Drift candidate
	StaleAfter time.Duration // age > this → Stale candidate
	ChurnCount int           // >= this many commits since approval on touched paths → Drift
}

// DefaultThresholds returns the production heuristic cutoffs:
// 7 days for Drift, 14 days for Stale, 10 commits for high churn.
func DefaultThresholds() Thresholds {
	return Thresholds{
		DriftAfter: 7 * 24 * time.Hour,
		StaleAfter: 14 * 24 * time.Hour,
		ChurnCount: 10,
	}
}
