package command

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// PRCreateOpts holds flags and injectable dependencies for `st pr create`.
type PRCreateOpts struct {
	Repo     string
	Base     string // default "main"
	Title    string
	Body     string
	BodyFile string
	Draft    bool

	// Injectable for testing (nil = use real implementations).
	RunGh        func(args []string) (string, error) // runs `gh <args...>`, returns combined stdout
	GitRemoteURL func(repoDir string) (string, error)
	GitBranch    func(repoDir string) (string, error)
	GitHeadSHA   func(repoDir string) (string, error)
	// Forwarded to PR() for manifest analysis (nil = real git).
	GitNameStatus func(repoDir string) (string, error)
	GitNumstat    func(repoDir string) (string, error)
	GitBlobHash   func(repoDir, path string) (string, error)
	FileExists    func(path string) bool
}

// PRCreate is the PERFORMING counterpart to `st pr` (which only records). It opens
// the PR via `gh pr create` and then records the manifest via PR(), so the whole
// PR-open step runs through st with its gates and stage advancement intact.
//
// Because PreToolUse hooks intercept only Claude's Bash *tool* calls (never st's
// own subprocess exec of gh), the two existing raw-`gh pr create` guards
// (pre-pr-live-acceptance-guard.sh, pre-pr-review-evidence-guard.sh) would NOT
// fire for the gh invocation done here. To avoid silently weakening those gates,
// the non-draft path re-checks the review gate before creating the PR:
//   - testing_evidence.live_acceptance is present (any value), and
//   - review_evidence passes (or a failing verdict is covered by review_skips)
//     and its SHA matches HEAD (via ReviewCheck).
//
// Note: the raw-gh shell hooks (pre-pr-review-evidence-guard.sh) read
// review_evidence directly and do not yet honor review_skips — an item with a
// fail verdict and review_skips set will pass st pr create but be blocked by
// the shell hook on raw `gh pr create`. Tracked for fix in I-1628 phase 2b.
//
// `--draft` skips both gates, matching both hooks' `--draft` bypass (the
// iterate-without-bots flow; the gates re-apply at `gh pr ready`).
func PRCreate(s *store.Store, cfg *config.Config, id string, opts PRCreateOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active to create a PR\n", id, item.Status)
		return 1
	}
	if opts.Repo == "" {
		fmt.Fprintln(os.Stderr, "--repo is required")
		return 2
	}
	if opts.Title == "" {
		fmt.Fprintln(os.Stderr, "--title is required")
		return 2
	}
	if opts.Body == "" && opts.BodyFile == "" {
		fmt.Fprintln(os.Stderr, "one of --body or --body-file is required")
		return 2
	}

	// Gate the non-draft path so routing through st never bypasses the two
	// PreToolUse guards that protect raw `gh pr create`.
	if !opts.Draft {
		if rc := liveAcceptanceCheck(item, id); rc != 0 {
			return rc
		}
		if rc := ReviewCheck(s, cfg, id, ReviewCheckOpts{GitHeadSHA: opts.GitHeadSHA}); rc != 0 {
			// ReviewCheck already printed the specific reason.
			return rc
		}
	}

	base := opts.Base
	if base == "" {
		base = "main"
	}

	repoDir := resolveRepoDirForItem(cfg, id, opts.Repo)

	// Resolve the GitHub slug from the repo's origin remote (no hardcoded map).
	gitRemoteURL := opts.GitRemoteURL
	if gitRemoteURL == nil {
		gitRemoteURL = func(dir string) (string, error) { return runGit(dir, "remote", "get-url", "origin") }
	}
	slug, err := slugForRepo(cfg, opts.Repo, gitRemoteURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: could not resolve GitHub slug for repo %q: %v\n", id, opts.Repo, err)
		return 1
	}

	// Head branch = the current branch in the item's worktree for this repo.
	gitBranch := opts.GitBranch
	if gitBranch == nil {
		gitBranch = func(dir string) (string, error) { return runGit(dir, "rev-parse", "--abbrev-ref", "HEAD") }
	}
	branchOut, err := gitBranch(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: could not resolve current branch in %s: %v\n", id, repoDir, err)
		return 1
	}
	branch := strings.TrimSpace(branchOut)
	if branch == "" || branch == "HEAD" {
		fmt.Fprintf(os.Stderr, "%s: refusing to open a PR from detached/empty HEAD in %s\n", id, repoDir)
		return 1
	}

	args := []string{"pr", "create", "-R", slug, "--base", base, "--head", branch, "--title", opts.Title}
	if opts.BodyFile != "" {
		args = append(args, "--body-file", opts.BodyFile)
	} else {
		args = append(args, "--body", opts.Body)
	}
	if opts.Draft {
		args = append(args, "--draft")
	}

	runGh := opts.RunGh
	if runGh == nil {
		runGh = func(a []string) (string, error) {
			cmd := exec.Command("gh", a...)
			cmd.Dir = repoDir
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
	}
	out, err := runGh(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: gh pr create failed: %v\n%s\n", id, err, out)
		return 1
	}

	// From here on the PR EXISTS on GitHub (gh succeeded). Any later failure must
	// NOT read as a create failure — surface that the PR is live and the exact
	// command to finish recording, so a re-run of `st pr create` doesn't just hit
	// "a PR already exists" with no breadcrumb (self-diagnosing per operator profile).
	prURL := strings.TrimSpace(out)
	prNum := parsePRNumberFromCreateOutput(out)
	if prNum == 0 {
		fmt.Fprintf(os.Stderr, "%s: PR was OPENED (%s) but its number could not be parsed from gh output — the PR EXISTS; finish recording with: st pr %s --repo %s --pr <N>\n", id, prURL, id, opts.Repo)
		return 1
	}

	fmt.Printf("%s: opened PR %s#%d (%s)\n", id, slug, prNum, prURL)

	// Record the manifest + advance the delivery stage to pr_open (reuse st pr).
	if rc := PR(s, cfg, id, PROpts{
		Repo:          opts.Repo,
		PRNumber:      prNum,
		GitNameStatus: opts.GitNameStatus,
		GitNumstat:    opts.GitNumstat,
		GitHeadSHA:    opts.GitHeadSHA,
		GitBlobHash:   opts.GitBlobHash,
		FileExists:    opts.FileExists,
	}); rc != 0 {
		fmt.Fprintf(os.Stderr, "%s: PR %s#%d was OPENED but recording its manifest failed (see above). The PR EXISTS — finish recording with: st pr %s --repo %s --pr %d\n", id, slug, prNum, id, opts.Repo, prNum)
		return rc
	}
	return 0
}

