package command

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// AutoTest detects changed files in the item's worktree repos and runs all
// applicable Tier 1 (required) and Tier 2 (scope) suites in order.
func AutoTest(s *store.Store, cfg *config.Config, id string, opts TestRecordOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active to run tests\n", id, item.Status)
		return 1
	}
	if cfg.Testing == nil {
		fmt.Fprintln(os.Stderr, "no testing configuration found")
		return 1
	}
	if cfg.Worktree == nil || len(cfg.Worktree.Repos) == 0 {
		fmt.Fprintln(os.Stderr, "--auto requires worktree.repos to be configured")
		return 1
	}

	// Mirror the classOK guard in TestRecord (testrecord.go:83) — an unknown
	// scope_class must fail loudly here too, not silently produce zero suites.
	if _, classOK := cfg.Testing.RequiredSuitesFor(item.ScopeClass); !classOK {
		fmt.Fprintf(os.Stderr, "unknown scope_class %q — declare in config.testing.scope_classes or remove from item\n", item.ScopeClass)
		return 1
	}

	touched, err := detectTouchedRepos(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detecting changes: %v\n", err)
		return 1
	}

	tier1, tier2 := selectAutoSuites(cfg, item, touched)
	all := append(tier1, tier2...)

	// Write auto-skip evidence for required suites whose repo had no changes.
	// This satisfies the testing_complete gate without requiring the operator
	// to run irrelevant suites or use --skip (which is blocked on required suites).
	if code := autoRecordSkips(s, cfg, id, item, touched); code != 0 {
		return code
	}

	if len(all) == 0 {
		fmt.Printf("[auto] %s — no applicable suites (touched repos: %s)\n", id, descTouched(touched))
		return 0
	}

	fmt.Printf("[auto] %s — %d suite(s): %s\n", id, len(all), strings.Join(all, ", "))

	var failed []string
	for i, suite := range all {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(all), suite)
		code := TestRecord(s, cfg, id, suite, TestRecordOpts{
			Run:        true,
			Agent:      opts.Agent,
			Cwd:        opts.Cwd,
			GitHeadSHA: opts.GitHeadSHA,
			RunCmd:     opts.RunCmd,
			ReadFile:   opts.ReadFile,
			Backend:    opts.Backend,
		})
		if code != 0 {
			failed = append(failed, suite)
		}
	}

	fmt.Println()
	if len(failed) == 0 {
		fmt.Printf("[auto] All %d suite(s) passed.\n", len(all))
		return 0
	}
	fmt.Printf("[auto] %d/%d suite(s) failed: %s\n", len(failed), len(all), strings.Join(failed, ", "))
	return 1
}

// autoRecordSkips writes "auto-skip: no files changed in <repo>" evidence for
// each required or scope suite whose repo had no changes in this worktree. This
// allows the testing_complete gate to pass without running suites that don't
// apply. Unlike user --skip, auto-skip is a system determination — it bypasses
// the user-skip guard in TestRecord intentionally.
func autoRecordSkips(s *store.Store, cfg *config.Config, id string, item *model.Item, touched map[string][]string) int {
	if item.ScopeClass != "" {
		// Class items have a fixed required-suite set; no auto-skip applies.
		return 0
	}
	now := time.Now().Format(time.RFC3339)
	var skipped []string

	recordSkip := func(name, repo string) bool {
		ev := fmt.Sprintf("auto-skip: no files changed in %s", repo)
		if err := s.Mutate(id, func(it *model.Item) error {
			it.SetNested("testing_evidence", name, ev)
			it.Doc.SetField("last_touched", now)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "recording auto-skip for %s: %v\n", name, err)
			return false
		}
		changelog.Append(cfg, id, changelog.Entry{
			Op: "test_skipped", Field: "testing_evidence." + name, NewValue: ev,
		})
		skipped = append(skipped, name)
		return true
	}

	requiredSuites, _ := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	for name := range requiredSuites {
		repo := autoScopeRepo(name)
		if _, changed := touched[repo]; changed || repo == "" {
			continue // suite applies (will run) or prefix unmapped (don't auto-skip)
		}
		if !recordSkip(name, repo) {
			return 1
		}
	}

	// Scope suites: auto-skip any suite whose scoped repo has no changes.
	// Suites with trigger patterns but no prefix mapping (repo == "") are
	// omitted silently — no UAT gate pressure for those.
	for name := range cfg.Testing.ScopeSuites {
		if name == "live_acceptance" {
			continue // manual gate — never auto-skipped
		}
		repo := autoScopeRepo(name)
		if repo == "" {
			continue // prefix unmapped — can't determine applicability
		}
		if _, changed := touched[repo]; changed {
			continue // repo changed — suite will run (or trigger didn't match, but not our call)
		}
		if !recordSkip(name, repo) {
			return 1
		}
	}

	if len(skipped) > 0 {
		sort.Strings(skipped)
		fmt.Printf("[auto] auto-skipped (no repo changes): %s\n", strings.Join(skipped, ", "))
		if err := autoSync(s, fmt.Sprintf("st test --auto: %s auto-skip", id)); err != nil {
			return 1
		}
	}
	return 0
}

