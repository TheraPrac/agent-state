package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// MaintainOpts configures `st maintain`.
type MaintainOpts struct {
	DryRun bool
}

// Maintain performs self-service git hygiene on the workspace clone so the
// operator stops doing it by hand: reap redundant stashes, prune provably-merged
// feature branches (local + remote), and return the clone to a clean main when
// it's been left on an already-merged branch. Designed to run unattended from
// session-start: it never blocks (each phase prints and continues on error) and
// only ever touches things that are provably safe — merged branches, churn-only
// dirty trees. Returns 0 except on a hard failure to read the repo.
func Maintain(s *store.Store, cfg *config.Config, opts MaintainOpts) int {
	root := cfg.Root()
	mode := "apply"
	if opts.DryRun {
		mode = "dry-run"
	}
	if !isGitRepo(root) {
		fmt.Printf("st maintain: %s is not a git repo — nothing to do\n", root)
		return 0
	}
	fmt.Printf("st maintain (%s)\n", mode)

	// Fresh origin/main so ancestry/merge checks below are accurate.
	_ = gitCmdDirQuiet(root, "fetch", "origin", "main", "--quiet")

	reapStashes(root, opts)
	pruneMergedBranches(root, opts)
	returnToCleanMain(root, opts)
	return 0
}

// maintainChurnPrefixes are machine-managed / regenerated paths whose presence
// in a dirty tree is never real WIP — safe to leave behind a branch switch.
var maintainChurnPrefixes = []string{"agent-state/", ".as/", ".plans/", "agent-memory/"}

func maintainIsChurn(path string) bool {
	for _, p := range maintainChurnPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	base := filepath.Base(path)
	return base == "deploy-dashboard.html" || strings.HasSuffix(path, "dashboard-history.jsonl")
}

// ── Phase 1: stashes ────────────────────────────────────────────────────────

// reapStashes delegates to the tested workspace reaper so the classification
// (archive unique code as tags, drop the rest) lives in exactly one place.
func reapStashes(root string, opts MaintainOpts) {
	script := filepath.Join(root, "scripts", "prune-redundant-stashes.sh")
	if _, err := os.Stat(script); err != nil {
		fmt.Println("  stashes: reaper script not present — skipping")
		return
	}
	args := []string{script, "-C", root}
	if !opts.DryRun {
		args = append(args, "--apply") // script defaults to dry-run without this
	}
	cmd := exec.Command("bash", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("  stashes: reaper reported an error: %v\n", err)
	}
}

// ── Phase 2: merged-branch prune ────────────────────────────────────────────

func pruneMergedBranches(root string, opts MaintainOpts) {
	cur := currentBranch(root)
	mergedPR := mergedPRBranches(root) // squash-aware; empty if gh unavailable

	out, err := gitOutputDir(root, "branch", "--format=%(refname:short)")
	if err != nil {
		fmt.Printf("  branches: cannot list (%v)\n", err)
		return
	}
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" || b == "master" || b == cur {
			continue // never the current branch, never main/master
		}
		if !branchMerged(root, b, mergedPR) {
			continue // unmerged → could be peer/in-flight work; leave it
		}
		if opts.DryRun {
			fmt.Printf("  branch: would prune %s (merged)\n", b)
			continue
		}
		// -D is safe here: we have proven b is merged. A branch checked out in
		// another worktree makes git refuse (harmless).
		if err := gitCmdDirQuiet(root, "branch", "-D", b); err != nil {
			fmt.Printf("  branch: kept %s (local delete refused: %v)\n", b, err)
			continue
		}
		// Remote delete is best-effort: the branch may already be gone, and the
		// pre-push hook exempts deletes (and is bypassed entirely when on main).
		_ = gitCmdDirQuiet(root, "push", "origin", "--delete", b)
		fmt.Printf("  branch: pruned %s (local+remote)\n", b)
	}
}

