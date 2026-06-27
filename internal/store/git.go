package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/theraprac/agent-state/internal/config"
)

// gitLockWaiterTimeout is how long a waiter spins before giving up on the
// .st-git.lock. The holder (GitSync) can hold the lock across multiple
// sequential network ops (pull + push + retries + verifyPushLanded), so
// the worst-case hold time is several minutes. 60s is a pragmatic
// interactive-command patience threshold, not a guarantee; a waiter may
// spuriously time out during a legitimate slow push-with-retries.
const gitLockWaiterTimeout = 60 * time.Second

// gitNetworkTimeout is the hard deadline for git operations that talk to a
// remote (pull, push, fetch). Combined with gitSSHCommand's keepalive
// options this ensures a dead TCP link releases the lock within ~60s.
const gitNetworkTimeout = 60 * time.Second

// gitSSHCommand is set via GIT_SSH_COMMAND on every network-facing git
// call. ConnectTimeout=10 limits the initial handshake; ServerAliveInterval
// + ServerAliveCountMax=3 disconnect a silent connection after 30s.
// Together with gitNetworkTimeout these prevent a hung ssh from holding
// .st-git.lock indefinitely and stalling all agent syncs (I-1411).
const gitSSHCommand = "ssh -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"

// autoStageSubdirs is the I-575 list of agent-state subdirectories whose
// untracked files SHOULD be picked up automatically by GitSync. This is
// deliberately narrow — `.plans/<id>.md` files dropped by `st prep` /
// `st start` are unambiguously agent-state and benefit nobody by sitting
// untracked, so they're auto-staged. Item directories (issues/, tasks/,
// archive/) are NOT in this list because peer agents' brand-new untracked
// item files (e.g. mid-`st create`, pre-GitSync) live there and must keep
// I-442's no-sweep protection — otherwise we re-commit them under the
// wrong agent's attribution. Add new entries here as the agent-state
// convention grows; never switch to `.` or `git add -A`.
var autoStageSubdirs = []string{".plans"}

// acquireGitLock takes an exclusive file lock on a .st-git.lock file
// in the item directory. Returns an unlock function. If the lock can't
// be acquired within gitLockWaiterTimeout, returns an error that names
// the holding process (PID + argv written to the lock file on acquire).
func acquireGitLock(dir string) (func(), error) {
	return acquireGitLockTimeout(dir, gitLockWaiterTimeout)
}

// acquireGitLockTimeout is the parameterised implementation of
// acquireGitLock. The separate function lets tests use short timeouts
// without modifying package-level constants.
func acquireGitLockTimeout(dir string, timeout time.Duration) (func(), error) {
	lockPath := filepath.Join(dir, ".st-git.lock")
	// O_RDWR so the holder can write PID info and a timed-out waiter can
	// read it back (O_WRONLY would prevent the seek+read on timeout).
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	start := time.Now()
	deadline := start.Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Stamp holder identity so waiters can name who holds the lock.
			// Check Truncate and Seek errors: if either fails (e.g. on an NFS
			// mount), skip the write entirely so no stale content from the
			// previous holder is left in the file to mislead a timed-out
			// waiter's hint. Diagnostic-only; locking correctness is unaffected.
			if terr := f.Truncate(0); terr == nil {
				if _, serr := f.Seek(0, 0); serr == nil {
					fmt.Fprintf(f, "pid=%d cmd=%s", os.Getpid(), strings.Join(os.Args, " "))
				}
			}
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			hint := ""
			f.Seek(0, 0)
			if info, rerr := io.ReadAll(f); rerr == nil && len(strings.TrimSpace(string(info))) > 0 {
				hint = " (holder: " + strings.TrimSpace(string(info)) + ")"
			}
			f.Close()
			return nil, fmt.Errorf("git lock timeout after %s%s", time.Since(start).Round(time.Second), hint)
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
	// runNetGit applies a 60s deadline + SSH keepalive so a stalled
	// network op cannot hold .st-git.lock indefinitely (I-1411).
	_ = runNetGit(root, "pull", "--ff-only")

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
// Message is the commit message.
//
// What gets staged (in order):
//  1. `git add -u` — every tracked file with modifications.
//  2. `git add -- <sub>` for each entry in autoStageSubdirs — untracked
//     files inside the narrow set of agent-state subdirs that are safe
//     to auto-commit (currently just `.plans/`). I-575.
//  3. `newPaths` (variadic, optional) — explicit absolute paths the
//     caller knows about. Used by callers that create files in
//     locations OUTSIDE autoStageSubdirs and outside the item-dir
//     (e.g. `mail.Send` writing to the workspace mailbox), and as a
//     defense-in-depth signal for any caller that wants to assert
//     "this specific file MUST be in the commit." Files inside
//     autoStageSubdirs no longer need to be passed here — they're
//     picked up by step 2.
//
// I-442: switching from `git add -A` to `git add -u` + explicit
// new-file paths fixes the canonical-clone bleed. The shared workspace
// clone holds in-progress edits from peer agents on feature branches;
// `git add -A` swept those untracked files into whatever sync command
// happened to fire next, scrambling commit attribution. `git add -u`
// stages only tracked-and-modified files, so peer-WIP elsewhere in the
// canonical clone (and peer-untracked item files in agent-state's
// issues/ / tasks/ / archive/ subdirs) stays untouched.
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

// ErrI807MainBranchGate is the sentinel for the I-807/I-765 gate refusal so
// upstream callers can detect it programmatically via errors.Is rather
// than by string matching on the human-readable message. I-765 broadened
// the gate from main-only to all branches; the sentinel is kept for
// back-compat with existing errors.Is call sites.
var ErrI807MainBranchGate = errors.New("I-807: main-branch dirty-non-state gate")

// gateGitOutput runs `git <args>` in dir with `GIT_DIR`, `GIT_WORK_TREE`,
// and `GIT_INDEX_FILE` scrubbed from the environment. The gate's git
// introspection (rev-parse, status --porcelain -z, log -z) MUST read
// the workspace's real state, not whatever index/dir an outer process
// happens to have exported (e.g. an interrupted plumbing-replay debug
// session, an embedded library use, an IDE wrapper). A leaked GIT_*
// env can route status/log at the wrong repo and cause the gate to
// fail-open on a truly-dirty workspace OR fail-closed against phantom
// offenders from the leaked index.
func gateGitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := os.Environ()
	scrubbed := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		scrubbed = append(scrubbed, kv)
	}
	cmd.Env = scrubbed
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), err
}

