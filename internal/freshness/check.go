package freshness

import (
	"fmt"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/plan"
	"github.com/theraprac/agent-state/internal/store"
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

	// RunClaude is the function-value injection point for the
	// Claude sub-agent adjudication phase (I-717). When non-nil,
	// fires ONLY for heuristic-Drift verdicts to promote/demote
	// the verdict via LLM review. Terminal verdicts (Stale/Fresh)
	// short-circuit before the Claude pass. Nil = heuristics-only
	// (today's pre-I-717 behavior).
	//
	// Signature matches command.RunEngine.RunClaude exactly so
	// the command-package bridge can pass `engine.RunClaude`
	// without an interface or struct (avoiding the freshness ↔
	// command import cycle).
	RunClaude func(cwd string, args []string, env []string) ([]byte, int, error)

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
		// I-716: closes the missing-sidecar carve-out. An item
		// with plan_approved=true but no sidecar is Stale — the
		// plan body either was never authored or got deleted
		// post-approval. Either way, st start must refuse so
		// the operator re-preps before any code edits.
		return &Result{
			Verdict: VerdictStale,
			Findings: []Finding{{
				Category: CategoryFileMissing,
				Message:  fmt.Sprintf("plan_approved is true but .plans/%s.md is missing — re-prep required (I-716)", id),
			}},
		}, nil
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

	// I-717: Claude sub-agent adjudication for ambiguous-middle
	// (Drift) verdicts. Terminal verdicts (Stale, Fresh) are
	// short-circuit — file-missing is unambiguous, no-findings
	// has nothing to elevate.
	//
	// LLM verdict can demote Drift → Fresh or promote Drift →
	// Stale; cannot move terminal verdicts. Engine error / empty
	// output / unparseable response = fail-closed (keep heuristic
	// Drift). The promoted CategoryClaude finding documents the
	// adjudication for the verbose UAT log.
	if verdict == VerdictDrift && opts.RunClaude != nil {
		closed := closedDepsForPrompt(loaded, itemDependencies(item), approvedAt, s)
		recentGitLog := gatherRecentGitLog(loaded, approvedAt, repoRoot, gitRunner)
		prompt := buildFreshnessPrompt(item, loaded, findings, recentGitLog, closed)
		newVerdict, rationale := runFreshnessClaudePass(opts.RunClaude, workspaceRoot, verdict, prompt)
		if newVerdict != verdict && rationale != "" {
			findings = append(findings, Finding{
				Category: CategoryClaude,
				Message:  rationale,
			})
			verdict = newVerdict
		}
	}

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

// closedDepsForPrompt returns the *model.Item slice for any
// depends_on entry that closed since plan_approved_at with
// keyword overlap on the plan's Approach. Reuses the same
// matching predicate checkDependencyClosure used to produce the
// findings, so the prompt sees exactly the items the heuristic
// flagged.
func closedDepsForPrompt(p *plan.Plan, deps []string, planApprovedAt time.Time, s *store.Store) []*model.Item {
	if p == nil || s == nil {
		return nil
	}
	keywords := approachKeywords(p.Approach)
	if len(keywords) == 0 {
		return nil
	}
	var out []*model.Item
	for _, depID := range deps {
		depItem, ok := s.Get(depID)
		if !ok {
			continue
		}
		if !isTerminal(depItem.Status) {
			continue
		}
		closedAt := depItem.LastTouched
		if closedAt.IsZero() || closedAt.Before(planApprovedAt) {
			continue
		}
		text := strings.ToLower(depItem.SBAR.Recommendation + " " + depItem.SBAR.Assessment)
		for kw := range keywords {
			if strings.Contains(text, kw) {
				out = append(out, depItem)
				break
			}
		}
	}
	return out
}

// gatherRecentGitLog runs `git log --since=<approvedAt> --oneline -n 20`
// on the first scope repo's root (since I-717's prompt only renders
// a single snippet) and returns the output. Returns empty string on
// any failure — the prompt section gets skipped naturally.
func gatherRecentGitLog(p *plan.Plan, approvedAt time.Time, repoRoot func(name string) (string, bool), runner func(repo string, args []string) ([]byte, error)) string {
	if p == nil || approvedAt.IsZero() || len(p.ScopeRepos) == 0 || runner == nil {
		return ""
	}
	root, ok := repoRoot(p.ScopeRepos[0])
	if !ok {
		return ""
	}
	out, err := runner(root, []string{
		"log", "--oneline", "-n", "20",
		"--since=" + approvedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
