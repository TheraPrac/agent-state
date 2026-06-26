package command

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// CloseScopeCheckOpts controls how closeScopeSuiteCheck resolves worktrees
// and runs git. Tests inject fakes; production leaves both fields nil.
type CloseScopeCheckOpts struct {
	// ResolveWorktree returns the worktree path for (itemID, repo).
	// Returning "" signals "worktree not found" — that repo is skipped.
	// Nil: uses resolveRepoDirForItem (I-407 lookup with RepoMap support).
	ResolveWorktree func(cfg *config.Config, itemID, repo string) string
	// RunGit runs git in dir with args and returns stdout.
	// Nil: uses the package-level runGit from gitdiff.go.
	RunGit func(dir string, args ...string) (string, error)
	// Skip bypasses the entire check (--skip-tier2-revalidation flag).
	Skip bool
}

// closeScopeSuiteCheck recomputes applicable scope suites from the current
// HEAD diff in each configured repo's worktree and verifies that every
// applicable suite has pass/skip evidence in item.TestingEvidence.
//
// Returns "" when all applicable suites pass, or an actionable message
// naming the missing suites. Conservative: worktree absent or git failure
// silently skips that repo — the check never blocks close due to missing
// infrastructure.
func closeScopeSuiteCheck(item *model.Item, cfg *config.Config, opts CloseScopeCheckOpts) string {
	if opts.Skip {
		return ""
	}
	if cfg == nil || cfg.Testing == nil || len(cfg.Testing.ScopeSuites) == 0 {
		return ""
	}
	if cfg.Worktree == nil || len(cfg.Worktree.Repos) == 0 {
		return ""
	}

	resolveWt := opts.ResolveWorktree
	if resolveWt == nil {
		// I-1477(e): use the strict item-only resolver, not resolveRepoDirForItem.
		// The latter falls back to the main repo when the item's worktree is gone
		// (already merged), where origin/main...HEAD picks up unrelated divergence
		// and falsely flags suites. itemWorktreeRepoDir returns "" instead, so a
		// merged item with no worktree contributes no changed files.
		resolveWt = itemWorktreeRepoDir
	}
	gitRunner := opts.RunGit
	if gitRunner == nil {
		gitRunner = runGit
	}

	// Collect which repos have changes and all changed file paths.
	// changedFiles is a combined list across repos; Triggers patterns are
	// matched against all files since they express file-path intent without
	// an implicit repo restriction.
	reposWithChanges := map[string]bool{}
	var changedFiles []string

	for _, repo := range cfg.Worktree.Repos {
		wt := resolveWt(cfg, item.ID, repo)
		if wt == "" {
			continue
		}
		// I-1477(e): anchor the diff at merge-base(origin/main, HEAD), matching
		// test_auto.go / ComputeFileChanges, so a stale local main ref can't skew
		// the result. Fall back to main..HEAD when origin/main is unavailable.
		var out string
		var err error
		if base, baseErr := gitRunner(wt, "merge-base", "origin/main", "HEAD"); baseErr == nil {
			out, err = gitRunner(wt, "diff", "--name-only", strings.TrimSpace(base)+"..HEAD")
		} else {
			out, err = gitRunner(wt, "diff", "--name-only", "main..HEAD")
		}
		if err != nil {
			continue // git unavailable or no remote — skip this repo conservatively
		}
		repoHasChanges := false
		for _, f := range strings.Split(out, "\n") {
			if f == "" {
				continue
			}
			repoHasChanges = true
			changedFiles = append(changedFiles, f)
		}
		if repoHasChanges {
			reposWithChanges[repo] = true
		}
	}

	if len(reposWithChanges) == 0 {
		return "" // nothing changed in any worktree — nothing to enforce
	}

	// For each scope suite, determine applicability and check evidence.
	var missing []string
	for name, sc := range cfg.Testing.ScopeSuites {
		if !scopeSuiteApplicable(sc, reposWithChanges, changedFiles) {
			continue
		}
		val := scopeEvidence(item, name)
		if strings.HasPrefix(val, "pass") ||
			strings.HasPrefix(val, "skip:") ||
			strings.HasPrefix(val, "auto-skip") {
			continue
		}
		missing = append(missing, name)
	}

	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf(
		"close-time Tier 2 revalidation failed: scope suite(s) applicable to current diff but not recorded: %s — "+
			"run `st test %s <suite> --run` (or `--skip \"<reason>\"`) for each, then re-close. "+
			"Use --skip-tier2-revalidation to bypass.",
		strings.Join(missing, ", "), item.ID,
	)
}

// scopeSuiteApplicable returns true when the suite should be checked given
// the set of repos that have changes and all changed file paths.
func scopeSuiteApplicable(sc config.ScopeSuiteConfig, reposWithChanges map[string]bool, changedFiles []string) bool {
	// repo_trigger: applicable when that specific repo has any changed files.
	if sc.RepoTrigger != "" && reposWithChanges[sc.RepoTrigger] {
		return true
	}
	// triggers: glob patterns — applicable when any changed file matches.
	// Uses autoGlobMatch (shared with test_auto.go) for consistent ** semantics,
	// including correct handling of patterns like **/*.go.
	for _, pattern := range sc.Triggers {
		for _, f := range changedFiles {
			if autoGlobMatch(pattern, f) {
				return true
			}
		}
	}
	return false
}

// scopeEvidence reads the testing evidence string for a suite from the item.
// Mirrors getTestingEvidence in internal/validate/gates.go (unexported there).
func scopeEvidence(item *model.Item, suite string) string {
	if item.TestingEvidence == nil {
		return ""
	}
	v, ok := item.TestingEvidence[suite]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
