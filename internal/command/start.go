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
	// Force bypasses the I-490 queue-approval gate. When true, an item
	// in pending status can still be activated. After the start
	// successfully completes, a `start_force` audit entry is appended
	// to the item's changelog (best-effort — a logging miss does not
	// fail the start). The audit is written post-Mutate so a force
	// that fails a later guard (assignment, claim race) doesn't leave
	// a misleading bypass record. Force also bypasses the I-681
	// sprint-inheritance gate (audited as `start_force_sprint`).
	Force bool
	// AddToSprint resolves the I-681 sprint-inheritance gate in one
	// step: when the item blocks an active-sprint member and has no
	// sprint of its own, add it to that sprint (via SprintAdd) and
	// continue, instead of refusing the start.
	AddToSprint bool

	// I-711: AckDrift carries the operator's acknowledgement that
	// the freshness gate flagged drift but they want to proceed
	// anyway. Empty = no ack; activation refuses if the gate
	// returns Drift. There is no analogous --ack-stale opt-out;
	// Stale forces re-prep.
	AckDrift string

	// Escalate overrides the resolved model tier (can go up or down).
	// Logged to changelog as start_escalate with the original tier.
	Escalate string

	// Inline is a no-op synonym for compatibility with wrapper hooks
	// that grep for the DISPATCH line — the directive is always printed.
	Inline bool

	// PRFetch is an injectable GitHub PR-state function for testing the
	// I-876 open-PR guard in createWorktrees. nil = use getPRState.
	PRFetch func(*config.Config, string) (string, []string)
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

	// T-461: approval gate removed — queue entries are auto-approved.
	// Track the sprint-inheritance bypass for post-Mutate audit.
	forceBypassedSprint := false

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

	// I-681: mid-sprint follow-up gate. If this item blocks a member of
	// an active sprint but has no sprint of its own, we'd be working a
	// sprint's blocker off the sprint — refuse. --add-to-sprint resolves
	// it in one step; --force bypasses (audited like the I-490 bypass).
	// Ambiguous links (>1 active sprint) always reject — never auto-pick.
	//
	// Deliberately placed AFTER the read-only dependency gate: a blocked
	// item should surface its actionable "blocked by" message first, and
	// the only state-mutating branch here (--add-to-sprint → SprintAdd)
	// must not run for an item that can't start anyway. All earlier gates
	// (status, I-490, assignment, deps) are read-only, so by this point
	// the start is prerequisite-clean; if --add-to-sprint adds the item
	// and a later claim race loses, the sprint membership is still
	// correct (it belongs there regardless of who works it — that is
	// precisely the I-681 invariant), so the write is not misleading.
	if t, ambiguous := resolveSprintInheritance(s, cfg, id); t != nil {
		switch {
		case opts.AddToSprint:
			if rc := SprintAdd(s, cfg, t.SprintID, []string{id}); rc != 0 {
				return rc
			}
			fmt.Printf("added %s to active sprint %s (blocks %s)\n", id, t.SprintID, t.Via)
			if it2, ok := s.Get(id); ok {
				item = it2 // refresh: SprintAdd mutated sprint/epic
			}
		case opts.Force:
			forceBypassedSprint = true
			fmt.Fprintf(os.Stderr,
				"warning: --force bypassed I-681 sprint-inheritance gate for %s (blocks %s in active sprint %s)\n",
				id, t.Via, t.SprintID)
		default:
			fmt.Fprintf(os.Stderr,
				"%s blocks %s in active sprint %s but is not in that sprint.\n"+
					"  fix: st sprint add %s %s   (or: st start %s --add-to-sprint)\n",
				id, t.Via, t.SprintID, t.SprintID, id, id)
			return 1
		}
	} else if len(ambiguous) > 0 {
		fmt.Fprintf(os.Stderr,
			"%s blocks members of multiple active sprints %v but is in none.\n"+
				"  fix: st sprint add <sprint> %s   (pick the correct sprint)\n",
			id, ambiguous, id)
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

	// I-711: freshness gate. Runs BEFORE worktree creation so a
	// Stale verdict doesn't leave orphan filesystem state. Fresh
	// proceeds silently; Drift refuses unless --ack-drift was
	// passed; Stale refuses with a re-prep instruction. The cache
	// at <workspace>/.as/cache/freshness/ makes a same-state
	// re-start instant.
	if code := runFreshnessGate(cfg, s, id, opts); code != 0 {
		return code
	}

	// Create worktrees if configured. Hoisted out of Mutate — git/fs
	// side effects don't belong inside the lock holder.
	var branch string
	if cfg.Worktree != nil && cfg.Worktree.Enabled {
		if opts.Slug == "" {
			fmt.Fprintln(os.Stderr, "--slug is required when worktree integration is enabled")
			return 2
		}
		normalized, err := normalizeSlug(id, opts.Slug)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 2
		}
		opts.Slug = normalized
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

		// I-447: starting work on an item enters the `coding` lifecycle
		// stage. advanceDeliveryStage protects items already past coding
		// (e.g., reopened after a partial close).
		advanceDeliveryStage(item, "coding")

		// I-830: backfill scope_class from goal tags for items that were
		// queued before auto-set was in place (idempotent — skips if already set).
		if item.ScopeClass == "" {
			if cls := cfg.Testing.ScopeClassForGoalTags(item.Tags); cls != "" {
				item.ScopeClass = cls
				item.Doc.SetField("scope_class", cls)
			}
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
		// I-1318: if session_started_at is already set (e.g. prior session crashed
		// before the stop-hook could flush it), accumulate the elapsed time before
		// resetting the timer epoch so the prior segment is not silently lost.
		if prior, _ := getNestedField(item, "time_tracking", "session_started_at"); prior != "" {
			if t0, err := time.Parse(time.RFC3339, prior); err == nil {
				elapsed := int(time.Now().Sub(t0).Seconds())
				if elapsed < 0 {
					elapsed = 0
				}
				acc := 0
				if v, _ := getNestedField(item, "time_tracking", "accumulated_seconds"); v != "" {
					fmt.Sscanf(v, "%d", &acc) //nolint:errcheck
				}
				item.SetNested("time_tracking", "accumulated_seconds", fmt.Sprintf("%d", acc+elapsed))
			}
		}
		item.SetNested("time_tracking", "session_started_at", now)
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

	// I-681: record a --force bypass of the sprint-inheritance gate, only
	// after the start succeeded. Distinct op so operators can tell a
	// deliberate off-sprint start from an I-490 queue-approval bypass.
	if forceBypassedSprint {
		_ = changelog.Append(cfg, id, changelog.Entry{
			Op:     "start_force_sprint",
			Reason: "bypassed I-681 mid-sprint follow-up sprint-inheritance gate via --force",
		})
	}
	// I-711: record a --ack-drift acknowledgement post-Mutate. The
	// freshness gate already printed the findings + ack to stderr;
	// this is the audit-trail counterpart, parallel to the three
	// bypass logs above. The note carries the operator-supplied
	// reason so future readers can correlate the bypass with
	// whatever context the operator cared about at activation.
	if opts.AckDrift != "" {
		_ = changelog.Append(cfg, id, changelog.Entry{
			Op:     "start_ack_drift",
			Reason: "bypassed I-711 freshness drift gate via --ack-drift: " + opts.AckDrift,
		})
	}

	fmt.Printf("Started %s — %s\n", id, item.Title)
	if agentID != "" {
		fmt.Printf("  Assigned to: %s\n", agentID)
	}
	if item.Sprint != "" {
		fmt.Printf("  sprint %s siblings discoverable via st next\n", item.Sprint)
	}

	// Emit dispatch directive so the operator knows which model tier to
	// launch. decideTier follows the model_tier → model_tier_rec → Haiku
	// API → sonnet-fallback chain without a network call when the tier
	// is already stamped on the item by plan prep/approve (T-425).
	tierResult := decideTier(s, cfg, ModelRecOpts{ItemID: id})
	dispatchTier := tierResult.Tier
	if opts.Escalate != "" {
		if _, valid := validTiers[opts.Escalate]; valid {
			_ = changelog.Append(cfg, id, changelog.Entry{
				Op: "start_escalate", OldValue: dispatchTier, NewValue: opts.Escalate,
			})
			dispatchTier = opts.Escalate
		} else {
			fmt.Fprintf(os.Stderr, "warning: --escalate %q is not a valid tier (haiku|sonnet|opus) — using resolved tier %s\n", opts.Escalate, dispatchTier)
		}
	}
	fmt.Printf("DISPATCH: launch session with model=%s\n", dispatchTier)

	// I-1326: hint when item has no goal link — it won't appear in goal-weighted ranking.
	// Skip for goal-type items: goals don't have parent goals.
	if item.Type != "goal" && len(item.Goals) == 0 {
		allGoals := s.List(store.TypeFilter("goal"))
		var goalHints []string
		for _, g := range allGoals {
			if g.Status == "active" {
				goalHints = append(goalHints, fmt.Sprintf("%s (%s)", g.ID, g.Title))
			}
		}
		fmt.Fprintf(os.Stderr, "hint: %s has no goal link — it won't appear in goal-weighted ranking\n", id)
		if len(goalHints) > 0 {
			fmt.Fprintf(os.Stderr, "  add one: st item goals add %s <goal-id>\n", id)
			fmt.Fprintf(os.Stderr, "  active goals: %s\n", strings.Join(goalHints, ", "))
		}
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
			// Auto-push from `st start` is internal; the I-490 gate already
			// fired earlier in Start, so it's safe to bypass here. When the
			// I-681 sprint gate was --force-bypassed, suppress the push-side
			// auto-inherit too so the bypass isn't silently undone.
			if rc := StackPush(s, cfg, id, StackPushOpts{
				FromPending:       true,
				SkipSprintInherit: forceBypassedSprint,
			}); rc != 0 {
				fmt.Fprintf(os.Stderr, "warning: auto-push failed (rc=%d); run `st push %s` to attribute metrics\n", rc, id)
			}
		}
	}
	return 0
}

