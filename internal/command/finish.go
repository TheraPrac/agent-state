package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// FinishOpts holds flags for the finish command.
type FinishOpts struct {
	DryRun  bool
	Force   bool
	ListAll bool
}

func Finish(s *store.Store, cfg *config.Config, id string, opts FinishOpts) int {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		fmt.Fprintln(os.Stderr, "worktree integration not enabled in config")
		return 1
	}

	baseDir := cfg.WorktreeBase()

	if opts.ListAll {
		// I-407: also surface orphans at the pre-fix legacy location so
		// operators can audit anything still sitting in the shared
		// workspace during the migration window.
		code := listWorktrees(baseDir)
		if legacy := cfg.WorktreeBaseLegacy(); legacy != "" && legacy != baseDir {
			if _, err := os.Stat(legacy); err == nil {
				fmt.Println()
				fmt.Println("Legacy worktrees (pre-I-407, in shared workspace):")
				if c := listWorktrees(legacy); c != 0 {
					return c
				}
			}
		}
		return code
	}

	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: as finish <id> [--dry-run] [--force] [--list]")
		return 2
	}

	wtDir := filepath.Join(baseDir, id)

	// I-407 migration: if the new agent-root location is empty but the
	// legacy <workspace>/worktrees/<id> dir exists (created before I-407
	// landed), clean it up from there. New worktrees always use the
	// new location.
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		legacy := filepath.Join(cfg.WorktreeBaseLegacy(), id)
		if _, err := os.Stat(legacy); err == nil {
			fmt.Fprintf(os.Stderr, "note: cleaning legacy worktree at %s (pre-I-407)\n", legacy)
			wtDir = legacy
		} else {
			fmt.Fprintf(os.Stderr, "worktree directory not found: %s\n", wtDir)
			return 1
		}
	}

	// Use .workinfo repo list if present (covers repos added via st worktree add);
	// fall back to the global config list.
	reposToFinish := cfg.Worktree.Repos
	if wi, err := readWorkinfo(wtDir); err == nil && len(wi.Repos) > 0 {
		reposToFinish = wi.Repos
	}

	// For each repo in the worktree
	for _, repo := range reposToFinish {
		repoDir := filepath.Join(wtDir, repo)
		if _, err := os.Stat(repoDir); os.IsNotExist(err) {
			continue
		}

		// Check for uncommitted changes
		if !opts.Force {
			out, err := gitOutputDir(repoDir, "status", "--porcelain")
			if err == nil && strings.TrimSpace(out) != "" {
				fmt.Fprintf(os.Stderr, "%s has uncommitted changes in %s\n", id, repo)
				fmt.Fprintln(os.Stderr, "use --force to remove anyway")
				return 1
			}

			// Check for unpushed commits
			out, err = gitOutputDir(repoDir, "log", "--oneline", "@{u}..HEAD")
			if err == nil && strings.TrimSpace(out) != "" {
				fmt.Fprintf(os.Stderr, "%s has unpushed commits in %s\n", id, repo)
				fmt.Fprintln(os.Stderr, "use --force to remove anyway")
				return 1
			}
		}

		if opts.DryRun {
			fmt.Printf("[dry-run] would remove worktree: %s\n", repoDir)
			continue
		}

		// Get the branch name before removing
		branch, _ := gitOutputDir(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		branch = strings.TrimSpace(branch)

		// Remove the worktree — resolve main repo via config
		mainRepoDir := resolveRepoDir(cfg, repo)
		if err := gitCmdDir(mainRepoDir, "worktree", "remove", repoDir); err != nil {
			fmt.Fprintf(os.Stderr, "removing worktree %s: %v\n", repoDir, err)
			// Try force remove
			if opts.Force {
				gitCmdDir(mainRepoDir, "worktree", "remove", "--force", repoDir)
			}
		}

		// Delete local branch (use main repo, not worktree)
		if branch != "" && branch != "main" && branch != "master" {
			gitCmdDir(mainRepoDir, "branch", "-d", branch)
		}

		// Force remove if worktree dir still exists
		if _, err := os.Stat(repoDir); err == nil {
			gitCmdDir(mainRepoDir, "worktree", "remove", "--force", repoDir)
		}

		fmt.Printf("Removed worktree: %s/%s (branch: %s)\n", id, repo, branch)
	}

	if !opts.DryRun {
		// Remove the worktree directory
		os.RemoveAll(wtDir)
		// Release item lock
		store.UnlockItem(cfg, id)

		// I-232: drop stale work_tracking fields and reset a still-active
		// item back to its type's start status. The worktree is gone, so
		// the branch field points at a dead reference; status:active on
		// an item with no worktree is the exact "stuck active" pattern
		// I-408 surfaced. Mirrors release.go's stuck-active recovery so
		// re-running `st finish` after a hand-deleted worktree leaves
		// the item in a recoverable shape. Best-effort: never blocks.
		clearStaleWorkTrackingAfterFinish(s, cfg, id)

		fmt.Printf("Finished %s — worktrees cleaned up\n", id)
	}

	return 0
}

