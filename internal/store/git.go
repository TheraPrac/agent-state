package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// gitLockTimeout is how long to wait for the git lock before giving up.
const gitLockTimeout = 15 * time.Second

// acquireGitLock takes an exclusive file lock on a .st-git.lock file
// in the item directory. Returns an unlock function. If the lock can't
// be acquired within gitLockTimeout, returns an error.
func acquireGitLock(dir string) (func(), error) {
	lockPath := filepath.Join(dir, ".st-git.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	deadline := time.Now().Add(gitLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("git lock timeout after %s (another st process is syncing)", gitLockTimeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// GitPull pulls latest changes from remote before reading items.
// Never destroys local work: uses git pull --ff-only, which refuses to
// advance when it would overwrite uncommitted changes or when history
// has diverged. Both failure modes are silent and safe — we just skip
// the pull and continue with current state; any subsequent GitSync
// will reconcile local and remote.
//
// History: earlier versions auto-committed uncommitted changes before
// pulling, which caused rebase conflicts when remote had archived a
// file locally still present in issues/. The fix for that was to
// aggressively `git checkout -- . && git clean -fd` before pulling,
// but that silently discarded in-progress mutations from state-changing
// commands (e.g. `st close` writes the move, the next command's pre-run
// GitPull then reverts it — the item pops back into issues/ and the
// close is lost). --ff-only is the conservative middle: no rebase,
// no destructive cleanup; just fetch and fast-forward when it's safe.
func GitPull(cfg *config.Config) error {
	if cfg.Git == nil || !cfg.Git.AutoPush {
		return nil
	}

	unlock, err := acquireGitLock(cfg.ItemDir())
	if err != nil {
		return nil
	}
	defer unlock()

	root := cfg.ItemDir()

	// Snapshot locked item files before any git operations.
	// These are items currently being worked on by a pipeline —
	// we must not let a pull overwrite their state even if ff-only
	// would cleanly advance through them.
	lockedSnapshots := snapshotLockedItems(cfg, root)

	// Fast-forward-only pull. Git refuses the fast-forward if:
	//   - history has diverged (local commits not on remote), or
	//   - uncommitted working-tree changes conflict with files the
	//     merge would touch.
	// In either case we silently skip; the next sync will reconcile.
	_ = gitCmdQuiet(root, "pull", "--ff-only")

	// Restore locked item files if the pull changed them.
	restoreLockedItems(lockedSnapshots)

	return nil
}

// snapshotLockedItems reads the content of all locked item files.
// Returns a map of file path -> content for restoration after pull.
func snapshotLockedItems(cfg *config.Config, root string) map[string][]byte {
	locked := LockedItems(cfg)
	if len(locked) == 0 {
		return nil
	}

	// Build a set for fast lookup
	lockedSet := make(map[string]bool, len(locked))
	for _, id := range locked {
		lockedSet[id] = true
	}

	snapshots := make(map[string][]byte)
	// Scan all type directories for files matching locked IDs.
	// Filenames are like "T-103-title-slug.md" — match by ID prefix.
	for _, tc := range cfg.Types {
		for _, dir := range tc.DirectoryMap {
			dirPath := filepath.Join(root, dir)
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				// Extract ID: everything before the second hyphen-delimited segment
				// e.g., "T-103-title-slug.md" -> check if "T-103" is locked
				name := strings.TrimSuffix(e.Name(), ".md")
				for id := range lockedSet {
					if name == id || strings.HasPrefix(name, id+"-") {
						path := filepath.Join(dirPath, e.Name())
						if data, err := os.ReadFile(path); err == nil {
							snapshots[path] = data
						}
						break
					}
				}
			}
		}
	}
	return snapshots
}

// restoreLockedItems writes back snapshotted content for any files
// that were changed by a git pull.
func restoreLockedItems(snapshots map[string][]byte) {
	for path, originalData := range snapshots {
		currentData, err := os.ReadFile(path)
		if err != nil || string(currentData) != string(originalData) {
			os.WriteFile(path, originalData, 0644)
		}
	}
}

// GitSync stages, commits, and pushes changes in the item root directory.
// Message is the commit message. Optional newPaths is the list of NEW
// (currently-untracked) files this caller wants committed; existing
// tracked files that the caller modified are picked up automatically
// via `git add -u`.
//
// I-442: switching from `git add -A` to `git add -u` + explicit
// new-file paths fixes the canonical-clone bleed. The shared workspace
// clone holds in-progress edits from peer agents on feature branches;
// `git add -A` swept those untracked files into whatever sync command
// happened to fire next, scrambling commit attribution. `git add -u`
// only stages tracked-and-modified files, so peer-WIP doesn't leak.
//
// Pre-fetches before committing to minimize conflicts. If push fails
// (remote ahead), retries with fetch + rebuild-commit-on-fetched-main +
// re-push up to maxRetries times.
//
// I-501: never runs `git pull --rebase`. The retry path was the main
// source of mid-rebase corruption when retries failed. Now: on push
// reject we fetch only, rebuild our agent-state commit on top of the
// fetched main via plumbing (commit-tree + update-ref), and push again.
// The working tree's index is never touched; rebase conflicts cannot
// arise.
func (s *Store) GitSync(message string, newPaths ...string) error {
	if s.cfg.Git == nil || !s.cfg.Git.AutoCommit {
		return nil
	}

	root := s.cfg.ItemDir()

	// Acquire lock to prevent concurrent git operations from parallel st
	// processes. Done BEFORE PreFlightGitState so a peer agent can't
	// transition the canonical clone into mid-rebase between our check
	// and our lock — the lock holder is the only writer of git state.
	unlock, err := acquireGitLock(root)
	if err != nil {
		return fmt.Errorf("git lock: %w", err)
	}
	defer unlock()

	// I-501: refuse if the canonical clone is mid-rebase / mid-merge /
	// holding a stale index.lock. The mutation that produced this call
	// already wrote to disk; we still emit the recovery hint and stop
	// here so we don't compound the corrupt state.
	if err := PreFlightGitState(root); err != nil {
		return err
	}

	// Pre-pull: fetch and integrate remote changes before committing.
	// Snapshot locked items so the pull can't overwrite active work.
	if s.cfg.Git.AutoPush {
		snap := snapshotLockedItems(s.cfg, root)
		_ = gitCmdQuiet(root, "pull", "--ff-only")
		restoreLockedItems(snap)
	}

	// Stage tracked-modified files only — peer agents' untracked WIP
	// files in the shared canonical clone DO NOT get swept.
	if err := gitCmd(root, "add", "-u"); err != nil {
		return fmt.Errorf("git add -u: %w", err)
	}

	// Stage explicit new files (callers that create files pass them).
	// Reject paths outside `root` — defense in depth for the bleed
	// this PR is fixing. A bugged caller passing a sibling agent's
	// path would otherwise produce a `../..` rel and git would happily
	// stage it. `--` defangs pathspecs that begin with `-`.
	for _, p := range newPaths {
		if p == "" {
			continue
		}
		rel := p
		if filepath.IsAbs(p) {
			r, err := filepath.Rel(root, p)
			if err != nil {
				return fmt.Errorf("git add new path %q: %w", p, err)
			}
			rel = r
		}
		if rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "..\\") {
			return fmt.Errorf("git add: path %q is outside item root %q", p, root)
		}
		if err := gitCmd(root, "add", "--", rel); err != nil {
			return fmt.Errorf("git add %q: %w", rel, err)
		}
	}

	// Check if there's anything STAGED to commit. `git status
	// --porcelain` would also show untracked files (e.g.
	// `.st-git.lock`, peer agents' WIP) which we intentionally don't
	// commit. `diff --cached --quiet` exits 0 when nothing is staged,
	// 1 when there are staged changes — a precise read of "is there
	// a commit to make" that ignores untracked noise.
	cached, err := gitOutput(root, "diff", "--cached", "--name-only")
	if err != nil {
		return fmt.Errorf("git diff --cached: %w", err)
	}
	if strings.TrimSpace(cached) == "" {
		return nil
	}

	// Commit
	if err := gitCmd(root, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push with retry
	if s.cfg.Git.AutoPush {
		if err := s.pushWithRetry(root, 3); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}

	return nil
}

// ErrPushDiverged is returned by pushWithRetry when the push is rejected
// AND the local commit cannot be cleanly replayed onto the fetched main
// (because of overlapping file changes from a peer agent). The local
// commit stays in place; the operator can resolve via `st sync` once
// the conflict is understood. I-501.
var ErrPushDiverged = errors.New("push rejected and replay diverged; local commit retained")

// ErrPushRejectedButOriginUnchanged is returned when the push was
// rejected by origin yet the fetch shows origin/main has not advanced
// past our parent. This usually indicates a server-side gate (pre-receive
// hook, branch protection, force-push refused) that retrying won't
// resolve. We surface it immediately rather than spin the retry loop
// up to maxRetries times only to emit the same generic push error. I-501.
var ErrPushRejectedButOriginUnchanged = errors.New("push rejected and origin has not moved (likely a pre-receive hook or branch protection)")

// pushWithRetry pushes refs/heads/main to origin. On rejection (peer
// raced), it fetches origin and rebuilds the commit via plumbing on top
// of the fresh origin/main, then retries the push.
//
// I-501: replaces the legacy `git pull --rebase` retry with a
// plumbing-only replay. The working tree's index is never touched;
// rebase conflicts cannot arise. Callers that need to know if the
// commit actually moved (e.g. ID-collision retry) inspect the local
// HEAD ref before vs after.
func (s *Store) pushWithRetry(root string, maxRetries int) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Push the local main ref explicitly (not "HEAD") so we work
		// regardless of which branch the working tree happens to be on.
		// I-501: decouples agent-state writes from working-tree HEAD.
		err := gitCmd(root, "push", "origin", "refs/heads/main:refs/heads/main")
		if err == nil {
			return nil
		}

		if attempt == maxRetries {
			return err
		}

		// Plumbing-only replay: fetch origin, rebuild our commit on top
		// of fetched origin/main without touching the working-tree
		// index. Snapshots aren't needed — we never mutate the working
		// tree.
		if replayErr := s.replayCommitOnFetchedMain(root); replayErr != nil {
			if errors.Is(replayErr, ErrPushDiverged) {
				return replayErr
			}
			if errors.Is(replayErr, ErrPushRejectedButOriginUnchanged) {
				// Retrying won't help — surface the original push error
				// alongside the diagnostic so the operator sees both.
				return fmt.Errorf("%w: %v", ErrPushRejectedButOriginUnchanged, err)
			}
			return fmt.Errorf("plumbing replay during push retry: %w", replayErr)
		}
	}
	return nil
}