// isManagedStatePath returns true when the toplevel-relative path lives
// inside the agent-state allowlist: the configured items prefix (e.g.
// `agent-state/`) or `.as/`. The lock file (`<itemsPrefix>.st-git.lock`)
// is also allowlisted defense-in-depth (it should also be `??`, which
// the caller filters earlier).
func isManagedStatePath(path, itemsPrefix string) bool {
	p := strings.ToLower(path)
	if strings.HasPrefix(p, ".as/") {
		return true
	}
	if itemsPrefix != "" && strings.HasPrefix(p, strings.ToLower(itemsPrefix)) {
		return true
	}
	return false
}

// IsGateSkippedStatus reports whether a `git status --porcelain` XY code is one
// checkNonStateGate deliberately ignores: pure-untracked (`??`) or
// working-tree-only / unstaged (index slot blank, `code[0] == ' '`). Only STAGED
// (index-side) entries can trip the gate. Shared so st orphan clear-nonstate
// clears exactly the staged set the gate blocks on — no classifier drift
// (I-1594). A short (<2 char) code is treated as not-skipped (caller guards len).
func IsGateSkippedStatus(code string) bool {
	if len(code) < 2 {
		return false
	}
	return code == "??" || code[0] == ' '
}

// IsManagedStatePath is the exported wrapper around isManagedStatePath so the
// command package (st orphan stash-nonstate, I-1594) classifies paths with the
// EXACT same allowlist as checkNonStateGate — `.as/` or itemsPrefix, lowercased
// per I-835 — avoiding classifier drift between the gate and the residue-stash
// command that exists to clear what the gate blocks.
func IsManagedStatePath(path, itemsPrefix string) bool {
	return isManagedStatePath(path, itemsPrefix)
}

