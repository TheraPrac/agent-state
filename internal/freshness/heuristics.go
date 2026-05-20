package freshness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// filePathPattern captures path-like tokens in a plan body for the
// file-existence heuristic. Anchored on whitespace boundaries, ends
// in a recognized source-file extension so prose mentions like
// "the auth flow" don't trigger.
// Longer alternatives (tsx, jsx, yaml) come before their shorter
// prefixes (ts, js, yml) so the regex's leftmost-first match picks
// the longer extension.
var filePathPattern = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:tsx|jsx|yaml|yml|ts|js|go|sql|tf|md|sh|py)\b`)

// extractReferencedPaths returns the unique set of workspace-
// relative file paths the plan body references via the file-path
// regex. The result is intersected against `os.Stat` (in
// checkFileExistence) to detect referenced files that no longer
// exist.
func extractReferencedPaths(planBody string) []string {
	matches := filePathPattern.FindAllString(planBody, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		// Skip obvious false-positives: lone "..." patterns end in
		// .md/.go via partial matches sometimes; canonicalize.
		m = strings.TrimSuffix(m, ".")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// checkFileExistence verifies every path the plan declares it will
// MODIFY exists on disk now. Returns Stale-category findings for
// missing paths.
//
// I-720: pivot to structured Plan.FilesToModify (away from
// regex-over-body). The old behavior picked up every path-shaped
// substring in the entire plan body, including FilesToCreate
// entries (paths that by definition shouldn't exist yet) and
// narrative path mentions in Approach / Out of Scope. The new
// approach only checks files the plan declares as targets of
// modification.
//
// Resolution rules per FilesToModify entry (priority order):
//
//  1. Absolute path → stat as-is.
//  2. Relative path with a known repo prefix (theraprac-api,
//     theraprac-web, theraprac-infra, theraprac-workspace, as) →
//     strip the prefix and look up the repo root via the closure:
//     - Closure returns root → stat at <root>/<rel-path>.
//     - Closure returns false → SKIP the path (no finding). Fail
//       open: we cannot verify, so we must not falsely promote to
//       Stale. Mirrors the same fail-open posture checkGitChurn
//       uses for missing scope repos.
//  3. Bare path AND len(p.ScopeRepos) == 1 → stat via
//     repoRoot(p.ScopeRepos[0]). I-720: this is the
//     "unambiguous single-repo scope" case the I-719 fix missed.
//     Closure returns false → SKIP (fail-open).
//  4. Bare path AND 0 or 2+ scope repos → stat under
//     workspaceRoot. Preserves back-compat for unscoped or
//     multi-repo plans referencing docs/*.md or .plans/*.md.
//
// Plan.FilesToCreate is NEVER checked: those are future files.
// The regex helper extractReferencedPaths stays in the package
// for checkGitChurn — churn legitimately wants a broad path set.
//
// statter is injectable for tests; production passes os.Stat.
// repoRoot is the same closure type checkGitChurn uses.
func checkFileExistence(p *plan.Plan, workspaceRoot string, repoRoot func(name string) (string, bool), statter func(string) error) []Finding {
	if p == nil {
		return nil
	}
	if len(p.FilesToModify) == 0 {
		// Legitimate case (e.g., plan only creates files). One-
		// line stderr breadcrumb so verbose UAT logs surface the
		// silent-skip; not a finding.
		fmt.Fprintf(os.Stderr, "freshness: plan has no FilesToModify — file-existence heuristic skipped (no findings)\n")
		return nil
	}

	var findings []Finding
	for _, path := range p.FilesToModify {
		// 1. Absolute path: stat as-is.
		if filepath.IsAbs(path) {
			if err := statter(path); err != nil {
				findings = append(findings, Finding{
					Category: CategoryFileMissing,
					Message:  fmt.Sprintf("plan references %q but the file no longer exists at %s", path, path),
				})
			}
			continue
		}

		// 2. Known-repo-prefix path: route via repoRoot closure.
		if repo := matchedRepoPrefix(path); repo != "" {
			findings = append(findings, statScopedPath(path, repo, repoRoot, statter)...)
			continue
		}

		// 3. Bare path with single scope_repo: route via
		//    repoRoot(scope_repos[0]). I-720 — the case I-719
		//    missed.
		if len(p.ScopeRepos) == 1 {
			findings = append(findings, statScopedPath(path, p.ScopeRepos[0], repoRoot, statter)...)
			continue
		}

		// 4. Bare path with zero or multi-repo scope: workspace-
		//    relative. Today's pre-I-720 fallback behavior.
		abs := filepath.Join(workspaceRoot, path)
		if err := statter(abs); err != nil {
			findings = append(findings, Finding{
				Category: CategoryFileMissing,
				Message:  fmt.Sprintf("plan references %q but the file no longer exists at %s", path, abs),
			})
		}
	}
	return findings
}

// statScopedPath looks up repoRoot for <repo>, strips the prefix
// (if any), and stats the resulting path. Fail-open if repoRoot
// returns false (closure says repo isn't present in this layout —
// we can't verify, so don't falsely flag missing).
func statScopedPath(path, repo string, repoRoot func(name string) (string, bool), statter func(string) error) []Finding {
	root, ok := repoRoot(repo)
	if !ok {
		return nil
	}
	rel := strings.TrimPrefix(path, repo+"/")
	abs := filepath.Join(root, rel)
	if err := statter(abs); err != nil {
		return []Finding{{
			Category: CategoryFileMissing,
			Message:  fmt.Sprintf("plan references %q but the file no longer exists at %s", path, abs),
		}}
	}
	return nil
}

// matchedRepoPrefix returns the repo name (without trailing slash)
// when p begins with a known-repo prefix; otherwise "". Used by
// checkFileExistence to route stat lookups through repoRoot.
func matchedRepoPrefix(p string) string {
	for _, pfx := range knownRepoPrefixes {
		if strings.HasPrefix(p, pfx) {
			return strings.TrimSuffix(pfx, "/")
		}
	}
	return ""
}

// checkAge translates the plan_approved_at timestamp against the
// configured thresholds.
//
//   - now - approvedAt > StaleAfter → Stale candidate
//   - now - approvedAt > DriftAfter → Drift candidate
//   - otherwise → no finding (Fresh by default)
func checkAge(approvedAt time.Time, now time.Time, th Thresholds) []Finding {
	if approvedAt.IsZero() {
		return nil
	}
	age := now.Sub(approvedAt)
	switch {
	case age > th.StaleAfter:
		return []Finding{{
			Category: CategoryAgeThreshold,
			Message:  fmt.Sprintf("plan approved %s ago (> stale cutoff of %s)", age.Round(time.Hour), th.StaleAfter),
		}}
	case age > th.DriftAfter:
		return []Finding{{
			Category: CategoryAgeThreshold,
			Message:  fmt.Sprintf("plan approved %s ago (> drift cutoff of %s)", age.Round(time.Hour), th.DriftAfter),
		}}
	}
	return nil
}

// approachKeywords extracts the meaningful tokens from a plan's
// Approach section for use by the dependency-closure heuristic.
// Lowercased, deduped, drops 1-3 letter stopwords.
func approachKeywords(approach string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Fields(approach) {
		t := strings.ToLower(strings.Trim(raw, ".,;:()[]\"`"))
		if len(t) <= 3 {
			continue
		}
		out[t] = true
	}
	return out
}