// knownBranchPrefixes are the leading "<type>/" segments that
// normalizeSlug will strip from a user-supplied --slug. Kept aligned with
// the canonical prefixes emitted by createWorktrees (feat / fix) plus the
// conventional-commit types operators commonly type by hand.
var knownBranchPrefixes = []string{"feat", "fix", "chore", "refactor", "hotfix"}

// normalizeSlug strips a leading "<type>/<id>-" prefix from a user-supplied
// --slug so that calling `st start I-579 --slug fix/I-579-foo` produces the
// same canonical branch (fix/I-579-foo) as `--slug foo`. After normalization
// the slug must be a single path segment — any remaining slash is a user
// error (slashes inside the slug create the broken-directory illusion that
// motivated I-579).
func normalizeSlug(id, slug string) (string, error) {
	s := slug
	for _, p := range knownBranchPrefixes {
		if len(s) >= len(p)+1 && strings.EqualFold(s[:len(p)], p) && s[len(p)] == '/' {
			s = s[len(p)+1:]
			break
		}
	}
	idDash := id + "-"
	if len(s) >= len(idDash) && strings.EqualFold(s[:len(idDash)], idDash) {
		s = s[len(idDash):]
	}
	if s == "" {
		return "", fmt.Errorf("--slug is empty after stripping leading <type>/<id>- prefix; got %q", slug)
	}
	if strings.Contains(s, "/") {
		return "", fmt.Errorf("--slug must be a single path segment after stripping leading <type>/<id>- prefix; got %q (normalized to %q)", slug, s)
	}
	return s, nil
}

