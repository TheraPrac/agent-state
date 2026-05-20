package freshness

import (
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// CheckOpts bundles injectables for testability. The production
// caller (command.Start) leaves all closures nil so defaults fire.
type CheckOpts struct {
	// Now overrides time.Now for deterministic age-threshold tests.
	Now time.Time

	// Thresholds overrides the production cutoffs. Zero-value uses
	// DefaultThresholds.
	Thresholds Thresholds

	// Statter overrides os.Stat for the file-existence check.
	Statter func(string) error

	// GitRunner overrides `git -C <root> <args...>` for the churn
	// heuristic and the workspace-HEAD cache key.
	GitRunner func(root string, args []string) ([]byte, error)

	// RepoRoot maps a scope-repo name to its on-disk path. The
	// production caller wires this to the standard agent-workspace
	// layout (each scope_repo lives at `<dirname(cfg.Root())>/<repo>`).
	// When nil, all repos are treated as not-present: git-churn
	// heuristic no-ops AND file-existence heuristic skips
	// known-prefix paths (I-719). Unrecognized-prefix paths still
	// fall back to workspace-relative resolution regardless.
	RepoRoot func(name string) (string, bool)

	// SkipCache disables read+write to the freshness cache. Used
	// by tests that want to force a re-evaluation each call.
	SkipCache bool
}

// Check evaluates plan + SBAR freshness for `id` against the
// current workspace state. Returns (Fresh|Drift|Stale, findings).
//
// Behavior:
//
//   - If plan_approved is false, returns Fresh with no findings
//     (no plan to validate). Callers that require a plan must
//     gate on PlanApproved separately.
//   - If the plan sidecar is missing, returns Fresh (matching the
//     I-710 missing-sidecar carve-out).
//   - Reads the cache first; instant return on hit. storeCache
//     never writes Stale, and loadCache has a defensive read-side
//     guard, so a cache hit can't be Stale.
//   - Heuristics fire next. Any file-missing finding or
//     age-over-StaleAfter is terminal Stale; otherwise any finding
//     is Drift; otherwise Fresh.
//   - Result is cached on disk before return (except Stale).
func Check(cfg *config.Config, s *store.Store, id string, opts CheckOpts) (*Result, error) {
	item, ok := s.Get(id)
	if !ok {
		return &Result{Verdict: VerdictFresh}, nil
	}
	if !item.PlanApproved {
		return &Result{Verdict: VerdictFresh}, nil
	}
	loaded, _ := plan.Load(cfg.PlansDir(), id)
	if loaded == nil {
		// No sidecar — nothing to validate against. Mirrors the
		// I-710 missing-sidecar carve-out.
		return &Result{Verdict: VerdictFresh}, nil
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	th := opts.Thresholds
	if th == (Thresholds{}) {
		th = DefaultThresholds()
	}
	statter := opts.Statter
	if statter == nil {
		statter = osStat
	}
	gitRunner := opts.GitRunner
	if gitRunner == nil {
		gitRunner = defaultGitRunner
	}
	repoRoot := opts.RepoRoot
	if repoRoot == nil {
		repoRoot = func(string) (string, bool) { return "", false }
	}

	workspaceRoot := cfg.Root()
	head := workspaceHead(workspaceRoot, gitRunner)
	planBody := loaded.RawText
	if planBody == "" {
		planBody = loaded.Approach
	}

	if !opts.SkipCache {
		if cached, ok := loadCache(workspaceRoot, id, planBody, head); ok {
			return cached, nil
		}
	}

	approvedAt, _ := parseRFC3339(item.PlanApprovedAt)

	var findings []Finding
	// I-720: pass the parsed plan to checkFileExistence so it
	// can iterate structured Plan.FilesToModify (not regex-over-
	// body) and route bare paths via the plan's ScopeRepos.
	// FilesToCreate is no longer falsely flagged as missing.
	// planBody is still consumed by checkGitChurn below.
	findings = append(findings, checkFileExistence(loaded, workspaceRoot, repoRoot, statter)...)
	findings = append(findings, checkAge(approvedAt, now, th)...)
	findings = append(findings, checkDependencyClosure(loaded, itemDependencies(item), approvedAt, s)...)
	// Review F6 fix: pass the same planBody used by file-existence
	// so churn extraction sees Approach content when RawText is
	// empty.
	findings = append(findings, checkGitChurn(loaded, planBody, approvedAt, repoRoot, th, gitRunner)...)

	// Review F7 fix: when approvedAt is the zero value (item
	// missing PlanApprovedAt or with a malformed timestamp), pass
	// 0 instead of ~56 years to classifyHeuristics so a
	// non-age-derived finding can't be silently escalated to Stale
	// via the `age > StaleAfter` branch.
	age := time.Duration(0)
	if !approvedAt.IsZero() {
		age = now.Sub(approvedAt)
	}
	verdict := classifyHeuristics(findings, age, th)

	r := &Result{
		Verdict:     verdict,
		Findings:    findings,
		PlanHash:    hashPlanBody(planBody),
		Head:        head,
		EvaluatedAt: now,
	}

	if !opts.SkipCache {
		_ = storeCache(workspaceRoot, id, planBody, head, r)
	}
	return r, nil
}

// classifyHeuristics translates the findings list into a single
// Verdict. Any file-missing finding promotes to Stale (the strong
// signal). Any age > StaleAfter promotes to Stale. Everything
// else with at least one finding is Drift. Empty findings → Fresh.
func classifyHeuristics(findings []Finding, age time.Duration, th Thresholds) Verdict {
	if len(findings) == 0 {
		return VerdictFresh
	}
	for _, f := range findings {
		if f.Category == CategoryFileMissing {
			return VerdictStale
		}
	}
	if age > th.StaleAfter {
		return VerdictStale
	}
	return VerdictDrift
}

// osStat is the production statter — referenced via the closure so
// tests can inject without touching the real filesystem.
func osStat(path string) error {
	_, err := osStatImpl(path)
	return err
}

// osStatImpl is wrapped to avoid importing os in the public API
// surface; lives in a separate file (stat_impl.go) so tests can
// shadow if needed.
