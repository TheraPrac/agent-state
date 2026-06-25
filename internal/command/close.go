package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

// CloseOpts holds flags for the close command.
type CloseOpts struct {
	Reason string
	Force  bool // bypass gate enforcement
	// SkipTier2Revalidation skips close-time recomputation of applicable scope
	// suites from the current diff (--skip-tier2-revalidation flag). Use only
	// when the worktree is unavailable or the agent is confident the push-gate
	// already enforced the correct set.
	SkipTier2Revalidation bool
	// FilesOpts is passed to the LOC freeze step. Tests inject fake git/resolve
	// here; production callers leave it zero (real git + real worktree discovery).
	FilesOpts FilesOpts
	// ScopeCheckOpts is passed to closeScopeSuiteCheck. Tests inject fakes here.
	ScopeCheckOpts CloseScopeCheckOpts
}

// webE2EScopeSkipped reports whether the web_e2e scope suite was
// intentionally skipped (`st test <id> web_e2e --skip`). Evidence is
// stored flat at TestingEvidence["web_e2e"] (testrecord.go SetNested
// 2-level); the canonical readers (gates.go, uat.go) use this flat map,
// NOT a dotted Doc path — a 3-level GetNestedField path silently never
// matches (I-696 review fix). When skipped, the post-merge e2e gate is
// not applicable and must not block close.
func webE2EScopeSkipped(item *model.Item) bool {
	if item == nil || item.TestingEvidence == nil {
		return false
	}
	v, ok := item.TestingEvidence["web_e2e"]
	if !ok {
		return false
	}
	s, _ := v.(string)
	return strings.HasPrefix(strings.TrimSpace(s), "skip")
}

// closeUsage returns the canonical one-line usage plus a concrete example
// for `st close`, shown on every parse-error path (I-1305) so a confused
// caller always sees how to reassemble the full command in one step. The
// valid resolutions are taken from the live terminal statuses so the
// example stays honest if the vocabulary ever changes.
func closeUsage(id string, terminal []string) string {
	example := id
	if example == "" {
		example = "<id>"
	}
	return fmt.Sprintf(
		"usage: st close <id> <%s> [--reason <text>]\n  e.g. st close %s abandoned --reason superseded",
		strings.Join(terminal, "|"), example)
}

// correctResolution maps a near-miss resolution to its canonical terminal
// status (I-1305). It returns (canonical, true) when `input` is a
// case-only mismatch (`Done`→`done`) or a unique case-insensitive prefix
// of exactly one terminal status (`abandon`→`abandoned`,
// `archive`→`archived`, `don`→`done`). An exact match needs no correction
// and an ambiguous prefix (`a` → abandoned|archived) returns ok=false, so
// in both cases the normal validator handles the value.
func correctResolution(input string, terminal []string) (string, bool) {
	in := strings.ToLower(strings.TrimSpace(input))
	if in == "" {
		return input, false
	}
	var match string
	prefixHits := 0
	for _, ts := range terminal {
		if ts == in {
			// Exact after lowercasing — only a correction if the case
			// differed from the original input.
			return ts, ts != input
		}
		if strings.HasPrefix(ts, in) {
			match = ts
			prefixHits++
		}
	}
	if prefixHits == 1 {
		return match, true
	}
	return input, false
}

