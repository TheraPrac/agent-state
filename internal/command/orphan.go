package command

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/store"
)

// OrphanStash scans the workspace for dirty agent-state files not owned by
// agentID, stashes each one with a recoverable label, and prints a banner
// block describing what was stashed. Files owned by agentID or with no
// assigned_to field are left untouched.
//
// T-403: belt-and-braces companion to T-401 (ownership gate) and T-402
// (atomic writes). Handles crash leftovers that bypass both.
func OrphanStash(workspaceRoot, itemDir, agentID string) []string {
	var stashed []string

	out, err := execGitOrphan(workspaceRoot, "status", "--porcelain", "--", itemDir+"/")
	if err != nil || len(out) == 0 {
		return nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		// Skip untracked files — git stash push exits 1 for untracked paths
		// without --include-untracked. We only stash tracked dirty files.
		if line[0:2] == "??" {
			continue
		}
		relPath := strings.TrimSpace(line[3:])
		if relPath == "" {
			continue
		}

		owner := readAssignedTo(filepath.Join(workspaceRoot, relPath))

		// Leave files with no owner or owned by the current agent.
		if owner == "" || owner == agentID {
			continue
		}

		label := fmt.Sprintf("st-orphan: %s owned-by:%s dropped-by:%s date:%s",
			relPath, owner, agentID, today)
		_, stashErr := execGitOrphanCapture(workspaceRoot, "stash", "push",
			"-m", label, "--", relPath)
		if stashErr != nil {
			fmt.Fprintf(os.Stderr, "orphan: failed to stash %s: %v\n", relPath, stashErr)
			continue
		}
		// git stash push output is a human message, not a ref. Retrieve the real ref.
		ref := "stash@{?}"
		if refOut, err := execGitOrphan(workspaceRoot, "stash", "list",
			"--max-count=1", "--format=%gd"); err == nil {
			ref = strings.TrimSpace(string(refOut))
		}
		stashed = append(stashed, fmt.Sprintf("  %s → %s (owned by %s)", relPath, ref, owner))
	}

	if len(stashed) > 0 {
		fmt.Printf("orphan: stashed %d file(s) not owned by %s:\n", len(stashed), agentID)
		for _, s := range stashed {
			fmt.Println(s)
		}
		fmt.Printf("  recover: git -C %q stash show <ref> | git -C %q stash apply <ref>\n",
			workspaceRoot, workspaceRoot)
		fmt.Printf("  list: st orphan list --workspace %q\n", workspaceRoot)
	}
	return stashed
}

