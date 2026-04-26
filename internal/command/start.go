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
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// StartOpts holds flags for the start command.
type StartOpts struct {
	Slug  string   // branch slug (e.g. "uat-database-reset")
	Repos []string // repos to create worktrees for (overrides config defaults)
}

func Start(s *store.Store, cfg *config.Config, id string, opts StartOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// Check: must be in start status
	tc, ok := cfg.Types[item.Type]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", item.Type)
		return 1
	}
	if item.Status != tc.StartStatus {
		fmt.Fprintf(os.Stderr, "%s is %s, not %s — cannot start\n", id, item.Status, tc.StartStatus)
		return 1
	}

	// Check: not assigned to another agent
	identity := cfg.Identity()
	agentID := identity.ID
	if item.AssignedTo != "" && item.AssignedTo != agentID {
		fmt.Fprintf(os.Stderr, "%s is assigned to %s — use `as release %s` first\n", id, item.AssignedTo, id)
		return 1
	}

	// Check: dependencies resolved
	g := deps.Build(s.All(), cfg)
	if g.IsBlocked(id) {
		unresolved := g.UnresolvedDeps(id)
		fmt.Fprintf(os.Stderr, "%s is blocked by: %v\n", id, unresolved)
		return 1
	}

	// Check: not already claimed by another live session
	sessionID := cfg.SessionID()
	if item.ClaimedBy != "" && item.ClaimedBy != sessionID {
		// Check if the claiming session is stale
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		claimingSession, _ := mgr.Load(item.ClaimedBy)
		if claimingSession != nil && !mgr.IsStale(claimingSession) {
			fmt.Fprintf(os.Stderr, "%s is claimed by session %s (since %s)\n", id, item.ClaimedBy, item.ClaimedAt)
			fmt.Fprintln(os.Stderr, "use `st release` to clear the claim, or wait for the session to expire")
			return 1
		}
		// Stale or dead session — clear the old claim
		fmt.Printf("  Releasing stale claim from session %s\n", item.ClaimedBy)
		_ = mgr.RemoveClaim(item.ClaimedBy, id)
	}

	// Ensure git hooks are active on all configured repos
	ensureHooksPath(cfg)
	AgentAutoAuth(cfg, agentID)

	// Create worktrees if configured. Hoisted out of Mutate — git/fs
	// side effects don't belong inside the lock holder.
	var branch string
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		if opts.Slug == "" {
			fmt.Fprintln(os.Stderr, "--slug is required when worktree integration is enabled")
			return 2
		}
		var err error
		branch, err = createWorktrees(cfg, id, item.Type, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "creating worktrees: %v\n", err)
			return 1
		}
	}

	now := time.Now().Format(time.RFC3339)

	if err := s.Mutate(id, func(item *model.Item) error {
		if branch != "" {
			item.SetNested("work_tracking", "branch", branch)
		}

		item.Doc.SetField("status", tc.ActiveStatus)
		item.Status = tc.ActiveStatus

		item.Doc.SetField("last_touched", now)
		if agentID != "" {
			item.Doc.SetField("last_touched_by", agentID)
		} else {
			item.Doc.SetField("last_touched_by", "user")
		}

		if agentID != "" {
			item.Doc.SetField("assigned_to", agentID)
			item.AssignedTo = agentID
		}

		// Stamp parent/root heritage when this run inherited identity
		// from a spawning agent. Only emit fields that are populated so
		// non-heritage starts don't accumulate empty meta blocks.
		if identity.ParentID != "" {
			item.Doc.SetNestedField("assigned_to_meta.parent_id", identity.ParentID)
			if identity.RootID != "" && identity.RootID != identity.ParentID {
				item.Doc.SetNestedField("assigned_to_meta.root_id", identity.RootID)
			}
			if identity.Role != "" {
				item.Doc.SetNestedField("assigned_to_meta.role", identity.Role)
			}
			if identity.SpawnedBySession != "" {
				item.Doc.SetNestedField("assigned_to_meta.spawned_by", identity.SpawnedBySession)
			}
			if identity.DelegatedItemID != "" {
				item.Doc.SetNestedField("assigned_to_meta.delegated_item", identity.DelegatedItemID)
			}
		}

		if sessionID != "" {
			item.ClaimedBy = sessionID
			item.ClaimedAt = now
			item.Doc.SetField("claimed_by", sessionID)
			item.Doc.SetField("claimed_at", now)

			item.Sessions = append(item.Sessions, sessionID)
			updateListInDoc(item, "sessions", item.Sessions)
		}

		if item.TimeTracking == nil {
			item.TimeTracking = make(map[string]interface{})
		}
		item.TimeTracking["started_at"] = now
		item.SetNested("time_tracking", "started_at", now)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	// Record session claim only after the item file write succeeds. If
	// the Mutate failed (lock timeout, etc.), the session manager would
	// otherwise hold a phantom claim against an item that never started.
	if sessionID != "" {
		mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
		spec := session.IdentitySpec{
			AgentID:          agentID,
			ParentAgentID:    identity.ParentID,
			RootAgentID:      identity.RootID,
			Role:             identity.Role,
			SpawnedBySession: identity.SpawnedBySession,
			DelegatedItemID:  identity.DelegatedItemID,
		}
		if _, err := mgr.EnsureSessionWithIdentity(sessionID, spec); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not create session: %v\n", err)
		}
		if err := mgr.AddClaim(sessionID, id); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record claim: %v\n", err)
		}
	}

	// Lock the item file so GitPull can't overwrite it
	if err := store.LockItem(cfg, id, sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not lock item: %v\n", err)
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "start", Field: "status",
		OldValue: tc.StartStatus, NewValue: tc.ActiveStatus,
	})

	fmt.Printf("Started %s — %s\n", id, item.Title)
	if agentID != "" {
		fmt.Printf("  Assigned to: %s\n", agentID)
	}
	return 0
}