// liveAcceptanceCheck mirrors pre-pr-live-acceptance-guard.sh: the item must
// carry a testing_evidence.live_acceptance entry (any value — pass/fail/skip).
// Returns 0 on pass, 1 on fail (with the same guidance the hook prints). Kept as
// a helper so the Go side is single-sourced (parallel to ReviewCheck) and phase
// 2b's blocking guard can reuse it.
func liveAcceptanceCheck(item *model.Item, id string) int {
	if v, _ := getNestedField(item, "testing_evidence", "live_acceptance"); strings.TrimSpace(v) != "" {
		return 0
	}
	fmt.Fprintf(os.Stderr,
		"%s: no testing_evidence.live_acceptance — exercise the binary against real deps before opening a PR (CLAUDE.md #15). Record with `st test %s live_acceptance --run` (or `--skip \"<reason>\"`), or use --draft to iterate.\n",
		id, id)
	return 1
}

// parsePRNumberFromCreateOutput extracts the PR number from `gh pr create`'s
// output, which prints the new PR's URL (…/pull/<n>) as the FINAL line. We keep
// the LAST numeric /pull/<n> match (not the first): gh may emit an earlier
// numeric /pull/<n> reference (a linked/superseded-PR notice, a body echo) before
// the created-PR URL, and the created URL is always last. (The "Create a pull
// request … /pull/new/<branch>" hint is naturally skipped — "new" fails Atoi.)
// Reuses the same /pull/<n> URL shape parsed elsewhere (reconcile.go).
func parsePRNumberFromCreateOutput(out string) int {
	result := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "/pull/") {
			continue
		}
		if _, _, prStr := parsePRURL(line); prStr != "" {
			if n, err := strconv.Atoi(prStr); err == nil {
				result = n
			}
		}
	}
	return result
}
