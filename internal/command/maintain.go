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

	// Stash reaping is independent of origin/main, so it always runs.
	reapStashes(root, opts)

	// Branch pruning and the main-checkout both decide "merged?" against
	// origin/main — if we can't refresh it, fail CLOSED rather than act on a
	// stale ref (a branch could look merged against an old tip when it isn't).
	if err := gitCmdDirQuiet(root, "fetch", "origin", "main", "--quiet"); err != nil {
		fmt.Println("  branches: skipped — could not refresh origin/main (offline?); not acting on a stale ref")
		return 0
	}
	mergedPR := mergedPRHeads(root)
	pruneMergedBranches(root, mergedPR, opts)
	returnToCleanMain(root, mergedPR, opts)
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

func pruneMergedBranches(root string, mergedPR map[string][]prHead, opts MaintainOpts) {
	cur := currentBranch(root)
	if cur == "HEAD" {
		// Detached HEAD: there's no "current branch" to protect by name, so don't
		// risk deleting the branch the detached commit belongs to. Skip entirely.
		fmt.Println("  branches: skipped — detached HEAD")
		return
	}
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
		// -D is safe: branchMerged proved b is contained in origin/main. A branch
		// checked out in another worktree makes git refuse (harmless).
		if err := gitCmdDirQuiet(root, "branch", "-D", b); err != nil {
			fmt.Printf("  branch: kept %s (local delete refused: %v)\n", b, err)
			continue
		}
		pruneRemoteBranch(root, b)
		fmt.Printf("  branch: pruned %s\n", b)
	}
}

// pruneRemoteBranch removes origin/<b> best-effort, but does NOT stay silent on a
// real failure (operator requirement). An already-absent remote ref is success.
func pruneRemoteBranch(root, b string) {
	out, err := gitCombinedDir(root, "push", "origin", "--delete", b)
	if err == nil {
		return
	}
	low := strings.ToLower(out)
	if strings.Contains(low, "remote ref does not exist") || strings.Contains(low, "deleted") {
		return // already gone
	}
	fmt.Printf("  branch: %s deleted locally but remote delete did NOT complete: %s\n",
		b, strings.TrimSpace(out))
}

// branchMerged reports whether b is provably contained in origin/main.
//   - tip is an ancestor of origin/main → regular/ff merge.
//   - else the branch's CURRENT tip must equal (or be contained in) a head commit
//     GitHub recorded as merged under this exact name → squash/rebase merge.
//
// Requiring the tip to match the merged head OID is what makes name reuse safe: a
// later branch that reuses a merged name but carries new commits has a different
// tip, matches nothing, and is kept.
func branchMerged(root, b string, mergedPR map[string][]prHead) bool {
	if gitCmdDirQuiet(root, "merge-base", "--is-ancestor", b, "origin/main") == nil {
		return true
	}
	heads := mergedPR[b]
	if len(heads) == 0 {
		return false
	}
	tipOut, err := gitOutputDir(root, "rev-parse", b)
	if err != nil {
		return false
	}
	tip := strings.TrimSpace(tipOut)
	for _, h := range heads {
		if h.oid == tip {
			return true
		}
		// Local branch is an older ancestor of the merged head (oid must be a
		// local object; if not, this errors → false → falls through).
		if gitCmdDirQuiet(root, "merge-base", "--is-ancestor", tip, h.oid) == nil {
			return true
		}
	}
	// The branch drifted past its merged PR head(s) — almost always post-merge
	// sync merges and `st sync` agent-state churn that accumulated after the PR
	// merged. Safe to prune ONLY if EVERY non-merge commit it carries beyond
	// origin/main and the merged PR heads touches nothing but churn. A reused
	// name with real new code has a non-churn commit here → kept (data-safe).
	return branchExtraIsChurnOnly(root, b, heads)
}