// checkNonStateGate is the I-807/I-765 defense-in-depth gate. It fails
// closed when any tracked / staged / committed-but-unpushed mutation outside
// the agent-state allowlist (`<itemsPrefix>`, `.as/`) is present, on ANY
// branch. I-807 introduced the gate for main only; I-765 broadened it to all
// branches to prevent non-state files from being auto-pushed via st sync
// regardless of which branch is checked out.
//
// Fail-opens (returns nil) when:
//   - symbolic-ref fails (corrupt HEAD, missing .git, etc.) AND the
//     detached-HEAD-at-origin/main fallback also can't resolve
//   - flat-layout fixture (itemsPrefix == "") — no items-vs-non-items
//     distinction to enforce; the gate only protects nested layouts
//   - ST_SYNC_ALLOW_NON_STATE=1 is set (any branch); OR
//     ST_SYNC_ALLOW_MAIN=1 is set AND branch is main (main-only scope,
//     back-compat with I-807 docs — use ST_SYNC_ALLOW_NON_STATE on
//     feature branches); audit-stderr is emitted so the bypass is named
//
// Note on unborn HEAD: a freshly-init'd repo with HEAD symbolic to
// refs/heads/main but no commits yet WILL trip the gate (symbolic-ref
// succeeds, onMain becomes true). This is intentional: the first commit
// is exactly the case where mis-routing a non-state push to main is
// hardest to recover from.
//
// Inspects: (a) `git status --porcelain` for tracked-modified / staged
// changes in the working tree, AND (b) `git log origin/main..HEAD
// --name-only` for non-state paths in already-committed-locally-but-
// unpushed commits (the "stranded local commit" path that working-tree
// inspection alone misses).
//
// Skips `??` (pure-untracked) porcelain entries to preserve I-442's
// peer-WIP protection. Restricts rename / copy detection to entries whose
// XY status code begins with `R` or `C` (not a path-text substring search)
// to avoid mangling filenames that happen to contain ` -> `. Gates BOTH
// the old AND new path of a rename so a rename FROM non-state INTO
// agent-state still flags the (deletion-side) non-state mutation.
//
// I-765 broadened this gate from main-only to all branches.
func checkNonStateGate(root string) error {
	allowMain := os.Getenv("ST_SYNC_ALLOW_MAIN") == "1"
	allowNonState := os.Getenv("ST_SYNC_ALLOW_NON_STATE") == "1"
	// ST_SYNC_ALLOW_MAIN is main-only (back-compat with I-807 docs).
	// ST_SYNC_ALLOW_NON_STATE bypasses on any branch.
	// overrideSet is resolved after onMain is determined below.

	// Use `symbolic-ref -q HEAD` (not `--abbrev-ref`) to distinguish a
	// branch literally named `HEAD` from a truly-detached HEAD.
	// symbolic-ref returns refs/heads/<branch> on a branch; non-zero +
	// empty stdout on detached HEAD.
	symRefOut, symRefErr := gateGitOutput(root, "symbolic-ref", "-q", "HEAD")
	detached := symRefErr != nil
	onMain := false
	currentBranch := "HEAD (detached)"
	if !detached {
		ref := strings.TrimSpace(symRefOut)
		onMain = ref == "refs/heads/main"
		currentBranch = strings.TrimPrefix(ref, "refs/heads/")
	} else {
		// Detached HEAD: if the detached commit is the same as
		// refs/remotes/origin/main, treat it as on-main for push-
		// protection purposes (pushWithRetry can still target main).
		headSHA, herr := gateGitOutput(root, "rev-parse", "HEAD")
		originSHA, oerr := gateGitOutput(root, "rev-parse", "refs/remotes/origin/main")
		if herr == nil && oerr == nil {
			h := strings.TrimSpace(headSHA)
			o := strings.TrimSpace(originSHA)
			if h != "" && h == o {
				onMain = true
				currentBranch = "main (detached)"
			}
		}
	}
	// I-765: gate runs on every branch, not just main. The early return
	// `if !onMain { return nil }` has been removed.
	// ST_SYNC_ALLOW_MAIN is main-only; ST_SYNC_ALLOW_NON_STATE works everywhere.
	overrideSet := allowNonState || (allowMain && onMain)

	// Resolve git toplevel so porcelain paths come back relative to the
	// git root (e.g. `agent-state/issues/I-X.md`, `claude-config/hooks/foo.sh`),
	// not relative to ItemDir which would emit `../claude-config/...`.
	toplevelOut, err := gateGitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil // fail-open
	}
	toplevel := strings.TrimSpace(toplevelOut)

	// Compute the items-root prefix relative to the git toplevel.
	// Resolve symlinks on BOTH sides atomically: macOS routes /var →
	// /private/var. If only one EvalSymlinks succeeds, Rel mixes the
	// canonical and raw forms and emits a `../../private/var/...`
	// traversal that mis-classifies every path as non-state. Use the
	// raw forms on either side's failure (or fail-open).
	// I-835: also lowercase both inputs before Rel. On macOS APFS
	// (case-insensitive, case-preserving), EvalSymlinks can return
	// different casings for the two paths — e.g. /Users/x/Dev/...
	// vs /Users/x/dev/... — making Rel emit a ../../... traversal
	// path. Lowercasing both is a no-op on correctly-cased paths and
	// on case-sensitive filesystems.
	canonRoot, errRoot := filepath.EvalSymlinks(root)
	canonToplevel, errTop := filepath.EvalSymlinks(toplevel)
	if errRoot != nil || errTop != nil {
		canonRoot = root
		canonToplevel = toplevel
	}
	itemsRel, err := filepath.Rel(strings.ToLower(canonToplevel), strings.ToLower(canonRoot))
	if err != nil {
		return nil // fail-open
	}
	itemsPrefix := ""
	if itemsRel != "." {
		itemsPrefix = strings.ToLower(filepath.ToSlash(itemsRel)) + "/"
	}

	// Flat layout has no items-vs-non-items distinction to enforce.
	// Surface this once per process so an operator inheriting flat
	// config doesn't silently assume the gate is active.
	if itemsPrefix == "" {
		flatLayoutAuditOnce()
		return nil
	}

	// Status porcelain with -z (NUL-terminated, raw bytes, no quoting).
	// Default porcelain v1 quotes any path containing a space or non-
	// ASCII byte — the surrounding `"..."` defeats HasPrefix allowlist
	// checks. -z is the canonical fix. Rename / copy entries arrive as
	// two NUL-terminated tokens: `<XY> <new-path>\0<old-path>\0`.
	statusOut, err := gateGitOutput(toplevel, "status", "--porcelain", "-z")
	if err != nil {
		return nil // fail-open
	}

	var offenders []string
	// Cross-scan dedup: track all reported paths (basename-stripped of
	// annotation suffixes) so the same file never appears twice when
	// it's both working-tree-dirty AND mentioned in a stranded commit.
	seen := make(map[string]bool)
	tokens := strings.Split(statusOut, "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			continue
		}
		code := tok[:2]
		path := tok[3:]
		// Skip pure-untracked and working-tree-only modifications.
		// `??` files are skipped so untracked peer WIP (I-442) is safe.
		// `code[0] == ' '` (index clean, working-tree dirty) is skipped
		// because `git add -u -- .` is scoped to agent-state/ and will
		// never stage files outside that prefix; blocking on them fires
		// false alarms for peer agents' uncommitted edits. I-1472.
		if IsGateSkippedStatus(code) {
			continue
		}
		// Rename / copy: -z format puts the OLD path in the next token
		// (no XY prefix). XY[0] is the truth-of-rename. Per
		// git-status(1) porcelain v1: R/C only appear in X (index),
		// never in Y — so checking code[0] catches `R `, `RM`, `RD`,
		// `C `, `CM`, etc. without any path-text substring search.
		if code[0] == 'R' || code[0] == 'C' {
			newPath := path
			oldPath := ""
			if i+1 < len(tokens) {
				oldPath = tokens[i+1]
				i++ // consume the old-path token
			}
			if !isManagedStatePath(newPath, itemsPrefix) && !seen[newPath] {
				offenders = append(offenders, newPath+" (rename target)")
				seen[newPath] = true
			}
			if oldPath != "" && !isManagedStatePath(oldPath, itemsPrefix) && !seen[oldPath] {
				offenders = append(offenders, oldPath+" (rename source)")
				seen[oldPath] = true
			}
			continue
		}
		if isManagedStatePath(path, itemsPrefix) {
			continue
		}
		if seen[path] {
			continue
		}
		offenders = append(offenders, path)
		seen[path] = true
	}

	// Also scan for non-state files in locally-committed-but-unpushed
	// commits on main. The working-tree inspection above misses commits
	// that were created before this gate landed (or by a parallel session)
	// and that pushWithRetry would otherwise batch-push to origin/main.
	// Use -z + NUL parsing so the same quoting concern doesn't bite.
	//
	// Restriction to onMain: pushWithRetry always pushes
	// refs/heads/main:refs/heads/main regardless of which branch is
	// checked out (I-501). Feature branch commits never reach origin/main
	// via st sync, so scanning refs/remotes/origin/main..HEAD on a feature
	// branch would falsely flag the entire feature branch commit log —
	// including intentional non-state commits that are part of the PR.
	if onMain {
		logOut, logErr := gateGitOutput(toplevel, "log", "-z", "--name-only", "--pretty=format:", "refs/remotes/origin/main..HEAD")
		if logErr == nil {
			for _, p := range strings.Split(logOut, "\x00") {
				p = strings.TrimSpace(p)
				if p == "" || seen[p] {
					continue
				}
				if isManagedStatePath(p, itemsPrefix) {
					continue
				}
				seen[p] = true
				offenders = append(offenders, p+" (already committed locally)")
			}
		}
	}

	if len(offenders) == 0 {
		return nil
	}

	var b strings.Builder
	// Body header drops the "I-807:" prefix — the sentinel's wrapped
	// text already carries it, so `err.Error()` reads
	// `I-807: main-branch dirty-non-state gate\nrefusing to commit...`
	// without a doubled prefix line.
	fmt.Fprintf(&b, "refusing to commit + push %d non-agent-state file(s) on branch %q:\n", len(offenders), currentBranch)
	for _, p := range offenders {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	fmt.Fprintf(&b, "Computed state prefix: %q\n", itemsPrefix)
	if onMain {
		b.WriteString("\nBranch is main — this would bypass PR review.\n")
		b.WriteString("Recovery: open a feature branch (git checkout -b fix/<id>-<slug>), commit these files there, push, and open a PR.\n")
		b.WriteString("Operator override (one-off bypass): ST_SYNC_ALLOW_NON_STATE=1 (or ST_SYNC_ALLOW_MAIN=1)\n")
	} else {
		fmt.Fprintf(&b, "\nBranch %q — non-state edits would auto-sync without PR review.\n", currentBranch)
		b.WriteString("Recovery: commit these files through the normal PR flow (git add, git commit, gh pr create).\n")
		b.WriteString("Operator override (one-off bypass): ST_SYNC_ALLOW_NON_STATE=1\n")
	}
	wrapped := fmt.Errorf("%w\n%s", ErrI807MainBranchGate, b.String())

	if overrideSet {
		overrideVar := "ST_SYNC_ALLOW_NON_STATE=1"
		if allowMain && !allowNonState {
			overrideVar = "ST_SYNC_ALLOW_MAIN=1"
		}
		fmt.Fprintf(os.Stderr, "[I-765] %s — bypassing gate, would have refused:\n", overrideVar)
		for _, p := range offenders {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		fmt.Fprintf(os.Stderr, "Computed state prefix: %q\n", itemsPrefix)
		return nil
	}
	return wrapped
}

// flatLayoutAuditedOnce ensures the flat-layout audit-stderr fires at
// most once per process so the operator sees the gate-inactive notice
// without spamming every GitSync call.
var flatLayoutAuditedOnce sync.Once

func flatLayoutAuditOnce() {
	flatLayoutAuditedOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "[I-807/I-765] flat-layout workspace — non-state gate is inactive (items root == git toplevel; no non-state surface to enforce)")
	})
}

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

	// I-807/I-765: fail closed if the dirty working tree contains any
	// non-agent-state file, on any branch. I-807 introduced the gate for
	// main; I-765 broadened it to all branches.
	if err := checkNonStateGate(root); err != nil {
		return err
	}

	// Pre-pull: fetch and integrate remote changes before committing.
	// Snapshot locked items so the pull can't overwrite active work.
	// runNetGit applies a 60s deadline + SSH keepalive (I-1411).
	if s.cfg.Git.AutoPush {
		snap := snapshotLockedItems(s.cfg, root)
		_ = runNetGit(root, "pull", "--ff-only")
		restoreLockedItems(snap)
	}

	// Stage tracked-modified files within agent-state/ only. The `-- .`
	// pathspec scopes `-u` to the CWD (root = agent-state/), preventing
	// modern Git's unscoped `git add -u` from accidentally staging
	// tracked-modified files outside agent-state/ (e.g. peer agents'
	// uncommitted hook edits). I-1472. I-442's peer-WIP protection for
	// untracked files is unchanged — `-u` still ignores `??` paths.
	if err := gitCmd(root, "add", "-u", "--", "."); err != nil {
		return fmt.Errorf("git add -u: %w", err)
	}

	// I-1451: .st-git.lock is the git lock itself — acquireGitLock rewrites it
	// ("pid=N cmd=...") on EVERY st op. It's gitignored, but on workspaces where
	// it was committed before that rule it stays tracked, so `git add -u` above
	// re-stages its churn every sync — polluting history and leaving the working
	// tree perpetually dirty (blocks session-stop). The workspace is a single
	// shared clone, so agents read the file directly and it never needs to be in
	// git. Drop it from the index every sync: untracks it on the first run
	// (commits the removal), then a harmless no-op (--ignore-unmatch).
	_ = gitCmd(root, "rm", "--cached", "--ignore-unmatch", ".st-git.lock")

	// I-575: also stage untracked-or-modified files inside the
	// agent-state plan-files subdirectory. `.plans/<id>.md` files are
	// dropped by `st prep` and `st start` and are unambiguously
	// agent-state — they have to land in a commit or the session-stop
	// hook nags every turn until the operator manually
	// `git add agent-state/.plans/<file>`.
	//
	// IMPORTANT — narrow scope to `.plans/` ONLY, NOT the full item-dir.
	// I-442's protection covers peer agents' brand-new untracked item
	// files in `issues/` / `tasks/` (e.g. another agent's `st create`
	// crashed before its own GitSync ran); those still must NOT be
	// swept here. Add new safe subdirectories to this list explicitly
	// as the agent-state convention evolves; do not switch to `.` or
	// `-A`.
	for _, sub := range autoStageSubdirs {
		subPath := filepath.Join(root, sub)
		if _, err := os.Stat(subPath); os.IsNotExist(err) {
			continue
		}
		if err := gitCmd(root, "add", "--", sub); err != nil {
			return fmt.Errorf("git add agent-state/%s: %w", sub, err)
		}
	}

	// Stage explicit new files (callers that create files pass them).
	// Reject paths outside `root` — defense in depth for the I-442
	// canonical-clone bleed. A bugged caller passing a sibling agent's
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
	// I-1593: do NOT early-return when there is nothing NEW to stage. Agent-state
	// commits that were committed locally but never pushed — e.g. a prior sync that
	// committed then failed to push, or a push skipped while a feature branch was
	// checked out — must still be pushed + ground-truth-verified. Returning nil here
	// printed "Synced." while local main stayed AHEAD of origin (the false-success
	// this fixes). So: the COMMIT step is conditional on having staged changes, but
	// the push + verifyPushLanded below run UNCONDITIONALLY, so a "Synced." always
	// means local main == origin/main (Inv 3: sync success is a verified postcondition).
	if strings.TrimSpace(cached) != "" {
		// I-594: detect cross-attribution — parallel `st update` calls each write
		// to the working tree before holding the git lock, so `git add -u` above
		// may have staged OTHER items' files alongside this call's intended changes.
		// When detected, replace the caller's single-item message with a bundle
		// message listing all staged files so git history stays accurate.
		//
		// Pass `root` so the function can normalize git-toplevel-relative paths
		// (e.g. "agent-state/tasks/I-123.md") to ItemDir-relative paths
		// ("tasks/I-123.md") before classifying them — required for nested-layout
		// workspaces where ItemDir is a subdirectory of the git repository root.
		message = synthesizeBundleMessage(root, message, cached)

		// I-1313: commit the staged agent-state onto refs/heads/main via
		// plumbing, NEVER `git commit` onto HEAD. When a code feature branch is
		// checked out, `git commit` would strand the agent-state commit on that
		// branch (PR pollution + "unpushed" nag) while pushWithRetry pushes the
		// LOCAL main ref that never received it — the routing bug this fixes.
		// Building the commit on refs/heads/main keeps agent-state fully
		// decoupled from the working-tree branch; the existing pushWithRetry /
		// replay handle staleness and same-file peer conflicts at push time.
		if _, err := s.commitStagedOntoMain(root, message); err != nil {
			return fmt.Errorf("commit onto main: %w", err)
		}

		// Clear the real index. The just-staged changes were committed onto
		// refs/heads/main, not onto HEAD, so they must not linger staged
		// against the working-tree branch (a subsequent `git add -u` / sync
		// would otherwise re-stage and re-commit them). Mixed reset to HEAD
		// leaves the working-tree files intact (they now match main) while
		// unstaging them. On main this is a no-op (HEAD already advanced).
		if err := gitCmdQuiet(root, "reset", "-q"); err != nil {
			return fmt.Errorf("git reset after main commit: %w", err)
		}

		// I-1451: on feature branches, git reset -q resets the real index to the
		// feature branch HEAD, which may still have .st-git.lock tracked (the
		// deletion was committed only onto refs/heads/main). Remove it again so
		// git add -u on the next sync doesn't re-stage it and dirty the index.
		_ = gitCmdQuiet(root, "rm", "--cached", "--ignore-unmatch", "--", ".st-git.lock")
	}

	// Push with retry
	if s.cfg.Git.AutoPush {
		if err := s.pushWithRetry(root, 3); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
		// I-684: post-push GROUND-TRUTH verification. pushWithRetry's nil
		// return is not proof the remote advanced — the plumbing replay
		// (I-501) can reshape local history and return nil without the
		// stranded commit(s) reaching origin, and a rejected push must
		// never read as success. Assert the local main HEAD is actually
		// contained in origin/main; if not, the durability primitive
		// failed and agent-state is stranded LOCAL-ONLY — return loudly
		// so `st sync` exits non-zero instead of printing `Synced.`
		// (operator silent-failure / demo-killer class).
		if err := verifyPushLanded(root); err != nil {
			return err
		}
	}

	return nil
}