// StashPullConflicts is the REACTIVE, PRECISE half of I-1620: it stashes ONLY
// the untracked files an incoming fast-forward merge would clobber, so a
// session-start `git pull --ff-only` on the shared theraprac-workspace main
// checkout can be retried instead of failing and leaving the session stale.
//
// It runs ONLY after a pull has already failed (session-start.sh calls it then
// retries once). The collision set is computed deterministically — never by
// parsing git's locale-dependent "would be overwritten by merge" English:
//
//	A = paths ADDED in origin/<branch> that are absent from HEAD
//	    (git diff --diff-filter=A --no-renames --name-only HEAD origin/<branch>)
//	U = locally UNTRACKED paths (git status --porcelain, code "??")
//	collisions = A ∩ U   ← exactly the untracked files the ff-merge can't write over
//
// The intersection is case-insensitive (the operator is on macOS/APFS, a
// case-insensitive FS: origin adding `docs/New.md` collides with a local
// untracked `docs/new.md`). Only that intersection is stashed; the ACTUAL local
// path is stashed, not the folded key. Untracked content origin does NOT add
// (agent-memory/MEMORY.md, WIP docs/ — the legitimate files I-1594 was burned by
// when it blanket-stashed) is left completely untouched. Stashed paths carry the
// I-1594 `st-nonstate-residue` label so `st orphan list` surfaces them for
// recovery.
//
// Best-effort throughout: every git step is tolerant of error and the function
// ALWAYS returns 0 — it must never block session start. A non-collision pull
// failure (genuine conflict, divergence, network, etc.) touches nothing and
// returns 0 so the hook prints its normal diagnostics.
func StashPullConflicts(workspaceRoot, agentID string) int {
	// Branch guard: only the shared main/master checkout. A feature-branch
	// worktree carries the agent's own legitimate untracked WIP — never touch it.
	// Detached HEAD (mid-rebase/merge) returns non-zero here and no-ops (fail-safe).
	refOut, refErr := execGitOrphan(workspaceRoot, "symbolic-ref", "-q", "HEAD")
	if refErr != nil {
		return 0
	}
	branch := strings.TrimPrefix(strings.TrimSpace(string(refOut)), "refs/heads/")
	if branch != "main" && branch != "master" {
		return 0
	}

	// Mid-operation guard (#7): a leftover rebase/merge or stale index.lock means
	// any further git write would compound corruption. Mirror the sibling
	// ClearStagedNonState — reuse the canonical store.PreFlightGitState helper and
	// no-op rather than stash mid-operation.
	if err := store.PreFlightGitState(workspaceRoot); err != nil {
		return 0
	}

	// Layout guard (#8, I-936 class): the diff/status paths below are reported
	// relative to the git TOPLEVEL; they only agree with workspaceRoot when it IS
	// the toplevel (exactly how session-start invokes it on the shared clone). In a
	// nested/parent-repo layout the stash pathspec would misfire — fail safe.
	topOut, topErr := execGitOrphan(workspaceRoot, "rev-parse", "--show-toplevel")
	if topErr != nil {
		return 0
	}
	top := strings.TrimSpace(string(topOut))
	if cTop, e1 := filepath.EvalSymlinks(top); e1 == nil {
		if cWS, e2 := filepath.EvalSymlinks(workspaceRoot); e2 == nil {
			top, workspaceRoot = cTop, cWS
		}
	}
	if !strings.EqualFold(filepath.Clean(top), filepath.Clean(workspaceRoot)) {
		return 0
	}

	// U = locally untracked paths — computed FIRST, BEFORE any network (#9). Reuse
	// the package's shared NUL parse so the `??` detection can never drift from the
	// gate's tokenization. No untracked files ⇒ no possible collision ⇒ skip the
	// fetch + diff entirely.
	statusOut, statusErr := execGitOrphan(workspaceRoot, "status", "--porcelain",
		"-z", "--untracked-files=all")
	if statusErr != nil || len(statusOut) == 0 {
		return 0
	}
	var untracked []string
	for _, e := range store.ParseStatusZ(string(statusOut)) {
		if e.Code == "??" {
			untracked = append(untracked, e.Path)
		}
	}
	if len(untracked) == 0 {
		return 0
	}

	// Make origin/<branch> current so the incoming-added diff reflects what the
	// pull is actually trying to merge (#3 — use the guarded branch, not a
	// hardcoded "main", so a master-default clone works). Best-effort.
	originRef := "origin/" + branch
	_, _ = execGitOrphanCapture(workspaceRoot, "fetch", "-q", "origin", branch)

	// Diverged-HEAD guard (#4/#5): only proceed if a real fast-forward is possible,
	// i.e. HEAD is an ANCESTOR of origin/<branch>. If HEAD has diverged (a local
	// commit origin lacks), the pull failure is NOT an untracked-file conflict — the
	// merge would never clobber the untracked file, so stashing it would be wrong.
	// merge-base --is-ancestor exits 0 (ancestor) / non-zero (not, or ref missing).
	if _, err := execGitOrphan(workspaceRoot, "merge-base", "--is-ancestor", "HEAD", originRef); err != nil {
		return 0
	}

	// A = paths ADDED in origin/<branch> relative to HEAD (NUL-separated, raw, no
	// quoting). --no-renames (#2): without it, an operator's diff.renames=true would
	// report a rename DESTINATION as R instead of A and we'd miss a genuine
	// collision; --no-renames forces the destination to surface as a plain add.
	addedOut, addErr := execGitOrphan(workspaceRoot, "diff", "--diff-filter=A",
		"--no-renames", "--name-only", "-z", "HEAD", originRef)
	if addErr != nil {
		return 0
	}
	// Key by lower-case for case-insensitive matching (#1, APFS). The map value is
	// unused — only membership matters.
	added := make(map[string]bool)
	for _, p := range strings.Split(string(addedOut), "\x00") {
		if p != "" {
			added[strings.ToLower(p)] = true
		}
	}
	if len(added) == 0 {
		return 0
	}

	// collisions = A ∩ U, in `git status` order for a stable label. Match folded,
	// stash the ACTUAL untracked path (#1).
	var collisions []string
	for _, p := range untracked {
		if added[strings.ToLower(p)] {
			collisions = append(collisions, p)
		}
	}
	if len(collisions) == 0 {
		// The pull failed for some other reason (real conflict, a dirty TRACKED
		// file, etc.). Touch nothing — the hook prints its diagnostics.
		// #5 (acknowledged, left as-is): the I-1594 "leave untracked alone"
		// invariant only relaxes for a GENUINE collision — a path origin now
		// tracks AND the operator left untracked. That residue is recoverable from
		// the stash, so accepting it is correct, not a regression.
		return 0
	}

	today := time.Now().UTC().Format("2006-01-02")
	label := fmt.Sprintf("st-nonstate-residue: %s dropped-by:%s date:%s",
		strings.Join(collisions, ", "), agentID, today)
	// -u is REQUIRED: the colliding paths are untracked, and `git stash push`
	// without --include-untracked silently ignores them (leaving the pull blocked).
	// #6 (acknowledged, left as-is): a second agent racing the same stash window is
	// a best-effort gap — vanishingly rare for a solo operator and self-healing on
	// the next session-start.
	args := append([]string{"stash", "push", "-u", "-m", label, "--"}, collisions...)
	if out, stashErr := execGitOrphanCapture(workspaceRoot, args...); stashErr != nil {
		fmt.Fprintf(os.Stderr, "stash-pull-conflicts: failed to stash %d colliding path(s): %v\n%s\n",
			len(collisions), stashErr, strings.TrimSpace(string(out)))
		return 0
	}

	fmt.Printf("stash-pull-conflicts: stashed %d untracked path(s) blocking the ff-pull (by %s):\n",
		len(collisions), agentID)
	for _, p := range collisions {
		fmt.Printf("  %s\n", p)
	}
	// #0 — accurate recovery. After the retried pull succeeds, upstream's now-TRACKED
	// version occupies each path, so `git stash apply` FAILS ("already exists, no
	// checkout") — never advise it (reads as data loss). The operator's original
	// content is preserved in the stash; inspect/extract it with `stash show -p`.
	fmt.Printf("  your original untracked content for the path(s) above is preserved in a\n")
	fmt.Printf("  stash labelled st-nonstate-residue; upstream's tracked version now occupies\n")
	fmt.Printf("  the worktree path. Inspect/extract your version (do NOT `stash apply` — it\n")
	fmt.Printf("  fails once the path is tracked) with:\n")
	fmt.Printf("    git -C %q stash list                 # find the <ref>\n", workspaceRoot)
	fmt.Printf("    git -C %q stash show -p <ref>         # view/extract your content\n", workspaceRoot)
	fmt.Printf("    st orphan list --workspace %q\n", workspaceRoot)
	return 0
}

