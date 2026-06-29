package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/theraprac/agent-state/internal/store"
)

// ClearStagedNonState un-stages STAGED non-state changes (scripts/, docs/, etc.)
// left in the SHARED theraprac-workspace main checkout, so they no longer trip
// checkNonStateGate and silently block the next agent's `st sync` (I-1594).
//
// Staged non-state is exactly — and only — what the gate refuses `st sync` on:
// the gate skips pure-untracked (`??`) and working-tree-only / unstaged (` M`)
// entries (store.IsGateSkippedStatus, mirrored from checkNonStateGate). This
// command clears precisely that staged set by UN-STAGING it
// (`git reset -q -- <paths>`), never by deleting or stashing. Un-staging:
//
//   - is non-destructive: the file content stays in the working tree exactly as
//     it was — a partially-staged file keeps its unstaged hunks, a staged
//     deletion stays deleted-but-unstaged, a staged add becomes untracked. The
//     gate skips all of those, so `st sync` is unblocked without losing a byte
//     of anyone's work. (The first cut stashed files and removed them from the
//     tree, which destroyed legitimate untracked/unstaged content in the shared
//     checkout — see I-1594 history.)
//   - handles staged deletions uniformly: a staged deletion un-stages to a
//     worktree-only deletion (gate-skipped). No fragile stash-by-pathspec.
//   - un-stages a staged rename only when BOTH sides are non-state (decomposing
//     it into a worktree deletion + untracked file, both gate-skipped). A rename
//     touching agent-state on either side is left entirely for the gate +
//     OrphanStash — un-staging one side would lose an agent-state item or leave
//     the gate blocked.
//
// STRICT NO-OP unless the checkout is on main/master: feature-branch worktrees
// carry the agent's own legitimate staged non-state WIP; only the shared main
// checkout should never hold staged non-state dirt. This branch guard is an
// extra boundary on top of the staged-only rule.
//
// NOT handled here (both tracked as I-1620, since each needs touching state the
// gate deliberately leaves alone, or rewriting history):
//   - failure-mode B: an UNTRACKED / unstaged-tracked file blocking
//     `git pull --ff-only` — needs reactive, pull-time handling of only the
//     paths an incoming merge would overwrite (never blanket-touch agent-memory/
//     or WIP docs/).
//   - committed-but-unpushed non-state on local main (which checkNonStateGate
//     ALSO refuses sync on): un-staging cannot clear a commit; that needs a
//     separate, careful reset/relocate of the offending commit.
//
// Best-effort: a git error logs to stderr and the function continues; it never
// aborts startup. Returns the list of un-staged paths.
func ClearStagedNonState(workspaceRoot, itemsPrefix, agentID string) []string {
	// Branch guard: only the shared main checkout. symbolic-ref returns
	// refs/heads/<branch> on a branch, non-zero on detached HEAD. A detached
	// HEAD (mid-rebase/merge) deliberately no-ops — never mutate a checkout
	// that is mid-operation (fail-safe).
	refOut, refErr := execGitOrphan(workspaceRoot, "symbolic-ref", "-q", "HEAD")
	if refErr != nil {
		return nil
	}
	branch := strings.TrimPrefix(strings.TrimSpace(string(refOut)), "refs/heads/")
	if branch != "main" && branch != "master" {
		return nil // feature branch — legitimate staged non-state WIP, leave it
	}

	// Mid-operation guard: an in-progress merge / rebase / cherry-pick / revert
	// keeps HEAD on the branch ref (so the symbolic-ref check above passes), yet
	// `git reset` would collapse the operation's higher-stage index entries and
	// corrupt the in-flight conflict resolution. Never mutate a checkout that is
	// mid-operation — fail safe.
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if _, e := execGitOrphan(workspaceRoot, "rev-parse", "-q", "--verify", marker); e == nil {
			return nil
		}
	}

	// Layout guard: this command derives "non-state" from itemsPrefix taken raw
	// (e.g. "agent-state/"), and `git status` run in workspaceRoot reports paths
	// relative to the git TOPLEVEL. Those agree only when workspaceRoot IS the
	// toplevel — which is exactly how session-start invokes it (the shared
	// workspace clone). If they diverge (a nested/parent-repo layout, the I-936
	// class), the prefix would misclassify and a staged agent-state item could be
	// un-staged → dropped by st sync. Rather than re-derive the gate's full
	// Rel/EvalSymlinks prefix here, fail safe: no-op unless workspaceRoot is the
	// toplevel. (Shared prefix derivation is tracked as a follow-up.)
	topOut, topErr := execGitOrphan(workspaceRoot, "rev-parse", "--show-toplevel")
	if topErr != nil {
		return nil
	}
	top := strings.TrimSpace(string(topOut))
	if physicalPath(top) != physicalPath(workspaceRoot) {
		return nil
	}

	// Flat layout (items root == git toplevel, Paths.Root "." or ""): the gate
	// fail-opens (no items-vs-non-items surface to enforce), so there is no
	// non-state residue to clear — mirror that and no-op, rather than treating
	// agent-state item files as residue.
	itemsPrefix = strings.TrimSpace(itemsPrefix)
	if itemsPrefix == "" || itemsPrefix == "." || itemsPrefix == "./" {
		return nil
	}
	if !strings.HasSuffix(itemsPrefix, "/") {
		itemsPrefix += "/"
	}

	// -z: NUL-terminated, raw bytes, no path quoting. Rename/copy entries arrive
	// as two NUL tokens: "<XY> <new>\0<old>\0". --untracked-files=no: every
	// untracked entry is skipped below (the gate skips `??`), so there is no
	// reason to pay for the untracked working-tree walk on the startup hot path.
	out, err := execGitOrphan(workspaceRoot, "status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil || len(out) == 0 {
		return nil
	}

	var paths []string  // staged non-state paths to un-stage (incl. both rename sides)
	var labels []string // primary paths, for the report
	// `git status -z` lists each path exactly once, so no dedup map is needed.
	// addPath queues a non-empty path; returns true if it appended.
	addPath := func(p string) bool {
		if p == "" {
			return false
		}
		paths = append(paths, p)
		return true
	}
	// ParseStatusZ (I-1621) does the shared NUL tokenization + rename pairing —
	// the same structural parse the gate runs — so this command can never drift
	// from checkNonStateGate on token/rename handling. The skip / managed-path
	// predicates below are applied here, identical to the gate's.
	for _, e := range store.ParseStatusZ(string(out)) {
		// Rename/copy: ParseStatusZ already paired the OLD path. A rename is
		// staged (code[0] is R/C, never gate-skipped), so handle it here before
		// the staged/managed checks below.
		if e.IsRename {
			newManaged := store.IsManagedStatePath(e.Path, itemsPrefix)
			oldManaged := e.OldPath != "" && store.IsManagedStatePath(e.OldPath, itemsPrefix)
			// A rename touching agent-state on EITHER side is left ENTIRELY for
			// the gate to flag and OrphanStash / the operator to resolve.
			// Un-staging only the non-state side would either leave a staged
			// agent-state deletion that st sync then commits (item data loss) or
			// leave the gate blocked on the non-state side. checkNonStateGate
			// gates both rename sides, so the rename is surfaced, not auto-mutated.
			// Only renames whose BOTH sides are non-state are un-staged.
			if newManaged || oldManaged {
				continue
			}
			if addPath(e.Path) {
				labels = append(labels, e.Path)
			}
			addPath(e.OldPath)
			continue
		}

		// Mirror checkNonStateGate's skips EXACTLY via the shared predicate:
		// only STAGED (index-side) entries reach the gate's offender list.
		if store.IsGateSkippedStatus(e.Code) {
			continue
		}
		// Leave agent-state (.as/ + itemsPrefix) for OrphanStash's ownership-
		// aware handling — identical rule to the gate.
		if store.IsManagedStatePath(e.Path, itemsPrefix) {
			continue
		}
		if addPath(e.Path) {
			labels = append(labels, e.Path)
		}
	}

	if len(paths) == 0 {
		return nil
	}

	// Un-stage all collected paths in one `git reset` (reset each index entry to
	// HEAD). Non-destructive — working-tree content is untouched.
	args := append([]string{"reset", "-q", "--"}, paths...)
	if _, resetErr := execGitOrphanCapture(workspaceRoot, args...); resetErr != nil {
		fmt.Fprintf(os.Stderr, "clear-nonstate: failed to un-stage non-state residue: %v\n", resetErr)
		return nil
	}

	fmt.Printf("clear-nonstate: un-staged %d staged non-state file(s) in the shared main checkout (by %s):\n", len(labels), agentID)
	for _, p := range labels {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("  (content left untouched in the working tree; re-stage with `git add` if intended)\n")
	// Deliberately NOT claiming sync is unblocked: other gate offenders the gate
	// also refuses on — committed-but-unpushed non-state, or a staged rename that
	// touches agent-state (left intact above) — are not cleared here (I-1620).
	return labels
}