// branchExtraIsChurnOnly fetches the merged PR head(s) for b (GitHub exposes
// refs/pull/<n>/head for every PR, even after the branch is deleted) and reports
// whether every non-merge commit on b that is NOT in origin/main and NOT in those
// heads touches only churn paths. Empty set ⇒ true. Any non-churn commit, or any
// inability to verify (fetch/parse failure), ⇒ false (keep). Network-bound, so
// it's only reached after the cheap ancestor/OID checks miss.
func branchExtraIsChurnOnly(root, b string, heads []prHead) bool {
	excludes := []string{"origin/main"}
	for _, h := range heads {
		if err := gitCmdDirQuiet(root, "fetch", "origin", "refs/pull/"+h.num+"/head"); err != nil {
			return false
		}
		shaOut, err := gitOutputDir(root, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return false
		}
		excludes = append(excludes, strings.TrimSpace(shaOut))
	}
	// Non-merge orphan commits: classify their full diff.
	nm := append([]string{"rev-list", "--no-merges", b, "--not"}, excludes...)
	out, err := gitOutputDir(root, nm...)
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(out) {
		files, err := gitOutputDir(root, "show", "--name-only", "--format=", c)
		if err != nil || hasNonChurn(files) {
			return false // real code (or unverifiable) beyond the merge → keep
		}
	}
	// Merge orphan commits: a clean sync-merge only brings in parent content
	// (already in origin/main or the merged head, hence excluded above). Only an
	// "evil merge" — changes made IN the merge itself, differing from ALL parents
	// (e.g. a hand-edited conflict resolution) — carries unique work, and
	// `diff-tree --cc` surfaces exactly those while staying empty for clean
	// merges. Any non-churn there → keep, so evil-merge work is never lost.
	mg := append([]string{"rev-list", "--merges", b, "--not"}, excludes...)
	mout, err := gitOutputDir(root, mg...)
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(mout) {
		files, err := gitOutputDir(root, "diff-tree", "--cc", "--no-commit-id", "--name-only", "-r", c)
		if err != nil || hasNonChurn(files) {
			return false
		}
	}
	return true
}

// hasNonChurn reports whether a newline-separated `--name-only` file list
// contains any path that isn't machine-managed churn.
func hasNonChurn(nameOnly string) bool {
	for _, f := range strings.Split(nameOnly, "\n") {
		if f = strings.TrimSpace(f); f != "" && !maintainIsChurn(f) {
			return true
		}
	}
	return false
}

// prHead is a merged PR's head: the GitHub PR number (to fetch refs/pull/<n>/head)
// and the head commit OID recorded as merged.
type prHead struct {
	num string
	oid string
}

// mergedPRHeads maps each merged PR's head branch name → its merged head(s).
// Best-effort: empty when gh is missing/unauthed/offline (callers then fall back
// to ancestor-only detection, still safe). Keyed by name with OID+number so
// branchMerged can reject a reused name and verify drifted branches.
func mergedPRHeads(root string) map[string][]prHead {
	m := map[string][]prHead{}
	slug := repoSlug(root)
	if slug == "" {
		return m
	}
	cmd := exec.Command("gh", "pr", "list", "--repo", slug, "--state", "merged",
		"--limit", "300", "--json", "headRefName,headRefOid,number",
		"-q", `.[] | .headRefName + " " + .headRefOid + " " + (.number|tostring)`)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 {
			m[f[0]] = append(m[f[0]], prHead{oid: f[1], num: f[2]})
		}
	}
	return m
}

// ── Phase 3: return to clean main ───────────────────────────────────────────