func Close(s *store.Store, cfg *config.Config, id, resolution string, opts CloseOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	tc, ok := cfg.Types[item.Type]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", item.Type)
		return 1
	}

	// I-433: legacy resolutions (resolved/wontfix/completed) are dead.
	// Catch them at the entry point with a migration pointer so muscle
	// memory `st close X resolved` doesn't silently become an "invalid
	// resolution" error with no breadcrumb to the new vocabulary.
	switch resolution {
	case "resolved", "completed":
		fmt.Fprintln(os.Stderr,
			"close: resolution \""+resolution+"\" is deprecated (I-433). "+
				"Use \"done\" instead — the unified vocabulary across both "+
				"tasks and issues is queued/active/done/abandoned/archived.")
		return 2
	case "wontfix":
		fmt.Fprintln(os.Stderr,
			"close: resolution \"wontfix\" is deprecated (I-433). "+
				"Use \"abandoned\" instead.")
		return 2
	}

	// I-1305: correct-and-confirm a near-miss resolution (a unique
	// case-insensitive prefix of exactly one terminal status, or a
	// case-only mismatch) so muscle-memory typos like `abandon`→
	// `abandoned`, `archive`→`archived`, or `Done`→`done` are accepted
	// with a confirmation note rather than dead-ending the caller.
	if corrected, ok := correctResolution(resolution, tc.TerminalStatuses); ok {
		fmt.Fprintf(os.Stderr, "close: interpreting %q as %q\n", resolution, corrected)
		resolution = corrected
	}

	// Resolution must be a valid terminal status
	validTerminal := false
	for _, ts := range tc.TerminalStatuses {
		if resolution == ts {
			validTerminal = true
			break
		}
	}
	if !validTerminal {
		// I-1305: every parse-error path prints the full corrected
		// invocation, not just the bare enum list, so the caller can
		// reassemble the whole command in one step.
		if resolution == "" {
			fmt.Fprintln(os.Stderr, "close: missing resolution")
		} else {
			fmt.Fprintf(os.Stderr, "close: invalid resolution %q — valid: %s\n",
				resolution, strings.Join(tc.TerminalStatuses, " "))
		}
		// Free text in the positional slot (a space), or a --reason
		// supplied with no resolution, is the classic "I put my reason
		// where the resolution goes" mistake — point at the separate flag.
		if strings.Contains(resolution, " ") || (resolution == "" && opts.Reason != "") {
			fmt.Fprintln(os.Stderr, "  note: the reason for closing goes in --reason, not the resolution slot")
		}
		fmt.Fprintln(os.Stderr, closeUsage(id, tc.TerminalStatuses))
		return 2
	}

	// Must be in active status (or start status for abandoned)
	if item.Status != tc.ActiveStatus && item.Status != tc.StartStatus {
		fmt.Fprintf(os.Stderr, "%s is %s — cannot close\n", id, item.Status)
		return 1
	}

	// If abandoning, require reason
	if resolution == "abandoned" || resolution == "declined" {
		if opts.Reason == "" {
			fmt.Fprintln(os.Stderr, "--reason is required when abandoning")
			return 2
		}
	}
	// Closed vocab gate for abandoned items (T-414). "declined" has different
	// semantics (idea triage rejection) and is intentionally exempt.
	if resolution == "abandoned" && !model.IsValidDropReason(opts.Reason) {
		fmt.Fprintf(os.Stderr,
			"close: --reason %q not valid for abandoned; must be one of: %s\n",
			opts.Reason, model.ValidDropReasonsJoined())
		return 2
	}

	// Gate enforcement — skip for abandon/declined since those bypass gates
	// by design. (wontfix is rejected earlier per I-433.)
	if !opts.Force && resolution != "abandoned" && resolution != "declined" {
		// T-393: close-time mirror of the push-gate Tier 2 check. Recompute
		// applicable scope suites from the current HEAD diff in the worktree
		// and reject close when any applicable suite has no recorded evidence.
		scopeOpts := opts.ScopeCheckOpts
		scopeOpts.Skip = scopeOpts.Skip || opts.SkipTier2Revalidation
		if msg := closeScopeSuiteCheck(item, cfg, scopeOpts); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			return 1
		}

		results := validate.EvaluateGates(item, "close", cfg, s.All())
		if !validate.GatesPassed(results) {
			failure := validate.FirstFailure(results)
			fmt.Fprintf(os.Stderr, "gate %q failed: %s\n", failure.Gate, failure.Message)
			fmt.Fprintln(os.Stderr, "use --force to bypass gates")
			return 1
		}
	}

	// I-696: post-merge local-main full-e2e gate. There is no GHA e2e
	// (I-637), so this is the only verification that the *merged* code is
	// releasable before the item closes / main becomes the deploy artifact.
	// Runs OUTSIDE the Mutate lock (slow, like the LOC snapshot below).
	// Self-gates on applicability (PR touched an e2e spec + PostMergeCmd
	// configured); honors an explicit web_e2e scope skip.
	recordPostMerge := false
	if !opts.Force && resolution != "abandoned" && resolution != "declined" {
		if !webE2EScopeSkipped(item) {
			ran, msg := postMergeE2E(cfg, id)
			if msg != "" {
				fmt.Fprintln(os.Stderr, msg)
				fmt.Fprintln(os.Stderr, "post-merge e2e gate failed (full suite vs merged local main); fix and re-close, or use --force to bypass")
				return 1
			}
			// Record audit evidence only when the gate actually ran
			// (ran==true); not-applicable items must not get a spurious
			// "pass" marker.
			recordPostMerge = ran
		}
	}

	// Transition
	oldStatus := item.Status
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	// Compute the cross-repo LOC snapshot OUTSIDE the lock so the slow
	// git diff doesn't block other Mutate callers waiting on this item.
	// The result is a pure data struct that we apply inside the closure.
	locSnapshot, _ := computeLOCSnapshot(s, cfg, id, opts.FilesOpts)

	// Capture the live claimed_by under the lock; do the session-manager
	// I/O AFTER the Mutate so we never act on a stale snapshot.
	var claimedBy string

	if err := s.Mutate(id, func(item *model.Item) error {
		claimedBy = item.ClaimedBy
		item.Doc.SetField("status", resolution)
		item.Status = resolution
		item.Doc.SetField("completed", nowStr)
		item.Doc.SetField("last_touched", nowStr)

		// T-414: write dropped_reason for abandoned items.
		if resolution == "abandoned" && opts.Reason != "" {
			item.DroppedReason = opts.Reason
			item.Doc.SetField("dropped_reason", opts.Reason)
		}

		// I-447: closing the item is the terminal lifecycle position.
		// Always set (not advance) so abandon paths surface as "closed"
		// in render too.
		item.SetNested("delivery", "stage", "closed")

		// I-696: persist the post-merge e2e pass as scope-suite evidence so
		// the gate is auditable in st show / st uat after close.
		if recordPostMerge {
			item.SetNested("testing_evidence", "web_e2e_postmerge", "pass "+nowStr)
		}

		// Record completion time tracking
		item.SetNested("time_tracking", "completed_at", nowStr)

		// I-1318: compute work duration from session-scoped accumulated time.
		// accumulated_seconds holds the sum of all completed work periods;
		// session_started_at is the current period's start (if the timer is
		// running). Together they give actual work time, not wall-clock.
		//
		// I-1335: work_duration_seconds carries ONLY measured active time.
		// When no session timer data exists the field is omitted (null) —
		// never the started_at wall-clock span, which previously made
		// measured and garbage values indistinguishable downstream.
		var workDur time.Duration
		haveSessionFields := false
		{
			accSecs := 0
			if v, ok := getNestedField(item, "time_tracking", "accumulated_seconds"); ok && v != "" {
				fmt.Sscanf(v, "%d", &accSecs) //nolint:errcheck
				haveSessionFields = true
			}
			if sessStart, ok := getNestedField(item, "time_tracking", "session_started_at"); ok && sessStart != "" {
				haveSessionFields = true
				if t0, err := time.Parse(time.RFC3339, sessStart); err == nil {
					// Use time.Now() here (not the pre-LOC-snapshot `now`) so that
					// git-diff latency in computeLOCSnapshot is included in work time.
					if elapsed := time.Now().Sub(t0); elapsed > 0 {
						workDur += elapsed
					}
				}
			}
			if haveSessionFields {
				workDur += time.Duration(accSecs) * time.Second
			}
		}
		item.SetNested("time_tracking", "session_started_at", "")
		if haveSessionFields {
			item.SetNested("time_tracking", "work_duration_seconds",
				fmt.Sprintf("%d", int(workDur.Seconds())))
			// Persist the final measured total so closed items are
			// self-consistent: work_duration_seconds == accumulated_seconds.
			// `st timer scrub` relies on this invariant to tell measured
			// values from legacy wall-clock fallbacks.
			item.SetNested("time_tracking", "accumulated_seconds",
				fmt.Sprintf("%d", int(workDur.Seconds())))
		}
		// total_duration_seconds and the wall_time fields carry the
		// wall-clock span (completed_at − started_at), independent of the
		// timer. Anchor on the same `now` that produced completed_at (I-514:
		// one anchor for all span fields) and always write when started_at
		// parses — clamp clock skew to 0 rather than omitting, so field
		// presence keeps meaning "span recorded".
		if startedAt, ok := getNestedField(item, "time_tracking", "started_at"); ok && startedAt != "" {
			if t0, err := time.Parse(time.RFC3339, startedAt); err == nil {
				span := now.Sub(t0)
				if span < 0 {
					span = 0
				}
				item.SetNested("time_tracking", "total_duration_seconds",
					fmt.Sprintf("%d", int(span.Seconds())))
				item.SetNested("time_tracking", "wall_time_hours", fmt.Sprintf("%.1f", span.Hours()))
				item.SetNested("time_tracking", "total_wall_time", formatDuration(span))
			}
		}

		// Apply the precomputed LOC snapshot. computed outside the lock
		// (see locSnapshot above) so this step is pure transformation.
		applyLOCSnapshot(item, locSnapshot)

		// Total AI time — prefer the new ai_time_seconds field
		// (SessionLog output); fall back to legacy ai_duration_seconds so
		// pre-rewire items keep working.
		var aiSecs int
		if v, ok := getNestedField(item, "time_tracking", "ai_time_seconds"); ok && v != "" {
			fmt.Sscanf(v, "%d", &aiSecs)
		} else if v, ok := getNestedField(item, "time_tracking", "ai_duration_seconds"); ok && v != "" {
			fmt.Sscanf(v, "%d", &aiSecs)
		}
		if aiSecs > 0 {
			item.SetNested("time_tracking", "total_ai_time", formatDuration(time.Duration(aiSecs)*time.Second))
		}

		// AI cost summary
		if aiCost, ok := getNestedField(item, "time_tracking", "ai_cost_usd"); ok && aiCost != "" {
			item.SetNested("time_tracking", "total_ai_cost_usd", aiCost)
		}

		// Token totals
		if v, ok := getNestedField(item, "time_tracking", "input_tokens"); ok && v != "" {
			item.SetNested("time_tracking", "total_input_tokens", v)
		}
		if v, ok := getNestedField(item, "time_tracking", "output_tokens"); ok && v != "" {
			item.SetNested("time_tracking", "total_output_tokens", v)
		}
		if v, ok := getNestedField(item, "time_tracking", "total_tokens"); ok && v != "" {
			item.SetNested("time_tracking", "total_tokens_final", v)
		}

		if opts.Reason != "" {
			item.Doc.SetField("resolution", opts.Reason)
		}

		if item.ClaimedBy != "" {
			item.ClaimedBy = ""
			item.ClaimedAt = ""
			item.Doc.SetField("claimed_by", "")
			item.Doc.SetField("claimed_at", "")
		}

		// I-232: clear work_tracking.branch/worktree when the recorded
		// branch no longer exists anywhere (any configured repo, local
		// OR remote). Worktrees on the disk are torn down on `st
		// finish`, but the branch field can linger pointing at a
		// branch that's been merged + deleted, misleading
		// `st queue show` and `st stack` displays. Best-effort — a
		// missing branch check (no configured repos, no .git, etc.)
		// simply leaves the fields alone.
		if recordedBranch, ok := getNestedField(item, "work_tracking", "branch"); ok && recordedBranch != "" {
			if !branchExistsAnywhere(cfg, recordedBranch) {
				item.Doc.RemoveNestedField("work_tracking.branch")
				item.Doc.RemoveNestedField("work_tracking.worktree")
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	if claimedBy != "" {
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		_ = mgr.RemoveClaim(claimedBy, id)
	}

	// Release item lock
	store.UnlockItem(cfg, id)

	// Move to correct directory
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	// I-472: sweep stale duplicates left behind by peer-merge events.
	// Modern Move uses os.Rename and shouldn't leave a duplicate, but
	// a peer agent's feature branch created when this item was still
	// active can resurrect issues/<id>-*.md when that branch merges
	// back to main — leaving two files for the same ID. `git add -u`
	// (in GitSync below) auto-stages the deletions so the cleanup
	// rides along in the close commit. Only same-basename matches are
	// removed; ID-collisions (different items sharing an ID prefix)
	// are left for human triage.
	if removed, derr := s.RemoveStaleDuplicates(id); derr != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup of stale duplicates for %s failed: %v\n", id, derr)
	} else {
		for _, p := range removed {
			fmt.Fprintf(os.Stderr, "removed stale duplicate: %s\n", p)
		}
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "close", Field: "status",
		OldValue: oldStatus, NewValue: resolution,
		Reason: opts.Reason,
	})

	fmt.Printf("Closed %s — %s (%s)\n", id, item.Title, resolution)

	// Auto-remove from the work queue. A closed item staying in the queue
	// just clutters `st queue show` and misleads the operator about what's
	// left. Silent if the item wasn't queued.
	if removed, qerr := removeFromQueueSilently(cfg, id); qerr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to remove %s from queue: %v\n", id, qerr)
	} else if removed {
		fmt.Printf("  also removed from queue\n")
	}

	// Auto-pop stack if this item is on top
	stack := LoadStack(cfg)
	if len(stack) > 0 && stack[len(stack)-1].ID == id {
		stack = stack[:len(stack)-1]
		// Skip any resolved items below
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if topItem, ok := s.Get(top.ID); ok && cfg.IsTerminalStatus(topItem.Type, topItem.Status) {
				fmt.Printf("  %s also resolved — skipping\n", top.ID)
				stack = stack[:len(stack)-1]
				continue
			}
			break
		}
		SaveStack(cfg, stack)
		// I-1302: record the item we returned to (or "" if stack is now
		// empty) so a reflexive `st pop` after close can detect the
		// double-pop and no-op instead of silently dropping the parent.
		returnedToID := ""
		if len(stack) > 0 {
			returnedToID = stack[len(stack)-1].ID
		}
		setCloseReturn(cfg, returnedToID)
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			if topItem, ok := s.Get(top.ID); ok {
				fmt.Printf("Returning to %s — %s\n", top.ID, topItem.Title)
			}
		} else {
			fmt.Println("Stack is now empty")
		}
	} else {
		// I-232: closed item wasn't on top, but may be sitting
		// mid-stack (operator pushed something on top, then closed
		// the lower one). removeFromStackSilently no-ops when not
		// present, so this is the safety net.
		if removed, serr := removeFromStackSilently(cfg, id); serr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove %s from stack: %v\n", id, serr)
		} else if removed {
			fmt.Printf("  also removed from stack (mid-stack ghost)\n")
		}
	}

	// Commit + push the close to git immediately. Previously the move to
	// archive/ and status change sat uncommitted until the caller happened
	// to run `st sync` or until `st run`'s deferred sync caught it. That
	// gap allowed silent-revert incidents (e.g. I-164): a subsequent st
	// command's PersistentPreRunE → GitPull destroyed the uncommitted
	// move, and "Closed" turned out to be a lie. Best-effort for transient
	// errors; gate refusal (I-807) propagates non-zero so the operator is
	// not misled about persistence.
	// I-442: pass the post-Move path. The Move from issues/→archive/
	// (or tasks/→archive/) is a rename — git add -u catches the
	// delete-from-old, but the new path is untracked and needs
	// explicit staging.
	newPath, _ := s.Path(id)
	syncErr := autoSync(s, fmt.Sprintf("st close: %s (%s)", id, resolution), newPath)

	// I-1439: the pre-sync RemoveStaleDuplicates above runs BEFORE
	// autoSync's internal `git pull --ff-only`. A peer feature-branch
	// created while this item was still active can resurrect
	// issues/<id>-*.md (or tasks/<id>-*.md) when it merges to main —
	// and that merge is pulled in DURING this close's sync, landing the
	// duplicate in the close commit itself where the earlier sweep could
	// not have seen it (this is exactly how I-1441 ended up with copies
	// in both issues/ and archive/, surfacing as a `duplicate id` warning
	// on every subsequent st invocation until a manual `st check --fix`).
	// Re-sweep post-pull; if anything was resurrected, commit the cleanup
	// so the collision never reaches `st check`. No-op (and no extra
	// commit) in the common case where the pull brought nothing back.
	if removed, derr := s.RemoveStaleDuplicates(id); derr != nil {
		fmt.Fprintf(os.Stderr, "warning: post-sync cleanup of stale duplicates for %s failed: %v\n", id, derr)
	} else if len(removed) > 0 {
		for _, p := range removed {
			fmt.Fprintf(os.Stderr, "removed stale duplicate (post-sync): %s\n", p)
		}
		if err := autoSync(s, fmt.Sprintf("st close: %s — sweep peer-resurrected duplicate", id)); err != nil {
			syncErr = err
		}
	}

	// Always run post-close cleanup even when sync failed — the item is
	// already durably on disk, and skipping sprint/epic auto-archive or
	// worktree cleanup would leave stale state that outlives the gate.
	autoArchiveSprintAndEpic(s, cfg, item.Sprint)
	if cleaned, _ := TryAutoFinishWorktree(cfg, id); cleaned {
		fmt.Printf("  also finished worktree\n")
	}

	// I-1587: remove the item's local test-log mirror directory written by
	// `st test`. Best-effort — a missing directory is a no-op.
	_ = os.RemoveAll(filepath.Join(cfg.Root(), ".as", "test-logs", id))

	if syncErr != nil {
		return 1
	}

	// Refresh the dashboard snapshot. Detached so it doesn't block or fail
	// the close; best-effort — a missing or crashing script is ignored.
	script := filepath.Join(cfg.Root(), "scripts", "agent-state-dashboard.py")
	if _, err := os.Stat(script); err == nil {
		dashCmd := exec.Command("python3", script)
		dashCmd.Stdout = nil
		dashCmd.Stderr = nil
		if err := dashCmd.Start(); err == nil {
			_ = dashCmd.Process.Release()
		}
	}

	return 0
}