// branchMerged reports whether b is provably contained in origin/main: either
// its tip is an ancestor (regular/ff merge) or GitHub reports its PR merged
// (squash/rebase merge, where the tip is NOT an ancestor).
func branchMerged(root, b string, mergedPR map[string]bool) bool {
	if gitCmdDirQuiet(root, "merge-base", "--is-ancestor", b, "origin/main") == nil {
		return true
	}
	return mergedPR[b]
}

// mergedPRBranches asks gh for head branches of merged PRs (squash-merge aware).
// Best-effort: empty map when gh is missing/unauthed/offline — callers then fall
// back to ancestor-only detection, which is still safe (just less complete).
func mergedPRBranches(root string) map[string]bool {
	m := map[string]bool{}
	slug := repoSlug(root)
	if slug == "" {
		return m
	}
	cmd := exec.Command("gh", "pr", "list", "--repo", slug, "--state", "merged",
		"--limit", "200", "--json", "headRefName", "-q", ".[].headRefName")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			m[line] = true
		}
	}
	return m
}

// ── Phase 3: return to clean main ───────────────────────────────────────────

// returnToCleanMain switches the clone back to main when it's been left on an
// already-merged branch with only agent-state churn dirtying the tree (the
// I-1313 case that makes a manual `git checkout main` fail). It never discards
// real WIP: any non-churn dirty path aborts the switch.
func returnToCleanMain(root string, opts MaintainOpts) {
	cur := currentBranch(root)
	if cur == "" || cur == "main" || cur == "master" {
		return
	}
	if !branchMerged(root, cur, mergedPRBranches(root)) {
		return // active unmerged work — leave the agent where they are
	}
	status, err := gitOutputDir(root, "status", "--porcelain")
	if err != nil {
		return
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if p := porcelainPath(line); p != "" && !maintainIsChurn(p) {
			fmt.Printf("  main: staying on %s — uncommitted non-churn change (%s)\n", cur, p)
			return
		}
	}
	if opts.DryRun {
		fmt.Printf("  main: would return to main from merged %s\n", cur)
		return
	}
	// The churn delta is already on main (I-1313 routing), so if checkout refuses
	// because of it, park it in a stash, switch, and drop (nothing is lost).
	if err := gitCmdDirQuiet(root, "checkout", "main"); err != nil {
		_ = gitCmdDirQuiet(root, "stash", "push", "-u", "-m", "st maintain: park churn")
		if err2 := gitCmdDirQuiet(root, "checkout", "main"); err2 != nil {
			fmt.Printf("  main: could not switch off %s (%v) — restoring stash\n", cur, err2)
			_ = gitCmdDirQuiet(root, "stash", "pop")
			return
		}
		_ = gitCmdDirQuiet(root, "stash", "drop")
	}
	fmt.Printf("  main: returned to main from merged %s\n", cur)
	// Now that cur isn't checked out, prune it too.
	_ = gitCmdDirQuiet(root, "branch", "-D", cur)
	_ = gitCmdDirQuiet(root, "push", "origin", "--delete", cur)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func currentBranch(root string) string {
	out, err := gitOutputDir(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func isGitRepo(root string) bool {
	return gitCmdDirQuiet(root, "rev-parse", "--git-dir") == nil
}

// repoSlug derives "owner/repo" from origin's URL (ssh or https), else "".
func repoSlug(root string) string {
	out, err := gitOutputDir(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(out)
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "github.com"); i >= 0 {
		rest := u[i+len("github.com"):]
		rest = strings.TrimLeft(rest, ":/")
		if strings.Count(rest, "/") >= 1 {
			return rest
		}
	}
	return ""
}

// porcelainPath extracts the (destination) path from a `git status --porcelain`
// line: "XY path" or rename "XY old -> new".
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	p := strings.TrimSpace(line[3:])
	if i := strings.Index(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return strings.Trim(p, "\"")
}

// gitCmdDirQuiet runs git in dir, discarding output; returns only the error
// (used for boolean checks like merge-base --is-ancestor and idempotent deletes).
func gitCmdDirQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