// createWorktrees creates git worktrees for the given item.
// Absorbs start-work.sh logic: pull main, create branch, worktree add,
// symlink .env files, npm install for Node repos (via
// maybeInstallNodeDeps — I-526).
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

	// I-876: refuse fresh worktree creation when an open PR already exists for
	// this branch — a parallel session has shipped this work. The agent should
	// use `st resume` to attach to the existing branch, not re-implement from
	// scratch. Degrades gracefully when gh is unavailable (guard skipped).
	prFetch := opts.PRFetch
	if prFetch == nil && toolAvailable("gh") {
		prFetch = getPRState
	}
	if prFetch != nil {
		if prState, prURLs := prFetch(cfg, branch); prState == "OPEN" {
			url := ""
			if len(prURLs) > 0 {
				url = " — " + prURLs[0]
			}
			return "", fmt.Errorf(
				"branch %q already has an open PR%s\n"+
					"  a parallel session has already pushed this work;\n"+
					"  run `st resume %s` to attach to the existing branch instead of re-implementing",
				branch, url, id,
			)
		}
	}

	// I-407: WorktreeBase resolves to <agent-root>/worktrees, not
	// <workspace>/worktrees, so each agent's worktrees are physically
	// distinct (workspace is symlinked across agents per I-418).
	baseDir := cfg.WorktreeBase()
	workDir := filepath.Join(baseDir, id)
	// I-778: RepoParent() honors worktree.parent_dir overrides (absolute
	// or relative) and otherwise resolves to AgentRoot — which anchors
	// to .as/agent-workspace.yaml under the invocation site, recovering
	// the correct per-agent root even when cfg.Root() was discovered
	// via an ST_ROOT env leak from a peer.
	parentDir := cfg.RepoParent()

	if err := os.MkdirAll(workDir, 0755); err != nil {
		return "", fmt.Errorf("creating worktree dir: %w", err)
	}

	for _, repoShort := range repos {
		if err := provisionSingleRepoWorktree(cfg, workDir, branch, repoShort, parentDir); err != nil {
			return "", err
		}
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

// binaryAutonomyEnabled reports whether the T-346 auto-approve-green
// behavior is opt-in active. Gated on ST_BINARY_AUTONOMY=1 so the
// rollout can be paused without a revert — see the plan's rollback
// section.
func binaryAutonomyEnabled() bool {
	v := strings.TrimSpace(os.Getenv("ST_BINARY_AUTONOMY"))
	return v == "1" || strings.EqualFold(v, "true")
}

// classifierGreen reports whether the item's persisted classifier
// verdict is green. T-345 writes verdict under classification.verdict;
// missing/empty verdict counts as not-green so an unclassified item
// stays on the normal approval path.
func classifierGreen(item *model.Item) bool {
	if item == nil || item.Doc == nil {
		return false
	}
	v, _ := item.Doc.GetNestedField("classification.verdict")
	return v == "green"
}

// installerFunc is the underlying installer that runs `make install`
// or `npm ci` inside wtPath. Routed through a package-level variable
// so unit tests can inject a fake and assert call behavior without
// shelling out. I-526.
type installerFunc func(wtPath string) error

// defaultInstaller is the production implementation: prefer
// `make install` when a Makefile is present (matches project
// convention and uses --legacy-peer-deps), fall back to
// `npm ci --legacy-peer-deps`.
func defaultInstaller(wtPath string) error {
	if _, err := os.Stat(filepath.Join(wtPath, "Makefile")); err == nil {
		cmd := exec.Command("make", "install")
		cmd.Dir = wtPath
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := exec.Command("npm", "ci", "--legacy-peer-deps")
	cmd.Dir = wtPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// nodeInstaller is the package-level installer. Tests overwrite this
// in a t.Cleanup-restored swap.
var nodeInstaller installerFunc = defaultInstaller

// maybeInstallNodeDeps installs node dependencies in wtPath when the
// worktree looks like a Node project (package.json present) and the
// dependency tree isn't already in place. No-op for non-Node repos
// (theraprac-api, theraprac-infra) and for repos whose node_modules
// already exists. I-526.
func maybeInstallNodeDeps(wtPath string) {
	if _, err := os.Stat(filepath.Join(wtPath, "package.json")); err != nil {
		return // not a Node project
	}
	if _, err := os.Stat(filepath.Join(wtPath, "node_modules")); err == nil {
		return // already installed
	}
	fmt.Printf("  %s: installing node_modules...\n", filepath.Base(wtPath))
	if err := nodeInstaller(wtPath); err != nil {
		// Non-fatal: we'd rather hand the operator a partially-set-up
		// worktree than abort the whole `st start` because npm is being
		// flaky. Subsequent `make` invocations will surface the same
		// error in a more discoverable place.
		fmt.Printf("  %s: install failed (%v) — run `cd %s && make install` manually\n",
			filepath.Base(wtPath), err, wtPath)
	}
}

// provisionSingleRepoWorktree fetches, creates, env-wires, and node-installs
// one repo's worktree under workDir on the given branch. Shared by
// createWorktrees (initial start) and WorktreeAdd (add-after-start).
func provisionSingleRepoWorktree(cfg *config.Config, workDir, branch, repoShort, parentDir string) error {
	wt := cfg.Worktree
	repoDir := wt.RepoMap[repoShort]
	if repoDir == "" {
		repoDir = repoShort
	}

	mainRepoPath := filepath.Join(parentDir, repoDir)
	wtPath := filepath.Join(workDir, repoDir)

	if _, err := os.Stat(wtPath); err == nil {
		fmt.Printf("  %s: worktree exists, skipping\n", repoDir)
		return nil
	}

	if _, err := os.Stat(filepath.Join(mainRepoPath, ".git")); err != nil {
		return fmt.Errorf("%s is not a git repo at %s", repoDir, mainRepoPath)
	}

	fmt.Printf("  %s: fetching origin main...\n", repoDir)
	fetchErr := gitRun(mainRepoPath, "fetch", "origin", "main")
	if fetchErr != nil {
		fmt.Printf("  %s: fetch skipped (%v)\n", repoDir, fetchErr)
	}

	if fetchErr == nil {
		fmt.Printf("  %s: fast-forwarding main...\n", repoDir)
		if err := gitRun(mainRepoPath, "merge", "--ff-only", "origin/main"); err != nil {
			fmt.Printf("  %s: fast-forward skipped (%v)\n", repoDir, err)
		}
	}

	fmt.Printf("  %s: creating worktree at %s\n", repoDir, wtPath)
	if branchExists(mainRepoPath, branch) {
		if err := gitRun(mainRepoPath, "worktree", "add", wtPath, branch); err != nil {
			return fmt.Errorf("worktree add %s (existing branch): %w", repoDir, err)
		}
	} else if remoteBranchExists(mainRepoPath, branch) {
		if err := gitRun(mainRepoPath, "worktree", "add", wtPath, "-b", branch, "origin/"+branch); err != nil {
			return fmt.Errorf("worktree add %s (remote branch): %w", repoDir, err)
		}
	} else {
		if fetchErr == nil {
			if err := gitRun(mainRepoPath, "worktree", "add", wtPath, "-b", branch, "origin/main"); err != nil {
				return fmt.Errorf("worktree add %s (new branch): %w", repoDir, err)
			}
		} else {
			if err := gitRun(mainRepoPath, "worktree", "add", wtPath, "-b", branch); err != nil {
				return fmt.Errorf("worktree add %s (new branch): %w", repoDir, err)
			}
		}
	}

	symlinkEnvFiles(mainRepoPath, wtPath)
	maybeInstallNodeDeps(wtPath)
	return nil
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