// createWorktrees creates git worktrees for the given item.
// Absorbs start-work.sh logic: pull main, create branch, worktree add,
// symlink .env files, npm install for web repos.
func createWorktrees(cfg *config.Config, id, itemType string, opts StartOpts) (string, error) {
	wt := cfg.Worktree
	repos := opts.Repos
	if len(repos) == 0 {
		repos = wt.Repos
	}
	if len(repos) == 0 {
		return "", fmt.Errorf("no repos configured for worktree creation")
	}

	// Branch naming: feat/T-xxx-slug for tasks, fix/I-xxx-slug for issues
	prefix := "feat"
	if strings.HasPrefix(id, "I-") {
		prefix = "fix"
	}
	branch := fmt.Sprintf("%s/%s-%s", prefix, id, opts.Slug)

	baseDir := filepath.Join(cfg.Root(), wt.BaseDir)
	workDir := filepath.Join(baseDir, id)
	parentDir := wt.ParentDir
	if parentDir == "" {
		parentDir = cfg.Root()
	}
	if !filepath.IsAbs(parentDir) {
		parentDir = filepath.Join(cfg.Root(), parentDir)
	}

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return "", fmt.Errorf("creating worktree dir: %w", err)
	}

	for _, repoShort := range repos {
		repoDir := wt.RepoMap[repoShort]
		if repoDir == "" {
			repoDir = repoShort // fallback: use short name as dir name
		}

		mainRepoPath := filepath.Join(parentDir, repoDir)
		wtPath := filepath.Join(workDir, repoDir)

		// Skip if already exists
		if _, err := os.Stat(wtPath); err == nil {
			fmt.Printf("  %s: worktree exists, skipping\n", repoDir)
			continue
		}

		// Verify main repo exists
		if _, err := os.Stat(filepath.Join(mainRepoPath, ".git")); err != nil {
			return "", fmt.Errorf("%s is not a git repo at %s", repoDir, mainRepoPath)
		}

		// Pull main
		fmt.Printf("  %s: pulling main...\n", repoDir)
		if err := gitRun(mainRepoPath, "pull", "--ff-only"); err != nil {
			// Non-fatal: might be on a different branch or no remote
			fmt.Printf("  %s: pull skipped (%v)\n", repoDir, err)
		}

		// Create worktree with branch
		fmt.Printf("  %s: creating worktree at %s\n", repoDir, wtPath)
		if branchExists(mainRepoPath, branch) {
			// Reuse existing branch
			if err := gitRun(mainRepoPath, "worktree", "add", wtPath, branch); err != nil {
				return "", fmt.Errorf("worktree add %s (existing branch): %w", repoDir, err)
			}
		} else if remoteBranchExists(mainRepoPath, branch) {
			// Track remote branch
			if err := gitRun(mainRepoPath, "worktree", "add", wtPath, "-b", branch, "origin/"+branch); err != nil {
				return "", fmt.Errorf("worktree add %s (remote branch): %w", repoDir, err)
			}
		} else {
			// Create new branch
			if err := gitRun(mainRepoPath, "worktree", "add", wtPath, "-b", branch); err != nil {
				return "", fmt.Errorf("worktree add %s (new branch): %w", repoDir, err)
			}
		}

		// Symlink .env files from main checkout
		symlinkEnvFiles(mainRepoPath, wtPath)
	}

	// Write .workinfo metadata
	writeWorkinfo(workDir, id, branch, repos)

	fmt.Printf("  Branch: %s\n", branch)
	fmt.Printf("  Dir:    %s\n", workDir)
	return branch, nil
}

func branchExists(repoDir, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

func remoteBranchExists(repoDir, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func symlinkEnvFiles(mainPath, wtPath string) {
	entries, err := filepath.Glob(filepath.Join(mainPath, ".env*"))
	if err != nil {
		return
	}
	for _, envFile := range entries {
		base := filepath.Base(envFile)
		if strings.HasSuffix(base, ".example") {
			continue
		}
		target := filepath.Join(wtPath, base)
		if _, err := os.Stat(target); err == nil {
			continue // already exists
		}
		os.Symlink(envFile, target)
		fmt.Printf("  symlinked %s\n", base)
	}
}

func writeWorkinfo(workDir, id, branch string, repos []string) {
	path := filepath.Join(workDir, ".workinfo")
	var b strings.Builder
	b.WriteString("# Worktree metadata — written by as start\n")
	b.WriteString(fmt.Sprintf("name: %s\n", id))
	b.WriteString(fmt.Sprintf("branch: %s\n", branch))
	b.WriteString(fmt.Sprintf("created: %s\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString("ids:\n")
	b.WriteString(fmt.Sprintf("  - %s\n", id))
	b.WriteString("repos:\n")
	for _, r := range repos {
		b.WriteString(fmt.Sprintf("  - %s\n", r))
	}
	os.WriteFile(path, []byte(b.String()), 0644)
}