// checkDependencyClosure scans every depends_on ID. If the item is
// in a terminal status (done/abandoned) AND its last_touched is
// after the plan was approved AND any keyword from the plan
// Approach appears in its resolution/recommendation prose, the
// dependency's closure may have invalidated the plan's premise.
func checkDependencyClosure(p *plan.Plan, deps []string, planApprovedAt time.Time, s *store.Store) []Finding {
	if p == nil || s == nil {
		return nil
	}
	keywords := approachKeywords(p.Approach)
	if len(keywords) == 0 {
		return nil
	}
	var findings []Finding
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
				findings = append(findings, Finding{
					Category: CategoryDependencyClosed,
					Message:  fmt.Sprintf("dependency %s closed %s with keyword overlap (%q) — may have invalidated plan premise", depID, closedAt.Format("2006-01-02"), kw),
				})
				break
			}
		}
	}
	return findings
}

func isTerminal(status string) bool {
	switch status {
	case "done", "abandoned", "completed", "resolved", "wontfix":
		return true
	}
	return false
}

func parseRFC3339(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	return time.Parse(time.RFC3339, s)
}

// checkGitChurn counts commits on the plan's touched paths since
// the plan was approved. Above the configured ChurnCount, emits a
// Drift candidate. Runs `git log --oneline --since=... -- <paths>`
// in each scope repo declared in the plan.
//
// repoRoot is a closure from scope-repo name → on-disk path so the
// caller can wire this to the real workspace layout (worktrees vs
// canonical clones).
//
// Review F1/F6: takes an explicit `planBody` rather than reading
// p.RawText directly, so the caller can pass the same body used by
// checkFileExistence (RawText with Approach fallback). Paths
// extracted from the plan are stripped of any leading scope-repo
// prefix before being handed to `git log`, since git resolves
// paths relative to the per-repo root — a workspace-prefixed path
// like `theraprac-api/internal/foo.go` would be looked up as
// `theraprac-api/internal/foo.go` UNDER /wsroot/theraprac-api,
// silently returning zero commits.
func checkGitChurn(p *plan.Plan, planBody string, approvedAt time.Time, repoRoot func(name string) (string, bool), th Thresholds, runner func(repo string, args []string) ([]byte, error)) []Finding {
	if p == nil || approvedAt.IsZero() {
		return nil
	}
	paths := extractReferencedPaths(planBody)
	if len(paths) == 0 {
		return nil
	}
	since := approvedAt.UTC().Format(time.RFC3339)
	var findings []Finding
	for _, repo := range p.ScopeRepos {
		root, ok := repoRoot(repo)
		if !ok {
			continue
		}
		repoPaths := pathsForRepo(repo, paths)
		if len(repoPaths) == 0 {
			continue
		}
		args := []string{"log", "--oneline", "--since=" + since, "--"}
		args = append(args, repoPaths...)
		out, err := runner(root, args)
		if err != nil {
			continue
		}
		count := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
		if count >= th.ChurnCount {
			findings = append(findings, Finding{
				Category: CategoryGitChurn,
				Message:  fmt.Sprintf("scope repo %q has %d commits on plan-touched paths since approval (>= churn cutoff %d) — touched code may have shifted", repo, count, th.ChurnCount),
			})
		}
	}
	return findings
}

