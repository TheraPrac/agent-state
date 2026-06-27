package command

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/plan"
	"github.com/theraprac/agent-state/internal/store"
)

// ReviewTargetOpts holds injectable functions so the resolver is unit-testable
// without touching git or the network (nil = use the real git/gh).
type ReviewTargetOpts struct {
	// GhPRExists reports whether PR `num` exists in GitHub repo `slug` (owner/repo).
	GhPRExists func(slug string, num int) bool
	// GitRemoteURL returns the `origin` remote URL for a local repo dir.
	GitRemoteURL func(repoDir string) (string, error)
	// GitToplevel returns the git toplevel of dir (used to locate the workspace repo).
	GitToplevel func(dir string) (string, error)
	// ActiveScopeSlugs returns the active item's scope_repos as GitHub slugs, for
	// disambiguation. nil = derive from the agent stack + plan sidecar.
	ActiveScopeSlugs func() []string
}

// ReviewTarget resolves a bare PR number to a repo-qualified target (owner/repo#num)
// across the workspace repos, preferring the active item's scope_repos when more than
// one repo carries that PR number. It prints the qualified target to stdout on success;
// on ambiguity or no match it prints a diagnostic to stderr and returns non-zero. It
// NEVER guesses silently — that silent guess is the I-1616 bug this command fixes.
func ReviewTarget(s *store.Store, cfg *config.Config, num int, opts ReviewTargetOpts) int {
	if num <= 0 {
		fmt.Fprintln(os.Stderr, "review-target: a positive PR number is required")
		return 2
	}

	if opts.GitRemoteURL == nil {
		opts.GitRemoteURL = func(dir string) (string, error) { return runGit(dir, "remote", "get-url", "origin") }
	}
	if opts.GitToplevel == nil {
		opts.GitToplevel = func(dir string) (string, error) { return runGit(dir, "rev-parse", "--show-toplevel") }
	}
	if opts.GhPRExists == nil {
		opts.GhPRExists = func(slug string, n int) bool {
			// Use the REST endpoint: it 404s (non-zero exit) for a missing PR.
			// `gh pr view <n> --json number` is unreliable here — it echoes the
			// requested number with exit 0 even when the PR does not exist.
			cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/pulls/%d", slug, n))
			return cmd.Run() == nil
		}
	}

	// Enumerate candidate repo slugs (deduped) from each workspace repo's origin remote.
	var slugs []string
	seen := map[string]bool{}
	for _, dir := range candidateRepoDirs(cfg, opts.GitToplevel) {
		url, err := opts.GitRemoteURL(dir)
		if err != nil {
			continue
		}
		slug, err := parseGitHubSlug(strings.TrimSpace(url))
		if err != nil || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	if len(slugs) == 0 {
		fmt.Fprintln(os.Stderr, "review-target: could not resolve any workspace repo slug (is `origin` set / gh authenticated?)")
		return 1
	}

	scope := opts.ActiveScopeSlugs
	var scopeSlugs []string
	if scope != nil {
		scopeSlugs = scope()
	} else {
		scopeSlugs = activeScopeSlugs(s, cfg, opts.GitRemoteURL)
	}

	target, err := selectTarget(num, slugs, scopeSlugs, opts.GhPRExists)
	if err != nil {
		fmt.Fprintln(os.Stderr, "review-target: "+err.Error())
		return 1
	}
	fmt.Println(target)
	return 0
}

// selectTarget queries which candidate slugs carry PR #num and applies the
// disambiguation policy. The ghExists call is injected so this is unit-testable.
func selectTarget(num int, slugs, scopeSlugs []string, ghExists func(string, int) bool) (string, error) {
	var matches []string
	for _, slug := range slugs {
		if ghExists(slug, num) {
			matches = append(matches, slug)
		}
	}
	return pickReviewTarget(num, matches, scopeSlugs)
}

// candidateRepoDirs returns the local directories of every repo a PR could live in:
// the configured worktree repos plus the workspace repo itself (which holds workspace
// PRs and is deliberately absent from worktree.repos).
func candidateRepoDirs(cfg *config.Config, gitToplevel func(string) (string, error)) []string {
	var dirs []string
	if cfg != nil && cfg.Worktree != nil {
		for _, r := range cfg.Worktree.Repos {
			dirs = append(dirs, resolveRepoDir(cfg, r))
		}
	}
	if cfg != nil {
		if top, err := gitToplevel(cfg.Root()); err == nil {
			if t := strings.TrimSpace(top); t != "" {
				dirs = append(dirs, t)
			}
		}
	}
	return dirs
}

// activeScopeSlugs derives the active item's scope_repos (top of the agent stack →
// plan sidecar) as GitHub slugs, for disambiguation. Returns nil when there is no
// active item, no plan, or no scope_repos — in which case an ambiguous PR number
// correctly errors out rather than being guessed.
func activeScopeSlugs(s *store.Store, cfg *config.Config, gitRemoteURL func(string) (string, error)) []string {
	stack := LoadStack(cfg)
	if len(stack) == 0 {
		return nil
	}
	p, err := plan.Load(cfg.PlansDir(), stack[0].ID)
	if err != nil || p == nil {
		return nil
	}
	var out []string
	for _, r := range p.ScopeRepos {
		url, err := gitRemoteURL(resolveRepoDir(cfg, r))
		if err != nil {
			continue
		}
		if slug, err := parseGitHubSlug(strings.TrimSpace(url)); err == nil {
			out = append(out, slug)
		}
	}
	return out
}

// pickReviewTarget applies the disambiguation policy. Pure (no IO) for testability.
func pickReviewTarget(num int, matches, scopeSlugs []string) (string, error) {
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no PR #%d found in any workspace repo — pass a repo-qualified target (owner/repo#num)", num)
	case 1:
		return fmt.Sprintf("%s#%d", matches[0], num), nil
	}
	// Multiple repos carry this PR number — try the active item's scope_repos.
	inScope := intersect(matches, scopeSlugs)
	if len(inScope) == 1 {
		return fmt.Sprintf("%s#%d", inScope[0], num), nil
	}
	return "", fmt.Errorf("PR #%d is ambiguous across repos [%s] — pass a repo-qualified target (owner/repo#%d)",
		num, strings.Join(matches, ", "), num)
}

// parseGitHubSlug extracts owner/repo from a git origin URL (git@ or https form).
func parseGitHubSlug(url string) (string, error) {
	u := strings.TrimSuffix(strings.TrimSpace(url), ".git")
	switch {
	case strings.Contains(u, "github.com:"): // git@github.com:owner/repo
		u = u[strings.Index(u, "github.com:")+len("github.com:"):]
	case strings.Contains(u, "github.com/"): // https://github.com/owner/repo
		u = u[strings.Index(u, "github.com/")+len("github.com/"):]
	default:
		return "", fmt.Errorf("not a github remote: %q", url)
	}
	u = strings.Trim(u, "/")
	if parts := strings.Split(u, "/"); len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "/" + parts[1], nil
	}
	return "", fmt.Errorf("could not parse owner/repo from %q", url)
}

// intersect returns elements of a that are also in b, order preserved.
func intersect(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	var out []string
	for _, x := range a {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}