// OrphanList prints git stashes in workspaceRoot whose messages begin with
// "st-orphan:" and the recovery command for each.
func OrphanList(workspaceRoot string) {
	out, err := execGitOrphan(workspaceRoot, "stash", "list")
	if err != nil {
		fmt.Fprintf(os.Stderr, "orphan list: %v\n", err)
		return
	}
	if len(out) == 0 {
		fmt.Println("orphan: no stashes found")
		return
	}

	found := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "stash@{N}: On branch: st-orphan: ..." or
		// "...: st-nonstate-residue: ..." (I-1594 non-state residue parked
		// from the shared main checkout). Surface both kinds.
		if !strings.Contains(line, "st-orphan:") && !strings.Contains(line, "st-nonstate-residue:") {
			continue
		}
		found++
		// Extract stash ref (first field before first colon-space).
		parts := strings.SplitN(line, ": ", 2)
		ref := parts[0]
		fmt.Printf("%s\n", line)
		fmt.Printf("  recover: git -C %q stash show %s\n", workspaceRoot, ref)
		fmt.Printf("           git -C %q stash apply %s\n", workspaceRoot, ref)
	}
	if found == 0 {
		fmt.Println("orphan: no st-orphan stashes found")
	}
}

// readAssignedTo reads the `assigned_to:` field from a YAML item file.
// Returns "" if the file cannot be read or the field is absent/empty.
func readAssignedTo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "assigned_to:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "assigned_to:"))
			// Strip YAML quotes if present.
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// execGitOrphan runs a git command in dir and returns stdout.
var execGitOrphan = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	err := cmd.Run()
	return buf.Bytes(), err
}

// execGitOrphanCapture runs a git command and returns combined stdout+stderr.
var execGitOrphanCapture = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return out, err
}