// pathsForRepo filters paths to those belonging to the given
// scope repo, stripping the repo-name prefix so the resulting
// paths resolve correctly when `git log` runs INSIDE that repo.
// Returns the repo-relative path set. Paths without a recognizable
// scope-repo prefix are included as-is — they're assumed to
// already be repo-relative (the common case for plans scoped to a
// single repo).
func pathsForRepo(repo string, paths []string) []string {
	var out []string
	prefix := repo + "/"
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, strings.TrimPrefix(p, prefix))
			continue
		}
		// No prefix → assume already repo-relative.
		if !containsRepoPrefix(p) {
			out = append(out, p)
		}
	}
	return out
}

// knownRepoPrefixes is the set of repos that may appear as leading
// path segments in plan bodies. Used to distinguish "already
// repo-relative" from "scoped to a different repo than the one
// currently being scanned".
var knownRepoPrefixes = []string{
	"theraprac-api/",
	"theraprac-web/",
	"theraprac-infra/",
	"theraprac-workspace/",
	"as/",
}

// containsRepoPrefix is a thin alias around matchedRepoPrefix for
// call sites (`pathsForRepo`) that only need a bool. Keeps the
// prefix-walk centralized so a future addition to
// knownRepoPrefixes only updates one place.
func containsRepoPrefix(p string) bool {
	return matchedRepoPrefix(p) != ""
}

// defaultGitRunner runs `git -C <root> <args...>` and returns
// combined output + exec error. Used by Check when no runner is
// injected.
func defaultGitRunner(root string, args []string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	return cmd.CombinedOutput()
}

// itemDependencies returns the depends_on IDs from the item's
// in-memory model. Defensive against nil model.Item.
func itemDependencies(item *model.Item) []string {
	if item == nil {
		return nil
	}
	return item.DependsOn
}
