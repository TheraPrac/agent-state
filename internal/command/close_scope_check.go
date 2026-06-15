package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// Nil: uses the default I-407 lookup (agent-root/worktrees/<id>/<repo>).
	ResolveWorktree func(cfg *config.Config, itemID, repo string) string
	// RunGit runs git in dir with args and returns trimmed stdout.
	// Nil: uses a real git exec.
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
		resolveWt = defaultResolveWorktree
	}
	runGit := opts.RunGit
	if runGit == nil {
		runGit = defaultScopeRunGit
	}

	// Collect which repos have changes and all changed file paths.
	reposWithChanges := map[string]bool{}
	var changedFiles []string

	for _, repo := range cfg.Worktree.Repos {
		wt := resolveWt(cfg, item.ID, repo)
		if wt == "" {
			continue
		}
		out, err := runGit(wt, "diff", "--name-only", "origin/main...HEAD")
		if err != nil {
			continue // git unavailable or no remote — skip this repo
		}
		for _, f := range strings.Split(out, "\n") {
			if f == "" {
				continue
			}
			reposWithChanges[repo] = true
			changedFiles = append(changedFiles, f)
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
			strings.HasPrefix(val, "skip") ||
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
	for _, pattern := range sc.Triggers {
		for _, f := range changedFiles {
			if matchScopeGlob(pattern, f) {
				return true
			}
		}
	}
	return false
}

// matchScopeGlob matches a file path against a glob pattern, with support
// for `**` (match any sequence of path components). For example:
//
//	"src/app/**"       matches "src/app/page.tsx", "src/app/deep/file.ts"
//	"src/components/**" does NOT match "src/lib/utils.ts"
//	"*.go"             uses filepath.Match semantics (no ** expansion)
func matchScopeGlob(pattern, path string) bool {
	idx := strings.Index(pattern, "**")
	if idx < 0 {
		// No **: use filepath.Match for standard glob semantics.
		ok, _ := filepath.Match(pattern, path)
		return ok
	}
	// Split on the first `**`: everything before it is a required prefix.
	prefix := pattern[:idx]
	if prefix != "" && !strings.HasPrefix(path, prefix) {
		return false
	}
	// Everything after `**` (stripped of leading /) is an optional suffix.
	suffix := strings.TrimPrefix(pattern[idx+2:], "/")
	if suffix == "" {
		// "prefix/**": any file under that directory.
		return true
	}
	// "prefix/**/suffix": suffix must appear somewhere in the remaining path.
	remaining := path[len(prefix):]
	return strings.HasSuffix(remaining, suffix) || strings.Contains(remaining, "/"+suffix)
}

// scopeEvidence reads the testing evidence string for a suite from the item.
// Mirrors the getTestingEvidence helper in gates.go.
func scopeEvidence(item *model.Item, suite string) string {
	if item.TestingEvidence == nil {
		return ""
	}
	v, ok := item.TestingEvidence[suite]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// defaultResolveWorktree looks up the worktree path using the I-407 order:
//  1. <agent-root>/worktrees/<id>/<repo>
//  2. <workspace-root>/worktrees/<id>/<repo>  (legacy fallback)
func defaultResolveWorktree(cfg *config.Config, itemID, repo string) string {
	agentRoot := cfg.AgentRoot()
	candidates := []string{
		filepath.Join(agentRoot, "worktrees", itemID, repo),
	}
	// Legacy: workspace-root may differ from agent-root (pre-I-418 layout).
	wsRoot := cfg.Root()
	if wsRoot != agentRoot {
		candidates = append(candidates, filepath.Join(wsRoot, "worktrees", itemID, repo))
	}
	for _, c := range candidates {
		// Mirrors branchExistsAnywhere: .git file or dir is the existence check.
		if _, err := os.Stat(filepath.Join(c, ".git")); err == nil {
			return c
		}
	}
	return ""
}

// defaultScopeRunGit runs git in dir and returns trimmed stdout.
func defaultScopeRunGit(dir string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", dir}, args...) //nolint:gosec
	out, err := exec.Command("git", fullArgs...).Output()
	return strings.TrimSpace(string(out)), err
}