// commitStagedOntoMain builds a commit containing the currently-staged
// agent-state changes on top of refs/heads/main and advances that ref —
// without ever touching the working-tree HEAD/index. It is the I-1313 fix
// for the routing bug where `git commit` landed agent-state on whatever
// feature branch happened to be checked out.
//
// Mechanism (same plumbing as replayCommitOnFetchedMain): seed a temp index
// from main's tree, overlay each staged path (hash-object the working-tree
// blob, or remove if deleted), write-tree, commit-tree -p main, and
// update-ref refs/heads/main with a compare-and-swap on the old value so a
// racing local writer can't be silently lost. The working tree and real
// index are never mutated here; the caller resets the real index afterward.
//
// Staleness / conflicts: this commits on whatever local refs/heads/main is.
// If origin/main has advanced, pushWithRetry's replay rebuilds the commit on
// the freshly-fetched origin/main and refuses (ErrPushDiverged) on a
// same-file peer edit — so no extra fetch/merge logic is needed here.
//
// Fallback: if refs/heads/main does not exist (flat-layout fixtures /
// single-branch repos), it targets the symbolic HEAD branch instead, so the
// behavior on such repos is identical to the previous `git commit`.
func (s *Store) commitStagedOntoMain(root, message string) (string, error) {
	// All path-sensitive plumbing here runs against the git TOPLEVEL. The
	// commit tree spans the whole repo, and `git diff --cached --name-only`
	// emits toplevel-relative paths — joining those onto `root` (which in a
	// nested layout is the `agent-state/` subdir) would double-prefix and
	// mis-stat every file. Resolve the toplevel once and anchor everything
	// (stat, hash-object, update-index) to it.
	toplevelOut, err := gitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("rev-parse --show-toplevel: %w", err)
	}
	toplevel := strings.TrimSpace(toplevelOut)

	// Resolve the base ref. Prefer refs/heads/main; fall back to HEAD's
	// branch when main is absent (flat-layout fixtures / single-branch repos).
	ref := "refs/heads/main"
	baseOut, err := gitOutput(toplevel, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || strings.TrimSpace(baseOut) == "" {
		symOut, symErr := gitOutput(toplevel, "symbolic-ref", "-q", "HEAD")
		if symErr != nil || strings.TrimSpace(symOut) == "" {
			return "", fmt.Errorf("commitStagedOntoMain: no refs/heads/main and HEAD is detached")
		}
		ref = strings.TrimSpace(symOut)
		baseOut, err = gitOutput(toplevel, "rev-parse", "--verify", ref)
		if err != nil {
			return "", fmt.Errorf("rev-parse %s: %w", ref, err)
		}
	}
	baseSHA := strings.TrimSpace(baseOut)

	// Pull the staged changes as raw entries — each is
	//   :<srcmode> <dstmode> <srcsha> <dstsha> <status>\0<path>\0
	// This gives the already-hashed staged blob AND its exact mode (so the
	// executable bit and symlinks survive), and -z makes paths with spaces or
	// non-ASCII bytes safe — unlike --name-only, which octal-quotes them and
	// would mis-stat. --no-renames keeps each change as a single-path A/M/D/T
	// entry. Reusing the staged blob directly means no re-hash and exact parity
	// with what `git commit` would have written.
	// --no-abbrev: full 40-char blob SHAs (update-index --cacheinfo rejects
	// abbreviated ones, which --raw emits by default).
	rawOut, err := gitOutput(toplevel, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev")
	if err != nil {
		return "", fmt.Errorf("diff --cached --raw: %w", err)
	}
	type stagedEntry struct{ mode, sha, status, path string }
	var entries []stagedEntry
	// -z stream alternates meta\0path\0meta\0path\0…
	rawTokens := strings.Split(rawOut, "\x00")
	for i := 0; i+1 < len(rawTokens); i += 2 {
		meta, path := rawTokens[i], rawTokens[i+1]
		if meta == "" || path == "" {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(meta, ":")) // [srcmode dstmode srcsha dstsha status]
		if len(f) < 5 {
			continue
		}
		entries = append(entries, stagedEntry{mode: f[1], sha: f[3], status: f[4], path: path})
	}
	if len(entries) == 0 {
		return baseSHA, nil
	}

	// Temp index seeded from base's tree, so we overlay onto main's content
	// (not HEAD's). Place it inside the REAL git dir.
	gitDirOut, err := gitOutput(toplevel, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("rev-parse --git-dir: %w", err)
	}
	gitDir := strings.TrimSpace(gitDirOut)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(toplevel, gitDir)
	}
	tmpIdx, err := os.CreateTemp(gitDir, "index.stcommit-*")
	if err != nil {
		return "", fmt.Errorf("create temp index: %w", err)
	}
	tmpIdxPath := tmpIdx.Name()
	tmpIdx.Close()
	defer os.Remove(tmpIdxPath)
	env := []string{"GIT_INDEX_FILE=" + tmpIdxPath}

	if err := gitCmdEnv(toplevel, env, "read-tree", baseSHA); err != nil {
		return "", fmt.Errorf("read-tree %s into temp index: %w", baseSHA, err)
	}

	for _, e := range entries {
		// Deletion (status D / dstmode 000000 / null dstsha): remove from the
		// temp index. --force-remove (not --remove): the staged index already
		// says "deleted", so the commit must drop it regardless of whether the
		// file still exists on disk. Plain --remove only drops MISSING files, so
		// a staged deletion of a still-present file (e.g. I-1451's live
		// .st-git.lock) would be silently ignored and survive in the tree.
		if e.status == "D" || e.mode == "000000" || strings.Trim(e.sha, "0") == "" {
			if err := gitCmdEnv(toplevel, env, "update-index", "--force-remove", "--", e.path); err != nil {
				return "", fmt.Errorf("update-index --force-remove %q: %w", e.path, err)
			}
			continue
		}
		// Overlay the already-staged blob at its exact mode (preserves the
		// executable bit and symlinks — no re-hash needed).
		if err := gitCmdEnv(toplevel, env, "update-index", "--add", "--cacheinfo",
			e.mode+","+e.sha+","+e.path); err != nil {
			return "", fmt.Errorf("update-index %q: %w", e.path, err)
		}
	}

	treeOut, err := gitOutputEnv(toplevel, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)

	// If the overlaid tree is identical to main's tree, the working-tree
	// agent-state already matches main (e.g. a re-sync on a feature branch
	// whose change was committed to main on a prior sync). Skip the commit
	// so we don't accumulate empty commits on main.
	baseTreeOut, err := gitOutput(toplevel, "rev-parse", baseSHA+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("rev-parse %s^{tree}: %w", baseSHA, err)
	}
	if strings.TrimSpace(baseTreeOut) == tree {
		return baseSHA, nil
	}

	commitOut, err := gitOutputStdin(toplevel, strings.TrimRight(message, "\n"),
		"commit-tree", tree, "-p", baseSHA)
	if err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	newCommit := strings.TrimSpace(commitOut)

	// Compare-and-swap: refuse if main moved under us (racing local writer).
	if err := gitCmdQuiet(toplevel, "update-ref", ref, newCommit, baseSHA); err != nil {
		return "", fmt.Errorf("update-ref %s (main moved concurrently?): %w", ref, err)
	}
	return newCommit, nil
}

