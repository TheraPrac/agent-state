package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// isSessionLive reports whether the given session id corresponds to a
// process that is still running. T-310 + T-311: the agent registry is
// the authoritative source — a session whose owning process has exited
// has no live registration. Falls back to session-manager TTL when the
// registry has no entry for the session (older sessions that predate
// T-311 wiring, or environments where Register isn't called).
func isSessionLive(cfg *config.Config, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if pid, ok := agentPIDForSession(cfg, sessionID); ok {
		return agent.IsPIDLive(pid)
	}
	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sess, _ := mgr.Load(sessionID)
	if sess == nil {
		return false
	}
	return !mgr.IsStale(sess)
}

// agentPIDForSession scans .as/agents/ for a registration whose
// session_id matches and returns its recorded PID. The agent registry
// is the source of truth for who's alive in this workspace.
func agentPIDForSession(cfg *config.Config, sessionID string) (int, bool) {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		return 0, false
	}
	for _, r := range regs {
		if r.SessionID == sessionID {
			return r.PID, true
		}
	}
	return 0, false
}

// sessionLiveProbe is the SessionLive function passed to
// store.SweepStaleClaims. It defers to isSessionLive so the sweep uses
// the same liveness contract as the in-Mutate compare-and-claim:
// agent registry first (PID-backed), then session-manager TTL fallback.
func sessionLiveProbe(cfg *config.Config) func(string) bool {
	return func(sessionID string) bool {
		return isSessionLive(cfg, sessionID)
	}
}

// primeClaimState runs the two cross-process sweeps that the new
// claim-via-Mutate flow depends on. Best-effort: errors are logged
// but do not block the calling command — a failed sweep just means
// stale claims/registrations linger until the next attempt.
//
// Sequence matters:
//  1. agent.Sweep first removes registration files for dead PIDs.
//  2. store.SweepStaleClaims then sees a clean registry and can
//     correctly mark unowned claims as releasable (with TTL fallback
//     for sessions that predate agent registration).
func primeClaimState(s *store.Store, cfg *config.Config) {
	if _, err := agent.Sweep(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent sweep: %v\n", err)
	}
	if released, err := store.SweepStaleClaims(s, cfg, sessionLiveProbe(cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: stale-claim sweep: %v\n", err)
	} else if len(released) > 0 {
		fmt.Fprintf(os.Stderr, "released stale claims on: %s\n", strings.Join(released, ", "))
	}
}

// StartOpts holds flags for the start command.
type StartOpts struct {
	Slug   string   // branch slug (e.g. "uat-database-reset")
	Repos  []string // repos to create worktrees for (overrides config defaults)
	NoPush bool     // skip the auto-push onto the work stack
}

func Start(s *store.Store, cfg *config.Config, id string, opts StartOpts) int {
	// T-310 + T-311: refresh the cross-process state before doing anything
	// claim-related. The agent registry sweep clears dead-PID entries; the
	// stale-claim sweep then releases items whose claimed_by names a
	// session whose process is gone. After sweeping, our claim attempt
	// can find a free item that the registry would have flagged as taken.
	primeClaimState(s, cfg)

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

	// T-311: register this process before claiming, so a concurrent
	// `st start` can see us as a live owner via the registry. Cleanup
	// is deferred so the registration file disappears on clean exit
	// regardless of which return path Start takes.
	_, agentCleanup, regErr := agent.Register(cfg, agent.Options{
		BaseAgentID:      identity.ID,
		ParentAgentID:    identity.ParentID,
		RootAgentID:      identity.RootID,
		Role:             identity.Role,
		SessionID:        cfg.SessionID(),
		SpawnedBySession: identity.SpawnedBySession,
		Scope:            "item:" + id,
	})
	if regErr != nil {
		fmt.Fprintf(os.Stderr, "warning: agent registration: %v\n", regErr)
	}
	defer agentCleanup()

	// Pre-flight (advisory): fast-fail when an obviously-live claim
	// belongs to someone else. The authoritative compare-and-claim
	// happens inside the Mutate below — this short-circuit just avoids
	// creating worktrees in the common "already taken" case. T-310.
	sessionID := cfg.SessionID()
	if item.ClaimedBy != "" && item.ClaimedBy != sessionID && isSessionLive(cfg, item.ClaimedBy) {
		fmt.Fprintf(os.Stderr, "%s is claimed by session %s (since %s)\n", id, item.ClaimedBy, item.ClaimedAt)
		fmt.Fprintln(os.Stderr, "use `st release` to clear the claim, or wait for the session to expire")
		return 1
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
		// Authoritative claim check inside the lock. A concurrent st start
		// that won the race (or a stale claim that was reclaimed between
		// the pre-flight and now) shows up here as a live mismatch — we
		// refuse to overwrite. T-310.
		if item.ClaimedBy != "" && item.ClaimedBy != sessionID {
			if isSessionLive(cfg, item.ClaimedBy) {
				return store.ErrAlreadyClaimed
			}
			// Dead session — record that we're reclaiming so the operator
			// understands the prior claim was stale, not deliberately lost.
			fmt.Printf("  Releasing stale claim from session %s\n", item.ClaimedBy)
		}

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
		if errors.Is(err, store.ErrAlreadyClaimed) {
			fmt.Fprintf(os.Stderr, "%s was claimed by another live process while we prepared the worktree\n", id)
			fmt.Fprintln(os.Stderr, "no agent-state changes were committed; clean up unused worktree manually if needed")
			return 1
		}
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

	// Auto-push onto the work stack so the Stop hook attributes per-turn
	// metrics to this item by default. Skip with --no-push for "set up the
	// worktree but I'm not actively driving this yet" cases. Already-on-
	// stack is treated as success since the user's intent (focus = id) is
	// already satisfied.
	if !opts.NoPush {
		entries := LoadStack(cfg)
		alreadyOnStack := false
		for _, e := range entries {
			if e.ID == id {
				alreadyOnStack = true
				break
			}
		}
		if !alreadyOnStack {
			if rc := StackPush(s, cfg, id, ""); rc != 0 {
				fmt.Fprintf(os.Stderr, "warning: auto-push failed (rc=%d); run `st push %s` to attribute metrics\n", rc, id)
			}
		}
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
