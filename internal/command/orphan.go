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
//	A = paths ADDED in origin/main that are absent from HEAD
//	    (git diff --diff-filter=A --name-only HEAD origin/main)
//	U = locally UNTRACKED paths (git status --porcelain, code "??")
//	collisions = A ∩ U   ← exactly the untracked files the ff-merge can't write over
//
// Only that intersection is stashed. Untracked content origin does NOT add
// (agent-memory/MEMORY.md, WIP docs/ — the legitimate files I-1594 was burned by
// when it blanket-stashed) is left completely untouched. Stashed paths carry the
// I-1594 `st-nonstate-residue` label so `st orphan list` surfaces them for
// recovery.
//
// Best-effort throughout: every git step is tolerant of error and the function
// ALWAYS returns 0 — it must never block session start. A non-collision pull
// failure (genuine conflict, network, etc.) touches nothing and returns 0 so the
// hook prints its normal diagnostics.
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

	// Make origin/main current so the incoming-added diff reflects what the pull
	// is actually trying to merge. Best-effort: a fetch failure just means we
	// compare against the already-known origin/main.
	_, _ = execGitOrphanCapture(workspaceRoot, "fetch", "-q", "origin", "main")

	// A = paths ADDED in origin/main relative to HEAD (NUL-separated, raw, no
	// quoting). These are the files the ff-merge would create in the worktree.
	addedOut, addErr := execGitOrphan(workspaceRoot, "diff", "--diff-filter=A",
		"--name-only", "-z", "HEAD", "origin/main")
	if addErr != nil {
		return 0
	}
	added := make(map[string]bool)
	for _, p := range strings.Split(string(addedOut), "\x00") {
		if p != "" {
			added[p] = true
		}
	}
	if len(added) == 0 {
		return 0
	}

	// U = locally untracked paths. Reuse the package's shared NUL parse so the
	// `??` detection can never drift from the gate's tokenization.
	statusOut, statusErr := execGitOrphan(workspaceRoot, "status", "--porcelain",
		"-z", "--untracked-files=all")
	if statusErr != nil || len(statusOut) == 0 {
		return 0
	}

	// collisions = A ∩ U, in origin/main's listed order for a stable label.
	var collisions []string
	seen := make(map[string]bool)
	for _, e := range store.ParseStatusZ(string(statusOut)) {
		if e.Code == "??" && added[e.Path] && !seen[e.Path] {
			collisions = append(collisions, e.Path)
			seen[e.Path] = true
		}
	}
	if len(collisions) == 0 {
		// The pull failed for some other reason (real conflict, network, a
		// dirty TRACKED file). Touch nothing — the hook prints its diagnostics.
		return 0
	}

	today := time.Now().UTC().Format("2006-01-02")
	label := fmt.Sprintf("st-nonstate-residue: %s dropped-by:%s date:%s",
		strings.Join(collisions, ", "), agentID, today)
	// -u is REQUIRED: the colliding paths are untracked, and `git stash push`
	// without --include-untracked silently ignores them (leaving the pull blocked).
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
	fmt.Printf("  recover: git -C %q stash list   (or: st orphan list --workspace %q)\n",
		workspaceRoot, workspaceRoot)
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