// resolveRepoDirForAuto resolves the git directory for a per-item auto-test
// diff check using Pattern 1 only (no cross-item fallback).
//
// resolveRepoDirForItem has broad fallback patterns 2 & 3 that scan the entire
// WorktreeBase(), which can return another item's clone when the requested repo
// is absent from the current item's worktree. For auto-test diff purposes that
// cross-item contamination is wrong: a diff of I-1302's theraprac-api produces
// I-1302's changes, causing incorrect auto-skip decisions for the current item.
// I-1473.
//
// Resolution rules:
//   - No worktree config or WorktreeForItem returns "": fall back to the main
//     checkout (resolveRepoDir). Item has no worktree; its scope is main-branch.
//   - Item worktree dir doesn't exist on disk: same main-checkout fallback.
//   - Item worktree dir exists but <repo> is absent: return "" (skip this repo;
//     do not cross to another item's clone).
//   - Item worktree dir exists and <repo> is a git dir: return that path.
func resolveRepoDirForAuto(cfg *config.Config, id, repo string) string {
	if cfg.Worktree == nil || cfg.Worktree.BaseDir == "" {
		return resolveRepoDir(cfg, repo)
	}
	wtBase := cfg.WorktreeForItem(id)
	if wtBase == "" {
		return resolveRepoDir(cfg, repo)
	}
	if _, err := os.Stat(wtBase); err != nil {
		// Item has no worktree on disk; diff against the main checkout.
		return resolveRepoDir(cfg, repo)
	}
	// Item worktree exists. Pattern 1: <worktree>/<repo>.
	candidate := filepath.Join(wtBase, repo)
	if isGitDir(candidate) {
		return candidate
	}
	if cfg.Worktree.RepoMap != nil {
		if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
			if c := filepath.Join(wtBase, mapped); isGitDir(c) {
				return c
			}
		}
	}
	// Worktree exists but repo is absent — skip rather than fall through to
	// Pattern 2/3, which would find another item's clone.
	return ""
}

// detectTouchedRepos runs `git diff main..HEAD --name-only` in each configured
// repo's worktree directory and returns the changed file list per repo name.
func detectTouchedRepos(cfg *config.Config, id string) (map[string][]string, error) {
	result := make(map[string][]string)
	for _, repo := range cfg.Worktree.Repos {
		dir := resolveRepoDirForAuto(cfg, id, repo)
		if dir == "" {
			continue // repo absent from item worktree — skip without cross-item fallback
		}
		if !isGitDir(dir) {
			continue
		}
		// Use merge-base(origin/main, HEAD) as the diff anchor — same as
		// ComputeFileChanges — so stale local main refs don't skew the result.
		// Fall back to main..HEAD if origin/main is unavailable (offline/no remote).
		base, err := runGit(dir, "merge-base", "origin/main", "HEAD")
		var out string
		if err == nil {
			out, err = runGit(dir, "diff", strings.TrimSpace(base)+"..HEAD", "--name-only")
		} else {
			out, err = runGit(dir, "diff", "main..HEAD", "--name-only")
		}
		if err != nil {
			// Silently skip — repo may have no divergence or no main ref.
			continue
		}
		var files []string
		for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
			if f != "" {
				files = append(files, f)
			}
		}
		if len(files) > 0 {
			result[repo] = files
		}
	}
	return result, nil
}