// clearStaleWorkTrackingAfterFinish removes work_tracking.branch /
// work_tracking.worktree and resets `status: active` items back to
// their type's StartStatus. Called after a successful, non-dry-run
// worktree removal. Best-effort — failures only log. I-232.
func clearStaleWorkTrackingAfterFinish(s *store.Store, cfg *config.Config, id string) {
	if s == nil {
		return
	}
	item, ok := s.Get(id)
	if !ok {
		return
	}
	// If the item is already terminal, the close path handled the
	// fields; don't fight it.
	if cfg.IsTerminalStatus(item.Type, item.Status) {
		return
	}
	if err := s.Mutate(id, func(item *model.Item) error {
		item.Doc.RemoveNestedField("work_tracking.branch")
		item.Doc.RemoveNestedField("work_tracking.worktree")
		if tc, ok := cfg.Types[item.Type]; ok && item.Status == tc.ActiveStatus {
			item.Status = tc.StartStatus
			item.Doc.SetField("status", tc.StartStatus)
		}
		if item.AssignedTo != "" {
			item.AssignedTo = ""
			item.Doc.SetField("assigned_to", "")
		}
		if item.ClaimedBy != "" {
			item.ClaimedBy = ""
			item.ClaimedAt = ""
			item.Doc.SetField("claimed_by", "")
			item.Doc.SetField("claimed_at", "")
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: clearing stale work_tracking for %s: %v\n", id, err)
	}
}

func listWorktrees(baseDir string) int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No worktrees.")
			return 0
		}
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", baseDir, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println("No worktrees.")
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// List repos in this worktree
		subEntries, _ := os.ReadDir(filepath.Join(baseDir, name))
		var repos []string
		for _, sub := range subEntries {
			if sub.IsDir() && !strings.HasPrefix(sub.Name(), ".") {
				repos = append(repos, sub.Name())
			}
		}
		fmt.Printf("%-12s  repos: %s\n", name, strings.Join(repos, ", "))
	}

	return 0
}

func gitCmdDir(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitOutputDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// TryAutoFinishWorktree is the close.go-friendly wrapper around the
// per-id worktree cleanup. It mirrors the safety checks from Finish()
// (no uncommitted changes, no unpushed commits) but never uses --force,
// silently skips when worktree config is disabled or no worktree dir
// exists, and returns (cleaned, retained) instead of an exit code.
//
// Closed items that left a worktree behind got abandoned in
// <agent-root>/worktrees/<id>/ indefinitely (pre-I-407 they lived in
// the shared workspace); this hook closes the loop so the operator
// doesn't have to remember `st finish` after every close. Legacy
// worktrees still under the workspace are detected via WorktreeBaseLegacy.
func TryAutoFinishWorktree(cfg *config.Config, id string) (cleaned bool, retained bool) {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return false, false
	}
	wtDir := filepath.Join(cfg.WorktreeBase(), id)
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		// I-407 migration: try the pre-fix shared-workspace location
		// before giving up. Lets old worktrees auto-clean on close.
		legacy := filepath.Join(cfg.WorktreeBaseLegacy(), id)
		if _, err := os.Stat(legacy); os.IsNotExist(err) {
			return false, false
		}
		wtDir = legacy
	}

	// Use .workinfo repo list if present (covers repos added via st worktree add);
	// fall back to the global config list. Mirrors Finish() lines 71-74 — without
	// this, extra repos bypass the uncommitted-changes guard and are silently
	// deleted by os.RemoveAll below.
	reposToCheck := cfg.Worktree.Repos
	if wi, err := readWorkinfo(wtDir); err == nil && len(wi.Repos) > 0 {
		reposToCheck = wi.Repos
	}

	// Pre-check: any repo with uncommitted or unpushed work blocks the
	// auto path entirely — we never want to drop in-flight code on the
	// floor when the operator's intent was just to close the item.
	for _, repo := range reposToCheck {
		repoDir := filepath.Join(wtDir, repo)
		if _, err := os.Stat(repoDir); os.IsNotExist(err) {
			continue
		}
		if out, err := gitOutputDir(repoDir, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
			fmt.Printf("  worktree %s/%s retained — uncommitted changes; run `st finish %s --force` after handling\n", id, repo, id)
			return false, true
		}
		if out, err := gitOutputDir(repoDir, "log", "--oneline", "@{u}..HEAD"); err == nil && strings.TrimSpace(out) != "" {
			fmt.Printf("  worktree %s/%s retained — unpushed commits; run `st finish %s --force` after pushing\n", id, repo, id)
			return false, true
		}
	}

	// Clean — remove each per-repo worktree + delete its branch.
	// Mirrors finish.go's cleanup loop minus the --force fallback (auto
	// path never forces; partial failure stays as retention).
	for _, repo := range reposToCheck {
		repoDir := filepath.Join(wtDir, repo)
		if _, err := os.Stat(repoDir); os.IsNotExist(err) {
			continue
		}
		branch, _ := gitOutputDir(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		branch = strings.TrimSpace(branch)
		mainRepoDir := resolveRepoDir(cfg, repo)
		if err := gitCmdDir(mainRepoDir, "worktree", "remove", repoDir); err != nil {
			fmt.Printf("  worktree %s/%s retained — `git worktree remove` failed; run `st finish %s --force`\n", id, repo, id)
			return false, true
		}
		if branch != "" && branch != "main" && branch != "master" {
			_ = gitCmdDir(mainRepoDir, "branch", "-d", branch)
		}
	}

	// Remove the now-empty wt parent dir. Item lock is already released
	// upstream in Close() (close.go:177), so we don't unlock here.
	_ = os.RemoveAll(wtDir)

	// I-232: drop stale work_tracking fields and reset a still-active
	// item. The Close path normally reaches terminal status before this
	// runs, but TryAutoFinishWorktree can also be called from non-close
	// paths (test harnesses, future call sites), so do the cleanup
	// defensively. Best-effort: a missing store just no-ops.
	if s, err := store.New(cfg); err == nil {
		clearStaleWorkTrackingAfterFinish(s, cfg, id)
	}
	return true, false
}
