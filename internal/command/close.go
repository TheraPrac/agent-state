package command

import (
	"fmt"
	"os"
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
	// FilesOpts is passed to the LOC freeze step. Tests inject fake git/resolve
	// here; production callers leave it zero (real git + real worktree discovery).
	FilesOpts FilesOpts
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

	// Resolution must be a valid terminal status
	validTerminal := false
	for _, ts := range tc.TerminalStatuses {
		if resolution == ts {
			validTerminal = true
			break
		}
	}
	if !validTerminal {
		fmt.Fprintf(os.Stderr, "invalid resolution %q — valid: %v\n", resolution, tc.TerminalStatuses)
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

	// Gate enforcement — skip for abandon/declined since those bypass gates
	// by design. (wontfix is rejected earlier per I-433.)
	if !opts.Force && resolution != "abandoned" && resolution != "declined" {
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

		// I-514: emit all four duration fields from a single wallDur so
		// they agree to the second / rounding. Prefer started_at; fall
		// back to item.Created only when started_at is missing or
		// unparseable (back-compat for legacy items closed without one).
		var wallDur time.Duration
		var anchored bool
		if startedAt, ok := getNestedField(item, "time_tracking", "started_at"); ok && startedAt != "" {
			if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
				wallDur = now.Sub(t)
				anchored = true
			}
		}
		if !anchored && !item.Created.IsZero() {
			wallDur = now.Sub(item.Created)
			anchored = true
		}
		if anchored {
			item.SetNested("time_tracking", "total_duration_seconds",
				fmt.Sprintf("%d", int(wallDur.Seconds())))
			item.SetNested("time_tracking", "work_duration_seconds",
				fmt.Sprintf("%d", int(wallDur.Seconds())))
			item.SetNested("time_tracking", "wall_time_hours", fmt.Sprintf("%.1f", wallDur.Hours()))
			item.SetNested("time_tracking", "total_wall_time", formatDuration(wallDur))
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
	// move, and "Closed" turned out to be a lie. GitSync is best-effort —
	// a failure here only warns, because the filesystem mutation already
	// succeeded and a later sync will carry the commit forward.
	// I-442: pass the post-Move path. The Move from issues/→archive/
	// (or tasks/→archive/) is a rename — git add -u catches the
	// delete-from-old, but the new path is untracked and needs
	// explicit staging.
	newPath, _ := s.Path(id)
	if err := s.GitSync(fmt.Sprintf("st close: %s (%s)", id, resolution), newPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after close failed: %v\n", err)
	}

	// Auto-archive sprint and epic when all items are terminal.
	autoArchiveSprintAndEpic(s, cfg, item.Sprint)

	// Auto-finish the worktree when one exists. Best-effort: never blocks
	// the close, never uses --force, prints a one-line retention warning
	// when uncommitted/unpushed work would be lost. Sibling of the queue
	// auto-remove and stack auto-pop above.
	if cleaned, _ := TryAutoFinishWorktree(cfg, id); cleaned {
		fmt.Printf("  also finished worktree\n")
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
