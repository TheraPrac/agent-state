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
	// production caller wires this to the worktree layout (each
	// scope_repo lives at `<worktree>/<repo>`). When nil, all repos
	// are treated as not-present (git-churn heuristic no-ops).
	RepoRoot func(name string) (string, bool)

	// SkipCache disables read+write to the freshness cache. Used
	// by tests that want to force a re-evaluation each call.
	SkipCache bool
}

// Check evaluates plan + SBAR freshness for `id` against the
// current workspace state. Returns (Fresh|Drift|Stale, findings)
// per the two-phase design documented on the package.
//
// Behavior:
//
//   - If plan_approved is false, returns Fresh with no findings
//     (no plan to validate). Callers that require a plan must
//     gate on PlanApproved separately.
//   - Reads the cache first; instant return on hit unless the
//     cached verdict is Stale (those are never cached).
//   - Cheap heuristics fire first. Any Stale candidate is terminal.
//   - Drift candidates trigger the Claude phase IF an engine is
//     wired; otherwise the heuristic verdict stands.
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
	findings = append(findings, checkFileExistence(planBody, workspaceRoot, statter)...)
	findings = append(findings, checkAge(approvedAt, now, th)...)
	findings = append(findings, checkDependencyClosure(loaded, itemDependencies(item), approvedAt, s)...)
	findings = append(findings, checkGitChurn(loaded, approvedAt, repoRoot, th, gitRunner)...)

	verdict := classifyHeuristics(findings, now.Sub(approvedAt), th)

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