// verifyPushLanded confirms the local `refs/heads/main` HEAD is reachable
// from `refs/remotes/origin/main` after a push — i.e. the remote genuinely
// received our commit. It re-fetches origin/main then uses
// `merge-base --is-ancestor` (exit 0 ⇔ localHEAD is an ancestor of, or
// equal to, origin/main). Each distinct failure mode gets its OWN
// actionable diagnostic — the durability primitive must not cry wolf with
// the wrong remediation (that erodes trust in the exact signal I-684
// exists to make trustworthy):
//   - fetch failed                 → the fetch's stderr, surfaced verbatim
//   - no refs/remotes/origin/main  → "no upstream ref" (NOT "oversized file")
//   - localHEAD not an ancestor    → genuinely stranded local-only
//
// CONCURRENCY CONTRACT: this MUST be called while GitSync's `.st-git.lock`
// is held (it re-fetches and reads the remote-tracking ref, and relies on
// the single-writer invariant — see the lock comment ~L211). It is invoked
// from GitSync before the deferred unlock fires; do NOT hoist it out of
// GitSync / past unlock() or the I-501 fetch-during-sync race returns.
func verifyPushLanded(root string) error {
	localOut, err := gitOutput(root, "rev-parse", "refs/heads/main")
	if err != nil {
		return fmt.Errorf("post-push verify: cannot resolve local main: %w", err)
	}
	localHead := strings.TrimSpace(localOut)

	// Refresh the remote-tracking ref so the ancestor test reflects the
	// remote's true state, not a stale snapshot. A fetch failure is itself
	// a reason we cannot certify the sync — surface its stderr verbatim
	// (I-684 / "surface WHY" principle). runNetGitCapture adds the 60s
	// deadline + SSH keepalive so a stalled network op here cannot hold
	// .st-git.lock indefinitely (I-1411).
	if fetchErr, err := runNetGitCapture(root, "fetch", "origin", "main"); err != nil {
		detail := strings.TrimSpace(fetchErr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("post-push verify: cannot fetch origin/main to confirm the push landed (agent-state may be stranded local-only): %s", detail)
	}

	// Distinguish "no remote-tracking ref" (exit 128 / missing upstream —
	// NOT an oversized-file rejection) from "ref exists but does not
	// contain our commit" (the genuine stranded case). Conflating them
	// sends the operator down the wrong path.
	if err := gitCmdQuiet(root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); err != nil {
		return fmt.Errorf(
			"post-push verify: no refs/remotes/origin/main after fetch — cannot certify the push for local commit %s landed. Check `git remote -v` / the origin fetch refspec / upstream config; agent-state may be stranded local-only",
			shortSHA(localHead))
	}

	if err := gitCmdQuiet(root, "merge-base", "--is-ancestor", localHead, "refs/remotes/origin/main"); err != nil {
		originOut, _ := gitOutput(root, "rev-parse", "refs/remotes/origin/main")
		return fmt.Errorf(
			"git push reported success but origin/main does NOT contain the local commit %s (origin/main=%s) — refs did not advance; agent-state is stranded LOCAL-ONLY. Resolve the rejection (e.g. an oversized file / pre-receive gate) and re-run `st sync`",
			shortSHA(localHead), shortSHA(strings.TrimSpace(originOut)))
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(unknown)"
	}
	return s
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
		// I-684: capture stderr so a persistent rejection's error carries
		// the remote's reason (GH001 / pre-receive declined), not just
		// "exit status 1".
		// I-1411: runNetGitCapture applies a 60s deadline + SSH keepalive.
		pushErr, err := runNetGitCapture(root, "push", "origin", "refs/heads/main:refs/heads/main")
		if err == nil {
			return nil
		}

		if attempt == maxRetries {
			if pushErr != "" {
				return fmt.Errorf("%w\nremote: %s", err, pushErr)
			}
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
				// AND the remote's actual rejection text (GH001 /
				// pre-receive declined) so the operator sees WHY, not
				// just "exit status 1" (I-684).
				if pushErr != "" {
					return fmt.Errorf("%w: %v\nremote: %s", ErrPushRejectedButOriginUnchanged, err, pushErr)
				}
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
	// I-1411: runNetGit applies a 60s deadline + SSH keepalive.
	if err := runNetGit(root, "fetch", "origin", "main"); err != nil {
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
//     - ahead > 0 AND behind > 0: RefreshDiverged (true divergence;
//     caller must `git pull --rebase` or equivalent recovery).
//     - ahead > 0 AND behind == 0: RefreshAhead with AheadCount
//     (I-430: unpushed local commits, recoverable via `st sync`).
//     - behind == 0: RefreshUpToDate (silent success).
//     - behind > 0 with uncommitted blockers: RefreshBlocked.
//     - behind > 0 otherwise: `git pull --ff-only`. Success →
//     RefreshPulled (PulledCount = behind). Failure → RefreshBlocked.
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
	// runNetGit adds the 60s deadline + SSH keepalive so a stalled network
	// op here cannot hold .st-git.lock indefinitely (I-1411).
	if err := runNetGit(root, "pull", "--ff-only"); err != nil {
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

// runNetGit runs a git command that touches the network (pull, push, fetch)
// with a hard process deadline and SSH keepalive options (I-1411). A dead TCP
// link cannot hold .st-git.lock for more than gitNetworkTimeout.
func runNetGit(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitNetworkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCommand)
	return cmd.Run()
}

// runNetGitCapture is the capturing analogue of runNetGit, mirroring
// gitCapture: streams stdout/stderr to the operator live while also
// capturing stderr so the returned error carries the remote's reason text
// (I-684). Used for push, which may emit remote: error: GH001 messages.
func runNetGitCapture(dir string, args ...string) (stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitNetworkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCommand)
	cmd.Stdout = os.Stdout
	var buf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err = cmd.Run()
	return strings.TrimSpace(buf.String()), err
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

// gitCapture runs a git command, still streaming stdout/stderr to the
// operator (so a rejected push's `remote: error: GH001 …` is visible live),
// AND capturing stderr so the caller can fold the remote's actionable
// rejection text into the returned error instead of a bare "exit status 1"
// (I-684 — the durability primitive must surface WHY it failed).
func gitCapture(dir string, args ...string) (stderr string, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	var buf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err = cmd.Run()
	return strings.TrimSpace(buf.String()), err
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

// synthesizeBundleMessage detects cross-attribution in the staged file list
// and returns an accurate bundle commit message when unexpected item files
// are present. If the staged set matches what `message` implies, message is
// returned unchanged.
//
// Cross-attribution occurs when parallel `st update` processes each write to
// the working tree before acquiring the git lock, causing `git add -u` to
// stage multiple items' changes together. The resulting commit message must
// name all staged items — not just the one the caller intended to write —
// so git history stays auditable.
//
// root is passed so the function can strip the git-toplevel-relative prefix
// (e.g. "agent-state/") that git diff --name-only always emits, normalizing
// paths to ItemDir-relative form before classification. This is required in
// nested-layout workspaces where ItemDir is a subdirectory of the git root.
//
// Detection heuristic: for "st update: <id>.*" messages, any item file in
// tasks/, issues/, or archive/ whose base name doesn't match <id> is
// unexpected. Auto-stage subdirs (.plans/, .changelog/, .as/, .locks/) are
// always expected and never count as cross-attribution.
func synthesizeBundleMessage(root, message, cached string) string {
	// Only inspect single-item "st update: <id>.*" messages.
	if !strings.HasPrefix(message, "st update: ") {
		return message
	}
	suffix := strings.TrimPrefix(message, "st update: ")
	expectedID := suffix
	if dot := strings.IndexByte(suffix, '.'); dot >= 0 {
		expectedID = suffix[:dot]
	}

	// Strip the git-root prefix so all path comparisons use bare names like
	// "tasks/I-123.md" regardless of nested-layout depth. In flat layout
	// (ItemDir == git root) the prefix is "." and no stripping is needed.
	prefix := ""
	if top, err := gitOutput(root, "rev-parse", "--show-toplevel"); err == nil {
		top = strings.TrimSpace(top)
		if rel, err := filepath.Rel(top, root); err == nil && rel != "." && rel != "" {
			prefix = rel + "/"
		}
	}

	stripPrefix := func(p string) string {
		if prefix != "" {
			return strings.TrimPrefix(p, prefix)
		}
		return p
	}

	// isAutoStageDir returns true for directories that are always expected
	// alongside any single-item update (plan files, changelogs, sessions, locks).
	isAutoStageDir := func(dir string) bool {
		for _, known := range []string{".plans", ".changelog", ".as", ".locks"} {
			if dir == known || strings.HasPrefix(dir, known+"/") {
				return true
			}
		}
		return false
	}

	// Single pass: collect unexpected item files and all item IDs for the message.
	var unexpected []string
	var itemIDs []string
	for _, line := range strings.Split(strings.TrimSpace(cached), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rel := stripPrefix(line)
		dir := filepath.Dir(rel)
		if isAutoStageDir(dir) {
			continue
		}
		switch dir {
		case "tasks", "issues", "archive":
			id := strings.TrimSuffix(filepath.Base(rel), ".md")
			itemIDs = append(itemIDs, id)
			if !matchesItemID(filepath.Base(rel), expectedID) {
				unexpected = append(unexpected, rel)
			}
		}
	}

	if len(unexpected) == 0 {
		return message
	}

	// Cross-attribution detected: the staged set includes item files not written
	// by this call. Warn and build a bundle message listing all staged item IDs.
	fmt.Fprintf(os.Stderr,
		"[st sync] WARNING: cross-attribution detected — %d unexpected item file(s) staged alongside %q: %s; synthesizing bundle commit message\n",
		len(unexpected), message, strings.Join(unexpected, ", "))

	return "st sync batch: " + strings.Join(itemIDs, ", ")
}

// matchesItemID reports whether the file base name (e.g. "T-123-foo-bar.md")
// corresponds to the given item ID (e.g. "T-123"). The name matches if,
// after stripping ".md", it equals id or has id as a prefix followed by "-".
func matchesItemID(base, id string) bool {
	name := strings.TrimSuffix(base, ".md")
	return name == id || strings.HasPrefix(name, id+"-")
}