// branchExistsAnywhere reports whether `branch` is present on any of
// the configured repos, either locally or on origin. Returns true when
// the branch check can't be performed (no worktree config, no .git
// directories) so the caller's "clear stale fields" path stays
// conservative: it only clears when we're confident the branch is
// gone. I-232.
func branchExistsAnywhere(cfg *config.Config, branch string) bool {
	if cfg == nil || cfg.Worktree == nil || len(cfg.Worktree.Repos) == 0 {
		return true // conservative — no config to check against
	}
	parentDir := cfg.RepoParent() // I-778: agent-aware repo parent resolution
	checkedAny := false
	for _, repoShort := range cfg.Worktree.Repos {
		repoDir := cfg.Worktree.RepoMap[repoShort]
		if repoDir == "" {
			repoDir = repoShort
		}
		repoPath := filepath.Join(parentDir, repoDir)
		if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
			continue // repo missing — skip, but don't claim "gone"
		}
		checkedAny = true
		if branchExists(repoPath, branch) || remoteBranchExists(repoPath, branch) {
			return true
		}
	}
	if !checkedAny {
		return true // no repo we could query — stay conservative
	}
	return false
}

// autoArchiveSprintAndEpic checks if all items in the sprint are terminal.
// If so, archives the sprint. Then checks if all sprints in the epic are
// archived, and if so, archives the epic. This runs after st close so that
// completed sprints/epics are automatically cleaned up without manual
// st sprint archive / st epic archive commands.
func autoArchiveSprintAndEpic(s *store.Store, cfg *config.Config, sprintID string) {
	if sprintID == "" {
		return
	}

	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		return
	}

	sp, err := reg.SprintByID(sprintID)
	if err != nil || sp.Status != "active" {
		return
	}

	// Check if all items in the sprint are terminal.
	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		if !cfg.IsTerminalStatus(item.Type, item.Status) {
			return // at least one item still active — don't archive
		}
	}

	// All items terminal — archive the sprint.
	sp.Status = "archived"
	fmt.Printf("[auto-archive] All items in sprint %q complete — archived\n", sp.Title)

	// Check if all sprints in the parent epic are now archived.
	epicID := sp.Epic
	if epicID != "" {
		allDone := true
		for _, es := range reg.Sprints {
			if es.Epic == epicID && es.Status != "archived" {
				allDone = false
				break
			}
		}
		if allDone {
			for i := range reg.Epics {
				if reg.Epics[i].ID == epicID {
					reg.Epics[i].Status = "archived"
					fmt.Printf("[auto-archive] All sprints in epic %q complete — archived\n", reg.Epics[i].Title)
					break
				}
			}
		}
	}

	if err := reg.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-archive save failed: %v\n", err)
	}
}