// returnToCleanMain switches the clone back to main when it's been left on an
// already-merged branch with only agent-state churn dirtying the tree (the
// I-1313 case that makes a manual `git checkout main` fail). It never discards
// real WIP: any non-churn dirty path aborts, and the stash it parks is dropped
// only after re-confirming it holds nothing but churn.
func returnToCleanMain(root string, mergedPR map[string][]prHead, opts MaintainOpts) {
	cur := currentBranch(root)
	if cur == "" || cur == "main" || cur == "master" || cur == "HEAD" {
		return // not on a deletable branch (incl. detached HEAD)
	}
	if !branchMerged(root, cur, mergedPR) {
		return // active unmerged work — leave the agent where they are
	}
	if !dirtyTreeIsAllChurn(root) {
		fmt.Printf("  main: staying on %s — uncommitted non-churn changes present\n", cur)
		return
	}
	if opts.DryRun {
		fmt.Printf("  main: would return to main from merged %s\n", cur)
		return
	}
	// The churn delta is already on main (I-1313 routing), so if checkout refuses
	// because of it, park it in a stash, switch, and drop. Before dropping we
	// RE-verify the stash holds only churn (a concurrent writer could have added a
	// real file between the scan above and the stash) — if not, keep it.
	if err := gitCmdDirQuiet(root, "checkout", "main"); err != nil {
		if perr := gitCmdDirQuiet(root, "stash", "push", "-u", "-m", "st maintain: park churn"); perr != nil {
			fmt.Printf("  main: could not park churn off %s (%v) — leaving as-is\n", cur, perr)
			return
		}
		if err2 := gitCmdDirQuiet(root, "checkout", "main"); err2 != nil {
			fmt.Printf("  main: could not switch off %s (%v) — restoring parked changes\n", cur, err2)
			_ = gitCmdDirQuiet(root, "stash", "pop")
			return
		}
		if stashIsAllChurn(root) {
			_ = gitCmdDirQuiet(root, "stash", "drop")
		} else {
			fmt.Println("  main: parked stash holds non-churn changes — kept (recover via git stash list)")
		}
	}
	fmt.Printf("  main: returned to main from merged %s\n", cur)
	if err := gitCmdDirQuiet(root, "branch", "-D", cur); err == nil {
		pruneRemoteBranch(root, cur)
	}
}

// dirtyTreeIsAllchurn reports whether every path in `git status --porcelain`
// is machine-managed churn. A line we cannot confidently parse (git C-quoted
// path) is treated as NON-churn — fail safe toward keeping the tree.
func dirtyTreeIsAllChurn(root string) bool {
	status, err := gitOutputDir(root, "status", "--porcelain")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p := porcelainPath(line)
		if p == "" || !maintainIsChurn(p) {
			return false
		}
	}
	return true
}

// stashIsAllChurn checks the top stash (incl. its untracked tree) holds only churn.
func stashIsAllChurn(root string) bool {
	out, err := gitOutputDir(root, "stash", "show", "--include-untracked", "--name-only", "stash@{0}")
	if err != nil {
		return false // can't confirm → don't drop
	}
	any := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		any = true
		if !maintainIsChurn(line) {
			return false
		}
	}
	return any
}

// ── helpers ─────────────────────────────────────────────────────────────────

func currentBranch(root string) string {
	out, err := gitOutputDir(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out) // "HEAD" when detached
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
	u := strings.TrimSuffix(strings.TrimSpace(out), ".git")
	if i := strings.Index(u, "github.com"); i >= 0 {
		rest := strings.TrimLeft(u[i+len("github.com"):], ":/")
		if strings.Count(rest, "/") >= 1 {
			return rest
		}
	}
	return ""
}

// porcelainPath extracts the (destination) path from a `git status --porcelain`
// line: "XY path" or rename "XY old -> new". A C-quoted path (spaces/special
// bytes → the line contains a double quote) returns "" so the caller fails safe.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	if strings.Contains(line, "\"") {
		return "" // C-quoted; don't risk mis-parsing → treat as non-churn
	}
	p := strings.TrimSpace(line[3:])
	if i := strings.Index(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return p
}

// gitCmdDirQuiet runs git in dir, discarding output; returns only the error
// (used for boolean checks and idempotent deletes).
func gitCmdDirQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// gitCombinedDir runs git in dir and returns combined stdout+stderr plus error,
// for cases where the failure text must be surfaced (not swallowed).
func gitCombinedDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
