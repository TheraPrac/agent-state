package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/store"
)

// NonStateStash parks uncommitted NON-state residue (scripts/, docs/, etc.)
// left in the SHARED theraprac-workspace main checkout by a peer, so it cannot
// silently block the next agent's state sync (I-1594). Two failure modes it
// clears: (A) staged non-state edits that trip checkNonStateGate and refuse
// `st sync`; (B) untracked non-state files that block the session-start
// `git pull --ff-only` ("untracked working tree files would be overwritten").
//
// Mirrors OrphanStash, with two deliberate differences:
//
//   - It is a STRICT NO-OP unless the checkout is on main/master. Feature-branch
//     worktrees carry the agent's own legitimate uncommitted non-state WIP; only
//     the shared main checkout should never hold non-state dirt. This branch
//     guard — NOT a per-file ownership check — is the peer-WIP-protection
//     boundary. It is why this command intentionally captures untracked (`??`)
//     and unstaged (` M`) files that checkNonStateGate deliberately skips
//     (I-442/I-1472): on the shared main checkout those are residue, and the
//     untracked ones are exactly failure-mode B.
//   - It captures untracked files too (git stash push -u).
//
// Path classification reuses the gate's own store.IsManagedStatePath so the set
// of "non-state" paths stashed here is identical to the set checkNonStateGate
// blocks on — no classifier drift.
//
// Staged RENAMES are deliberately NOT auto-parked. Clearing a staged rename
// safely requires mutating the index/working tree (the deleted old side cannot
// be named as a stash pathspec), and a half-cleared rename risks either leaving
// the gate blocked or committing an agent-state deletion. A staged non-state
// rename in the shared main checkout is rare; the gate flags BOTH rename sides,
// so it is surfaced for the operator to resolve rather than auto-mutated here.
//
// Nothing is deleted — every parked file is recoverable via `git stash` /
// `st orphan list`. Best-effort: any git error logs to stderr and processing
// continues; it never aborts startup.
func NonStateStash(workspaceRoot, itemsPrefix, agentID string) []string {
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
		return nil // feature branch — legitimate non-state WIP, leave it
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

	// -z: NUL-terminated, raw bytes, no path quoting. Rename/copy entries
	// arrive as two NUL tokens: "<XY> <new>\0<old>\0".
	// --untracked-files=all: list individual untracked FILES, not collapsed
	// directories, so per-file classification (and per-file stashing) works for
	// wholly-untracked dirs like docs/ or scripts/.
	out, err := execGitOrphan(workspaceRoot, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil || len(out) == 0 {
		return nil
	}

	var residues []string // non-state, non-rename paths to park
	seen := make(map[string]bool)
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			continue
		}
		code := tok[:2]
		path := tok[3:]
		// Rename/copy: the OLD path is the next NUL token (no XY prefix).
		// Consume it to keep parsing aligned, then skip the whole entry — see
		// the "Staged RENAMES are deliberately NOT auto-parked" note above.
		if code[0] == 'R' || code[0] == 'C' {
			if i+1 < len(tokens) {
				i++
			}
			continue
		}
		if path == "" || seen[path] {
			continue
		}
		// Leave agent-state (.as/ + itemsPrefix) for OrphanStash's ownership-
		// aware handling — identical rule to the gate.
		if store.IsManagedStatePath(path, itemsPrefix) {
			continue
		}
		seen[path] = true
		residues = append(residues, path)
	}

	today := time.Now().UTC().Format("2006-01-02")
	var stashed []string
	for _, p := range residues {
		label := fmt.Sprintf("st-nonstate-residue: %s dropped-by:%s date:%s",
			p, agentID, today)
		// -u captures untracked paths (failure-mode B); harmless for tracked.
		if _, stashErr := execGitOrphanCapture(workspaceRoot, "stash", "push", "-u",
			"-m", label, "--", p); stashErr != nil {
			fmt.Fprintf(os.Stderr, "nonstate-stash: failed to stash %s: %v\n", p, stashErr)
			continue
		}
		stashed = append(stashed, p)
	}

	if len(stashed) > 0 {
		fmt.Printf("nonstate-stash: parked %d non-state file(s) from the shared main checkout (dropped-by %s):\n", len(stashed), agentID)
		for _, p := range stashed {
			fmt.Printf("  %s\n", p)
		}
		// Each file is parked in its OWN git stash. Do NOT print stash@{N} refs
		// here — every push shifts earlier stashes down, so a ref captured at
		// push time is stale by the end. `st orphan list` reads the live stash
		// list and prints the authoritative ref per file.
		fmt.Printf("  each is parked in its own git stash labeled 'st-nonstate-residue: <path>'\n")
		fmt.Printf("  recover: st orphan list --workspace %q   (then git -C %q stash apply <ref>)\n", workspaceRoot, workspaceRoot)
	}
	return stashed
}