// replayCommitOnFetchedMain rebuilds the local main ref to land our
// most-recent commit on top of a freshly-fetched origin/main. Used by
// pushWithRetry when the initial push is rejected.
//
// Implementation: build a fresh tree by starting from origin/main's tree
// and overlaying ONLY the files our commit changed. Then commit-tree
// against origin/main and update-ref. The working tree's index is
// never touched; the temp index lives in a temporary file.
//
// On overlapping changes (peer also changed a file we changed), surface
// ErrPushDiverged so the operator can recover via `st sync`. We never
// auto-overwrite a peer's edit.
func (s *Store) replayCommitOnFetchedMain(root string) error {
	// 1. Fetch origin/main (no working-tree mutation).
	if err := gitCmdQuiet(root, "fetch", "origin", "main"); err != nil {
		return fmt.Errorf("fetch origin main: %w", err)
	}

	// 2. Identify our local commit and its parent.
	headOut, err := gitOutput(root, "rev-parse", "refs/heads/main")
	if err != nil {
		return fmt.Errorf("rev-parse refs/heads/main: %w", err)
	}
	localHead := strings.TrimSpace(headOut)

	parentOut, err := gitOutput(root, "rev-parse", localHead+"^")
	if err != nil {
		return fmt.Errorf("rev-parse %s^: %w", localHead, err)
	}
	localParent := strings.TrimSpace(parentOut)

	originOut, err := gitOutput(root, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		return fmt.Errorf("rev-parse origin/main: %w", err)
	}
	originHead := strings.TrimSpace(originOut)

	if originHead == localParent {
		// Origin didn't move past our parent, yet our push was rejected.
		// Nothing to replay — and retrying the same push will hit the
		// same rejection. Surface the diagnostic immediately so the
		// operator sees an actionable message instead of waiting for
		// maxRetries retries to surface a generic "push failed."
		return ErrPushRejectedButOriginUnchanged
	}

	// 3. List the files we changed in our local commit. Empty list ⇒
	//    nothing to replay; treat as a no-op success.
	changedOut, err := gitOutput(root, "diff-tree", "--no-commit-id", "--name-only", "-r", localHead)
	if err != nil {
		return fmt.Errorf("diff-tree %s: %w", localHead, err)
	}
	var changed []string
	for _, line := range strings.Split(strings.TrimSpace(changedOut), "\n") {
		if line == "" {
			continue
		}
		changed = append(changed, line)
	}
	if len(changed) == 0 {
		return nil
	}

	// 4. Detect overlap: did the peer's commits between localParent and
	//    originHead also change any of our changed files? If so, refuse
	//    to overwrite — surface ErrPushDiverged with both refs.
	overlapOut, err := gitOutput(root, "diff", "--name-only", localParent, originHead)
	if err != nil {
		return fmt.Errorf("diff %s %s: %w", localParent, originHead, err)
	}
	peerChanged := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(overlapOut), "\n") {
		if line == "" {
			continue
		}
		peerChanged[line] = true
	}
	for _, p := range changed {
		if peerChanged[p] {
			return fmt.Errorf("%w (file %q changed locally and on origin/main between %s..%s)",
				ErrPushDiverged, p, localParent, originHead)
		}
	}

	// 5. Build the new tree in a temp index — start from origin/main's
	//    tree, overlay each of our changed files (or remove if deleted).
	commitMsg, err := gitOutput(root, "log", "-1", "--format=%B", localHead)
	if err != nil {
		return fmt.Errorf("read commit message: %w", err)
	}

	tmpIdx, err := os.CreateTemp(filepath.Join(root, ".git"), "index.replay-*")
	if err != nil {
		return fmt.Errorf("create temp index: %w", err)
	}
	tmpIdxPath := tmpIdx.Name()
	tmpIdx.Close()
	defer os.Remove(tmpIdxPath)

	env := []string{"GIT_INDEX_FILE=" + tmpIdxPath}

	if err := gitCmdEnv(root, env, "read-tree", originHead); err != nil {
		return fmt.Errorf("read-tree origin/main into temp index: %w", err)
	}

	for _, rel := range changed {
		abs := filepath.Join(root, rel)
		if _, statErr := os.Stat(abs); statErr != nil {
			if os.IsNotExist(statErr) {
				// File was deleted in our commit — remove from temp index.
				if err := gitCmdEnv(root, env, "update-index", "--remove", "--", rel); err != nil {
					return fmt.Errorf("update-index --remove %q: %w", rel, err)
				}
				continue
			}
			return fmt.Errorf("stat %q: %w", abs, statErr)
		}
		// Hash the working-tree blob and stage it at rel in the temp index.
		// `--path <rel>` makes git apply .gitattributes (text=auto, smudge/clean
		// filters) the same way `git add` would, so the replayed tree's blob
		// hash matches what a normal commit would produce.
		blobOut, err := gitOutput(root, "hash-object", "-w", "--path", rel, "--", abs)
		if err != nil {
			return fmt.Errorf("hash-object %q: %w", abs, err)
		}
		blob := strings.TrimSpace(blobOut)
		if err := gitCmdEnv(root, env, "update-index", "--add", "--cacheinfo",
			"100644,"+blob+","+rel); err != nil {
			return fmt.Errorf("update-index %q: %w", rel, err)
		}
	}

	treeOut, err := gitOutputEnv(root, env, "write-tree")
	if err != nil {
		return fmt.Errorf("write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)

	// 6. Create the new commit on top of origin/main and advance our
	//    local main ref. Old non-FF commit is dropped — its tree
	//    contents are incorporated into the new commit on the right
	//    parent.
	commitTreeOut, err := gitOutputStdin(root, strings.TrimRight(commitMsg, "\n"),
		"commit-tree", tree, "-p", originHead)
	if err != nil {
		return fmt.Errorf("commit-tree: %w", err)
	}
	newCommit := strings.TrimSpace(commitTreeOut)

	if err := gitCmdQuiet(root, "update-ref", "refs/heads/main", newCommit, localHead); err != nil {
		return fmt.Errorf("update-ref refs/heads/main: %w", err)
	}

	return nil
}

// RefreshOutcome describes how a RefreshWorkspace call resolved.
type RefreshOutcome int

const (
	RefreshDisabled RefreshOutcome = iota // git config nil — feature off
	RefreshUpToDate                       // fetch ok, no new commits
	RefreshPulled                         // fast-forwarded N commits
	RefreshDiverged                       // local AND remote have non-shared commits (ff-only refused)
	RefreshBlocked                        // uncommitted changes prevented ff
	RefreshOffline                        // fetch failed (network/auth/timeout)
	// RefreshAhead (I-430): local has unpushed commits but remote has
	// nothing new — pure-ahead, recoverable via `st sync`. Distinct from
	// RefreshDiverged so the dashboard doesn't scare the operator with
	// a "diverged" warning when the fix is just a push.
	RefreshAhead
)

// RefreshResult is the structured outcome of RefreshWorkspace, used by
// callers (e.g. st status) to render a banner explaining what happened
// and to decide whether to reload the store.
type RefreshResult struct {
	Outcome     RefreshOutcome
	PulledCount int   // commits fast-forwarded when Outcome == RefreshPulled
	AheadCount  int   // unpushed commits when Outcome == RefreshAhead (I-430)
	Err         error // last underlying error (for diagnostics; not always set)
}

// refreshFetchTimeout caps the network round-trip so a flaky remote
// can't hang `st status`. Fetch failure → RefreshOffline; the caller
// renders against last-known-good local state.
const refreshFetchTimeout = 10 * time.Second

// RefreshWorkspace updates the workspace clone from origin, returning a
// structured outcome that callers can render. Unlike GitPull it always
// attempts the refresh (gated only by `cfg.Git != nil`, not AutoPush) —
// the contract of `st status` is "show me the current state," and an
// operator running it has explicitly asked for the network round-trip.
//
// Sequence:
//  1. Acquire the workspace git lock (concurrent st processes serialize).
//  2. Snapshot any locked-item files (in-flight pipeline state) so the
//     pull cannot overwrite them, mirroring GitPull's protection.
//  3. `git fetch origin` with a timeout — fetch failure → RefreshOffline.
//  4. Inspect ahead/behind counts:
//       - ahead > 0 AND behind > 0: RefreshDiverged (true divergence;
//         caller must `git pull --rebase` or equivalent recovery).
//       - ahead > 0 AND behind == 0: RefreshAhead with AheadCount
//         (I-430: unpushed local commits, recoverable via `st sync`).
//       - behind == 0: RefreshUpToDate (silent success).
//       - behind > 0 with uncommitted blockers: RefreshBlocked.
//       - behind > 0 otherwise: `git pull --ff-only`. Success →
//         RefreshPulled (PulledCount = behind). Failure → RefreshBlocked.
//  5. Restore locked-item snapshots.
//
// Never auto-stashes, auto-rebases, or pushes. Operator intervention
// required for divergence and blocked states — banners surface them.
func RefreshWorkspace(cfg *config.Config) RefreshResult {
	if cfg.Git == nil {
		return RefreshResult{Outcome: RefreshDisabled}
	}

	root := cfg.ItemDir()

	unlock, err := acquireGitLock(root)
	if err != nil {
		// Another st process holds the lock — treat as offline rather than
		// blocking the user; they'll get fresh state on the next call.
		return RefreshResult{Outcome: RefreshOffline, Err: err}
	}
	defer unlock()

	lockedSnapshots := snapshotLockedItems(cfg, root)
	defer restoreLockedItems(lockedSnapshots)

	// 1. Fetch with timeout. Network/auth failure → offline.
	ctx, cancel := context.WithTimeout(context.Background(), refreshFetchTimeout)
	defer cancel()
	if err := gitCmdContext(ctx, root, "fetch", "--quiet", "origin"); err != nil {
		return RefreshResult{Outcome: RefreshOffline, Err: err}
	}

	// 2. Ahead/behind counts vs. upstream.
	behind, behindErr := gitCountCommits(root, "HEAD..@{upstream}")
	ahead, aheadErr := gitCountCommits(root, "@{upstream}..HEAD")
	if behindErr != nil || aheadErr != nil {
		// No upstream configured (or branch not tracking). Treat as up-to-date
		// rather than scaring the user — there's nothing to pull from.
		return RefreshResult{Outcome: RefreshUpToDate}
	}

	if ahead > 0 && behind > 0 {
		return RefreshResult{Outcome: RefreshDiverged}
	}
	if ahead > 0 {
		// I-430: pure-ahead. Local commits not yet pushed; nothing
		// alarming. Surface the count so the operator can recover with
		// `st sync` before the next push falls behind.
		return RefreshResult{Outcome: RefreshAhead, AheadCount: ahead}
	}
	if behind == 0 {
		return RefreshResult{Outcome: RefreshUpToDate}
	}

	// 3. Pull. ff-only refuses if uncommitted changes conflict with
	// the merge or any other non-ff condition surfaces — surface as Blocked.
	if err := gitCmdQuiet(root, "pull", "--ff-only"); err != nil {
		return RefreshResult{Outcome: RefreshBlocked, Err: err}
	}

	return RefreshResult{Outcome: RefreshPulled, PulledCount: behind}
}

// gitCountCommits runs `git rev-list --count <range>` and parses the
// result. Returns 0 + error if the range can't be evaluated (e.g. no
// upstream configured).
func gitCountCommits(dir, revRange string) (int, error) {
	out, err := gitOutput(dir, "rev-list", "--count", revRange)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, err
	}
	return n, nil
}

// gitCmdContext runs git with a context (timeout-capable). Uses
// exec.CommandContext so the process is killed when the context expires.
func gitCmdContext(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func gitCmd(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitCmdQuiet runs a git command silently (no stdout/stderr forwarding).
func gitCmdQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// gitCmdEnv runs git with extra env vars (e.g. GIT_INDEX_FILE) so a
// command can target a temp index without disturbing the real one. The
// process inherits the parent env, then env is appended. Used by the
// I-501 plumbing-replay path.
func gitCmdEnv(dir string, env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitOutputEnv is the output-capturing analog of gitCmdEnv.
func gitOutputEnv(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return string(out), err
}

// gitOutputStdin runs git with stdin piped from the supplied string and
// returns stdout. Used for `commit-tree` which reads the message from
// stdin in our plumbing-replay path.
func gitOutputStdin(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}