// selectAutoSuites returns (tier1, tier2) suite lists based on which repos have
// changes and whether the item has a scope_class override.
//
//   - Tier 1 (required suites): filtered by suite-name prefix → repo mapping,
//     unless scope_class is set (class suites run regardless of file scope).
//   - Tier 2 (scope suites): filtered by prefix AND trigger glob patterns.
//   - live_acceptance is always excluded — it is a manual gate, not auto-runnable.
func selectAutoSuites(cfg *config.Config, item *model.Item, touched map[string][]string) (tier1, tier2 []string) {
	requiredSuites, _ := cfg.Testing.RequiredSuitesFor(item.ScopeClass)

	seen := map[string]bool{}

	for name := range requiredSuites {
		if seen[name] {
			continue
		}
		if item.ScopeClass != "" {
			// Class suites are defined by item type, not by which files changed.
			seen[name] = true
			tier1 = append(tier1, name)
			continue
		}
		repo := autoScopeRepo(name)
		if _, changed := touched[repo]; changed && repo != "" {
			seen[name] = true
			tier1 = append(tier1, name)
		}
	}
	sort.Strings(tier1)

	for name, sc := range cfg.Testing.ScopeSuites {
		if name == "live_acceptance" || seen[name] {
			continue
		}
		repo := autoScopeRepo(name)
		files, changed := touched[repo]
		if !changed || repo == "" {
			continue
		}
		if len(sc.Triggers) > 0 && !autoMatchTriggers(files, sc.Triggers) {
			continue
		}
		seen[name] = true
		tier2 = append(tier2, name)
	}
	sort.Strings(tier2)

	return tier1, tier2
}

// autoScopeRepo maps a suite name to its worktree repo directory name using the
// suite-name prefix convention: api_* → theraprac-api, web_* → theraprac-web, etc.
func autoScopeRepo(suiteName string) string {
	prefix, _, _ := strings.Cut(suiteName, "_")
	switch prefix {
	case "api":
		return "theraprac-api"
	case "web":
		return "theraprac-web"
	case "infra":
		return "theraprac-infra"
	case "workspace", "as":
		return "as"
	default:
		return ""
	}
}

// autoMatchTriggers returns true if any file matches any trigger glob pattern.
// Supports ** as "any path segment(s)": src/app/** matches src/app/foo/bar.ts.
func autoMatchTriggers(files []string, patterns []string) bool {
	for _, f := range files {
		for _, pattern := range patterns {
			if autoGlobMatch(pattern, f) {
				return true
			}
		}
	}
	return false
}

// autoGlobMatch matches a file path against a glob pattern with ** support.
func autoGlobMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	// Split on first **: check prefix (before **) and suffix (after **).
	before, after, _ := strings.Cut(pattern, "**")
	if before != "" && !strings.HasPrefix(name, before) {
		return false
	}
	if after == "" {
		return true
	}
	// Strip leading / from the suffix segment so "**/*.go" works.
	after = strings.TrimPrefix(after, "/")
	if after == "" {
		return true
	}
	ok, _ := filepath.Match(after, filepath.Base(name))
	if ok {
		return true
	}
	// Fall back to suffix matching for multi-segment suffixes (e.g. after =
	// "hooks/useFoo.ts" from pattern "src/**/hooks/useFoo.ts"). Use the /
	// separator form only — the bare HasSuffix(name, after) would false-positive
	// on "src/other-hooks/useFoo.ts" matching pattern "src/**/hooks/useFoo.ts".
	// Root-level files (no leading /) are caught by the filepath.Match branch above.
	return strings.HasSuffix(name, "/"+after)
}

// descTouched returns a short description of which repos were touched.
func descTouched(touched map[string][]string) string {
	if len(touched) == 0 {
		return "none"
	}
	var repos []string
	for repo := range touched {
		repos = append(repos, fmt.Sprintf("%s(%d)", repo, len(touched[repo])))
	}
	sort.Strings(repos)
	return strings.Join(repos, ", ")
}