// computeLOCSnapshot runs the cross-repo git diff and returns the
// computed result. Pure read-only — does no item-file mutation. Safe
// to call OUTSIDE Mutate so the lock isn't held during slow git work.
// Returns (result, ok). ok=false means the caller should freeze zeros
// (the helper has already logged the warning).
func computeLOCSnapshot(s *store.Store, cfg *config.Config, itemID string, opts FilesOpts) (*FilesResult, bool) {
	res, code := ComputeFileChanges(s, cfg, itemID, opts)
	if code != 0 {
		fmt.Fprintf(os.Stderr, "warning: LOC snapshot for %s failed (code %d) — freezing zeros\n", itemID, code)
		return nil, false
	}
	return &res, true
}

// applyLOCSnapshot writes the precomputed snapshot into the item's
// time_tracking and files_changed lists. Pure transformation — no I/O.
// Safe to call INSIDE a Mutate closure.
func applyLOCSnapshot(item *modelItemRef, res *FilesResult) {
	if res == nil {
		return
	}
	item.SetNested("time_tracking", "lines_added", fmt.Sprintf("%d", res.Totals.Added))
	item.SetNested("time_tracking", "lines_removed", fmt.Sprintf("%d", res.Totals.Removed))
	item.SetNested("time_tracking", "lines_net", fmt.Sprintf("%+d", res.Totals.Net))
	item.SetNested("time_tracking", "files_changed_count", fmt.Sprintf("%d", res.Totals.Files))

	for _, r := range res.Repos {
		line := fmt.Sprintf("%s: files=%d added=%d removed=%d net=%+d",
			r.Repo, r.Files, r.Added, r.Removed, r.Net)
		if !updateListLine(item, "time_tracking", "by_repo",
			func(raw string) bool {
				t := raw
				if idx := strings.Index(t, ":"); idx >= 0 {
					return t[:idx] == r.Repo
				}
				return false
			}, line) {
			item.Doc.AppendToNestedList("time_tracking", "by_repo", line)
		}
	}

	for _, f := range res.Files {
		line := fmt.Sprintf("%s %s %s +%d -%d (%+d) [%s]",
			f.Repo, f.Action, f.Path, f.Added, f.Removed, f.Net, f.Type)
		item.Doc.AppendToNestedList("time_tracking", "files_changed", line)
	}

	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: LOC freeze: %s\n", w)
	}
}

// modelItemRef is an alias pin: the concrete type is *model.Item. Named
// separately so the freezeLOCSnapshot signature reads cleanly without
// pulling another import just for the type expression.
type modelItemRef = model.Item
