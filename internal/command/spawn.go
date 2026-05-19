package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/spawn"
	"github.com/jfinlinson/agent-state/internal/store"
)

// SpawnChildOpts holds inputs for `st spawn child <item>`.
type SpawnChildOpts struct {
	// Item is the item id the child will work on. v1 supports only
	// items the parent already claims (same-item spawn shares the
	// parent's worktree per T-312). Different-item spawn is filed as
	// I-452 follow-up.
	Item string
}

// childSuffixRE matches `<base>-<N>` agent ids that the nextSuffix
// scheme produces for child agents — used to infer "caller is already
// a child" when the env-var heritage signal is missing (path-derived
// or local-config-derived identities don't populate ParentID, so the
// id pattern is the only depth signal available).
var childSuffixRE = regexp.MustCompile(`^[A-Za-z0-9._-]+-\d+$`)

// SpawnChild materializes a child agent registration under the
// caller's identity. T-326 / T-312.
//
// Behavior:
//   - Resolves parent identity via cfg.Identity(). Refuses if no
//     identity is bound or if AS_SESSION_ID is empty (a session id
//     is required so the registration's claim guard isn't a no-op).
//   - Enforces the depth-2 cap. The caller is "already a child" when
//     EITHER Identity.ParentID is set (env-var heritage) OR the
//     caller's id matches the `<base>-<N>` suffix pattern (path or
//     local-config heritage that doesn't populate ParentID).
//   - Calls agent.Register with ParentAgentID + RootAgentID set so
//     the child's session events roll up to the root for cost
//     attribution (I-369).
//
// Output: prints `<child-id><TAB><pid>` on stdout so the caller can
// pipe into `cut` / `read` and exec a downstream subprocess with
// AS_AGENT_ID=<child-id>.
//
// IMPORTANT — registration lifetime: the registration's PID is
// os.Getpid() of THIS spawn-child invocation. The process exits
// immediately after printing, so by the time a subsequent agent.Sweep
// runs the PID is dead and the registration gets reaped. Callers must
// adopt the registration promptly via AS_AGENT_ID=<id> in their next
// command. A future enhancement would let callers pass a `--pid <N>`
// to bind the registration to an already-forked child process.
//
// V1 supports SAME-ITEM spawn only (parent's claim covers the child).
// Different-item spawn with a new worktree is filed as I-452.
func SpawnChild(s *store.Store, cfg *config.Config, opts SpawnChildOpts) int {
	if opts.Item == "" {
		fmt.Fprintln(os.Stderr, "spawn child: item id is required")
		return 2
	}

	parent := cfg.Identity()
	if parent.ID == "" {
		fmt.Fprintln(os.Stderr,
			"spawn child: no agent identity in this shell. "+
				"Set AS_AGENT_ID, run from a per-agent dir, or write "+
				".as/local-agent.yaml.")
		return 1
	}

	// Refuse without a session id. parentSession ends up in both the
	// claim guard below and the SpawnedBySession field of the new
	// registration; an empty session would silently bypass scope
	// collision detection between zero-session agents.
	parentSession := cfg.SessionID()
	if parentSession == "" {
		fmt.Fprintln(os.Stderr,
			"spawn child: no AS_SESSION_ID set. A session id is required "+
				"so the parent's claim is unambiguous.")
		return 1
	}

	// Depth-2 policy. Two signals:
	//  - ParentID set via AS_AGENT_PARENT_ID env var (explicit
	//    spawn-from-spawn).
	//  - Id pattern `<base>-<N>` (caller's identity came from a path
	//    like `theraprac-agent-a-1`, where Identity() doesn't
	//    populate ParentID but the suffix already encodes child-ness).
	if parent.ParentID != "" || childSuffixRE.MatchString(parent.ID) {
		stated := parent.ParentID
		if stated == "" {
			stated = "<inferred from id>"
		}
		fmt.Fprintf(os.Stderr,
			"spawn child: %s is already a child (parent=%s) — depth-2 cap reached. "+
				"Spawn from the root agent instead.\n",
			parent.ID, stated)
		return 1
	}

	item, ok := s.Get(opts.Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "spawn child: item %s not found\n", opts.Item)
		return 1
	}
	if item.ClaimedBy != "" && item.ClaimedBy != parentSession {
		fmt.Fprintf(os.Stderr,
			"spawn child: %s is claimed by session %s, not by parent session %s\n",
			opts.Item, item.ClaimedBy, parentSession)
		return 1
	}

	rootID := parent.RootID
	if rootID == "" {
		rootID = parent.ID
	}

	// I-326: deliberately do NOT defer the cleanup. The registration
	// must outlive this short-lived spawn-child invocation so the
	// caller's downstream subprocess can adopt the chain via
	// AS_AGENT_ID=<reg.AgentID>. agent.Sweep reclaims the file when
	// the registered PID is no longer alive — that's expected
	// turnover, not a leak. (Diverges from start.go/run.go where the
	// registration's lifecycle matches the long-running process.)
	reg, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID:      parent.ID,
		ParentAgentID:    parent.ID,
		RootAgentID:      rootID,
		Role:             "child",
		SessionID:        parentSession,
		SpawnedBySession: parentSession,
		Scope:            "item:" + opts.Item,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn child: register: %v\n", err)
		return 1
	}

	fmt.Printf("%s\t%d\n", reg.AgentID, reg.PID)
	return 0
}

// SpawnOpts holds inputs for `st spawn <item>` — the reasoning-worker
// launcher (T-360), distinct from SpawnChild's registration-only path.
type SpawnOpts struct {
	// Item is the agent-state item the worker will drive end-to-end.
	Item string
	// BudgetOverride, when > 0, LOWERS the effective cap below the
	// coordinator.yaml per-item value (the live-verify "spend $1 on a
	// throwaway item" path). It can only ever reduce the cap: a value
	// above the coordinator cap is rejected, never silently honored —
	// the autonomy boundary is retuned in coordinator.yaml, not
	// per-invocation (contract §11).
	BudgetOverride float64
	// DryRun prints the fully-resolved launch plan (binary, cwd, args,
	// budget, log path, prompt) and exits WITHOUT launching, registering,
	// or starting the item. Side-effect-free — the unit-test + operator
	// inspection surface.
	DryRun bool
}

// Spawn launches a budget-capped, JSONL-observable reasoning worker
// (`claude -p`) on an agent-state item. This is the Shape-3 §10/§13
// linchpin: a single-shot LAUNCHER only — no coordinator loop, no stall
// heuristics, no auto-dispatch (those are separate downstream items).
//
// Hard invariants (contract §11/§13, do not regress):
//   - The reasoning binary is the RESOLVED one (internal/spawn), never
//     PATH `claude` — the cmux shim hangs when invoked nested (§13 f2).
//   - --max-budget-usd is ALWAYS set, sourced from coordinator.yaml
//     (or an explicit smaller override). A worker is NEVER spawned
//     uncapped — the cap is a process-enforced circuit breaker.
//   - The worker launches DETACHED (own process group) and is never
//     waited on; `st spawn` returns immediately and never blocks the
//     caller's session.
//   - Observation is the session JSONL (§13 f3), made deterministic by
//     pinning --session-id; the redirect log is diagnostic only.
//   - Tripwire / blast-radius / plan-gate / merge-gate enforcement is
//     the existing per-worker hooks' job (§9.1) — NOT re-implemented
//     here. `st spawn` reads coordinator.yaml, never writes it (§11).
func Spawn(s *store.Store, cfg *config.Config, opts SpawnOpts) int {
	if strings.TrimSpace(opts.Item) == "" {
		fmt.Fprintln(os.Stderr, "spawn: an item id is required")
		return 2
	}

	item, ok := s.Get(opts.Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "spawn: item %s not found\n", opts.Item)
		return 1
	}

	ident := cfg.Identity()
	if ident.ID == "" {
		fmt.Fprintln(os.Stderr,
			"spawn: no agent identity in this shell. Set AS_AGENT_ID, run "+
				"from a per-agent dir, or write .as/local-agent.yaml.")
		return 1
	}

	// Depth/recursion guard (mirrors SpawnChild's T-326/T-312 cap): a
	// worker must not spawn workers. A spawned worker inherits the
	// spawning agent's AS_AGENT_ID, so without this a runaway worker
	// could fan out budget-capped workers recursively. The caller is
	// "already a spawned agent" when ParentID is set (env heritage) or
	// its id matches the `<base>-<N>` suffix (path/local-config
	// heritage that doesn't populate ParentID). Dispatch fan-out is the
	// coordinator's job (parallelism_cap K4), never a worker's.
	if ident.ParentID != "" || childSuffixRE.MatchString(ident.ID) {
		fmt.Fprintf(os.Stderr,
			"spawn: %s is itself a spawned agent — workers must not spawn "+
				"workers (budget/blast-radius). Spawn from a root agent.\n",
			ident.ID)
		return 1
	}

	// A real launch claims the item via Start(); an empty session id
	// makes that claim a no-op (ClaimedBy=="" gives zero mutual
	// exclusion — concurrent spawns would all proceed on the same
	// item). Mirror SpawnChild's guard. Dry-run claims nothing, so it
	// is exempt (keeps the inspection path usable with no session).
	if !opts.DryRun && cfg.SessionID() == "" {
		fmt.Fprintln(os.Stderr,
			"spawn: no AS_SESSION_ID set. A session id is required so the "+
				"worker's item claim is unambiguous.")
		return 1
	}

	// Budget cap — mandatory and bounded by the autonomy boundary. The
	// coordinator.yaml per-item cap is ALWAYS read (a worker is never
	// spawned if the boundary is unreadable — contract §11). An
	// explicit --budget override may only lower the cap (the
	// live-verify "spend $1 on a throwaway" path); raising it above the
	// boundary is rejected — the boundary is retuned by a deliberate
	// edit to coordinator.yaml + commit, never per-invocation (§11).
	coordCap, err := spawn.ParsePerItemBudget(spawn.CoordinatorYAMLPath(cfg.Root()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		return 1
	}
	budget := coordCap
	if opts.BudgetOverride > 0 {
		if opts.BudgetOverride > coordCap {
			fmt.Fprintf(os.Stderr,
				"spawn: --budget %g exceeds the coordinator.yaml per-item cap "+
					"$%g (the autonomy boundary). Lower --budget, or raise the "+
					"cap in coordinator.yaml + commit (contract §11).\n",
				opts.BudgetOverride, coordCap)
			return 1
		}
		budget = opts.BudgetOverride
	}

	// Resolve the reasoning binary BEFORE any side effect so a missing
	// install fails loudly having spawned/started nothing.
	bin, err := spawn.ResolveClaudeBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		return 1
	}

	// The worker runs cwd = the item's worktree base (contains every
	// repo worktree + the workspace symlink; CLAUDE.md loads via the
	// agent-root ancestor). When the item isn't started yet, reuse the
	// `st start` path to materialise it — except under --dry-run, which
	// stays strictly side-effect-free and just shows the would-be cwd.
	worktree := cfg.WorktreeForItem(opts.Item)
	if worktree == "" {
		fmt.Fprintln(os.Stderr,
			"spawn: worktree integration is disabled — a worker needs an "+
				"isolated worktree to drive an item")
		return 1
	}
	if !dirExists(worktree) {
		if opts.DryRun {
			// fall through with the would-be path; do not start.
		} else {
			slug := deriveSlug(item)
			if rc := Start(s, cfg, opts.Item, StartOpts{Slug: slug}); rc != 0 {
				fmt.Fprintf(os.Stderr,
					"spawn: could not start %s (rc=%d) — nothing spawned\n",
					opts.Item, rc)
				return rc
			}
			if it, ok := s.Get(opts.Item); ok {
				item = it
			}
			worktree = cfg.WorktreeForItem(opts.Item)
			if !dirExists(worktree) {
				fmt.Fprintf(os.Stderr,
					"spawn: worktree %s still absent after start — nothing spawned\n",
					worktree)
				return 1
			}
		}
	}

	// Reuse the canonical RFC-4122 v4 generator (same package, run.go)
	// rather than carrying a second hand-rolled UUID implementation —
	// one format definition, one place to fix (CLAUDE.md
	// reuse-don't-reinvent). Behaviourally equivalent to the prior
	// inline version; this is deduplication, not a bug fix.
	workerSID := generateSessionID()

	prompt := buildWorkerPrompt(item)
	capStr := strconv.FormatFloat(budget, 'f', -1, 64)
	args := []string{
		"-p", prompt,
		"--session-id", workerSID,
		"--permission-mode", "bypassPermissions",
		"--max-budget-usd", capStr,
		"--output-format", "stream-json",
		"--verbose",
	}

	ts := time.Now().Format("20060102-150405")
	logDir := filepath.Join(cfg.Root(), ".as", "spawn-logs")
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.log", opts.Item, ts))

	if opts.DryRun {
		fmt.Println("DRY RUN — nothing launched, registered, or started")
		fmt.Printf("item:        %s (%s)\n", item.ID, item.Type)
		fmt.Printf("binary:      %s\n", bin)
		fmt.Printf("cwd:         %s\n", worktree)
		fmt.Printf("budget(usd): %s\n", capStr)
		fmt.Printf("session-id:  %s\n", workerSID)
		fmt.Printf("log:         %s\n", logPath)
		fmt.Printf("argv:        %s %s\n", bin, redactedArgs(args))
		fmt.Println("--- worker prompt ---")
		fmt.Println(prompt)
		return 0
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "spawn: cannot create log dir %s: %v\n", logDir, err)
		return 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: cannot open log %s: %v\n", logPath, err)
		return 1
	}
	defer logFile.Close()
	// The log is created before the launch; if the launch fails there is
	// nothing in it but it would linger forever as a confusing empty
	// artifact ("did a worker run here?"). Clean it up unless we
	// successfully launched (launched flips true after c.Start()).
	launched := false
	defer func() {
		if !launched {
			_ = os.Remove(logPath)
		}
	}()

	c := exec.Command(bin, args...)
	c.Dir = worktree
	c.Stdout = logFile
	c.Stderr = logFile
	c.Stdin = nil
	// Build the worker env:
	//  - Anchor ST_ROOT to the workspace `st spawn` itself resolved.
	//    Without this the worker inherits whatever ST_ROOT leaked into
	//    this shell — which, because every agent's theraprac-workspace
	//    is a symlink to one canonical clone, can name a DIFFERENT
	//    agent's path and (via filepath.Dir) point st's per-agent
	//    worktree base at the wrong agent. cwd and ST_ROOT must agree.
	//  - Strip AWS_PROFILE / AWS_DEFAULT_PROFILE (I-586 precedent,
	//    internal/evidence/exec.go): the operator's SSO profiles expire
	//    within hours and the AWS SDK reads AWS_PROFILE then falls back
	//    to AWS_DEFAULT_PROFILE. A long-running autonomous worker that
	//    inherited a stale profile would have every AWS call (st test
	//    evidence upload, deploy-check, …) silently fail on expired
	//    creds — exactly the silent-failure demo-killer. The worker
	//    mints its own agent IAM session via agent-aws-auth.sh.
	env := withEnv(os.Environ(), "ST_ROOT", cfg.Root())
	env = withoutEnv(env, "AWS_PROFILE", "AWS_DEFAULT_PROFILE")
	c.Env = env
	// Detach into a brand-new session so the worker outlives this
	// short-lived `st spawn` invocation and has no controlling tty.
	// Setsid alone is correct and sufficient: it makes the child a new
	// session+process-group leader. Adding Setpgid here would EPERM —
	// setpgid(0,0) on a process that setsid() just made a session
	// leader fails ("operation not permitted"), which manifests as a
	// fork/exec error and spawns nothing.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawn: failed to launch worker: %v\n", err)
		return 1
	}
	launched = true
	pid := c.Process.Pid
	// Release so the Go runtime stops tracking the detached child (no
	// wait, no zombie). The process keeps running independently.
	_ = c.Process.Release()

	// The cleanup func is intentionally discarded (`_`): unlike
	// start.go/run.go where the registration's lifetime matches the
	// long-running st process, the worker registration must OUTLIVE
	// this short-lived `st spawn` invocation (it tracks the detached
	// claude PID, not st's). agent.Sweep reclaims it when that PID
	// dies — expected turnover, not a leak. (Same rationale as
	// SpawnChild's documented non-defer.)
	reg, _, regErr := agent.Register(cfg,
		workerRegisterOptions(ident, workerSID, cfg.SessionID(), opts.Item, pid))
	if regErr != nil {
		// The worker is already running and budget-capped; a failed
		// registration only costs observability, so do not kill it —
		// surface loudly and keep going.
		fmt.Fprintf(os.Stderr,
			"spawn: WARNING worker launched (pid %d) but registration failed: %v\n",
			pid, regErr)
	}

	// Durable item→worker-session link. The agent registration is
	// SWEPT the moment the worker's PID dies, so it cannot make
	// `st transcript <item>` resolve the worker post-hoc (review is a
	// first-class part of the trust surface, not just live watching).
	// A spawned `claude -p` worker never writes this item's
	// time_tracking.by_session itself — its own hooks attribute time to
	// the SPAWNING agent's active item, not the item it runs ON. So
	// seed the (sid, project_dir) line here via seedBySession (a pure
	// seed — NO turn/token credit, unlike upsertBySession), so
	// `st transcript <item>` resolves the worker forever AND the
	// worker's own later upsertBySession deltas accumulate onto the
	// same sid-keyed line rather than duplicating it.
	now := time.Now().Format(time.RFC3339)
	if err := s.Mutate(opts.Item, func(it *model.Item) error {
		seedBySession(it, workerSID, worktree, now)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr,
			"spawn: WARNING worker launched (pid %d) but could not record its "+
				"session on %s (st transcript %s won't show it post-hoc): %v\n",
			pid, opts.Item, opts.Item, err)
	}

	tag := opts.Item
	if reg != nil {
		tag = reg.AgentID
	}
	fmt.Printf("spawned worker %s on %s (pid %d, budget $%s)\n", tag, opts.Item, pid, capStr)
	fmt.Printf("  session: %s\n", workerSID)
	fmt.Printf("  log:     %s\n", logPath)
	fmt.Printf("  observe: st watch   |   st transcript %s\n", opts.Item)
	return 0
}

// workerRegisterOptions maps the spawn inputs onto agent.Options for a
// reasoning worker (Role="worker"), pinning SessionID to the UUID we
// passed `claude --session-id` so the registration ⋈ JSONL join is
// deterministic at spawn time (§13 f3). Pure + table-testable so the
// "registration" acceptance criterion doesn't require exec'ing claude.
func workerRegisterOptions(ident config.Identity, workerSID, spawnedBy, item string, pid int) agent.Options {
	rootID := ident.RootID
	if rootID == "" {
		rootID = ident.ID
	}
	return agent.Options{
		BaseAgentID:      ident.ID,
		ParentAgentID:    ident.ID,
		RootAgentID:      rootID,
		Role:             "worker",
		SessionID:        workerSID,
		SpawnedBySession: spawnedBy,
		Scope:            "item:" + item,
		PID:              pid,
	}
}

// withEnv returns environ with key set to val — replacing an existing
// key=... entry in place (last-wins is also safe with exec, but an
// in-place replace keeps the slice clean and the intent obvious).
func withEnv(environ []string, key, val string) []string {
	out := make([]string, 0, len(environ)+1)
	prefix := key + "="
	replaced := false
	for _, e := range environ {
		if strings.HasPrefix(e, prefix) {
			out = append(out, prefix+val)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

// withoutEnv returns environ with every `key=...` entry for any of keys
// removed. Used to strip the operator's expiring AWS SSO profile vars
// from an autonomous worker's environment (I-586 precedent).
func withoutEnv(environ []string, keys ...string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if i := strings.IndexByte(e, '='); i >= 0 && drop[e[:i]] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// slugSanitizeRE keeps slugs to a single safe path segment.
var slugSanitizeRE = regexp.MustCompile(`[^a-z0-9]+`)

// deriveSlug builds a branch slug from the item title (lowercased,
// non-alphanumerics collapsed to single dashes, trimmed, capped).
// Falls back to the item id when the title yields nothing usable.
func deriveSlug(item *model.Item) string {
	s := slugSanitizeRE.ReplaceAllString(strings.ToLower(item.Title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = strings.ToLower(item.ID)
	}
	return s
}

// buildWorkerPrompt renders the spawned-worker brief: the operating
// frame (drive to merged+live per CLAUDE.md, the coordinator.yaml
// boundary governs, escalate per §7 rather than exceed it) followed by
// the item's own SBAR + acceptance criteria so the worker has the full
// task context without a round trip.
func buildWorkerPrompt(item *model.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a spawned reasoning worker for %s: %s.\n\n", item.ID, item.Title)
	b.WriteString(
		"Drive this item to merged + live-verified per CLAUDE.md's delivery " +
			"loop (plan → approve → code → self-review → test → PR → " +
			"/code-review → resolve every finding → UAT → merge → deploy → " +
			"monitor → close). Work ONLY this item; do not expand scope.\n\n")
	b.WriteString(
		"The autonomy boundary in theraprac-workspace/.as/coordinator.yaml " +
			"governs you. When an action would cross it — a tripwire, a " +
			"blast-radius escalation, the respawn/budget limits, or a genuine " +
			"blocker — STOP and escalate per operating-contract §7: file a " +
			"tracked, dependency-linked issue and leave clean, recoverable " +
			"state. Never use --no-verify; never exceed the boundary to " +
			"\"finish\".\n\n")

	fmt.Fprintf(&b, "--- ITEM %s (%s) ---\n", item.ID, item.Type)
	fmt.Fprintf(&b, "Title: %s\n", item.Title)

	sbar := item.SBAR
	if !sbar.IsEmpty() {
		b.WriteString("\nSBAR\n")
		if s := strings.TrimSpace(sbar.Situation); s != "" {
			fmt.Fprintf(&b, "Situation: %s\n", s)
		}
		if s := strings.TrimSpace(sbar.Background); s != "" {
			fmt.Fprintf(&b, "Background: %s\n", s)
		}
		if s := strings.TrimSpace(sbar.Assessment); s != "" {
			fmt.Fprintf(&b, "Assessment: %s\n", s)
		}
		if s := strings.TrimSpace(sbar.Recommendation); s != "" {
			fmt.Fprintf(&b, "Recommendation: %s\n", s)
		}
	}

	if len(item.AcceptanceCriteria) > 0 {
		b.WriteString("\nAcceptance criteria:\n")
		for _, ac := range item.AcceptanceCriteria {
			if ac = strings.TrimSpace(ac); ac != "" {
				fmt.Fprintf(&b, "- %s\n", ac)
			}
		}
	}
	return b.String()
}

// redactedArgs renders argv for the dry-run view, collapsing the long
// prompt to a placeholder so the line stays scannable (the full prompt
// is printed separately).
func redactedArgs(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-p" {
			out[i+1] = fmt.Sprintf("<prompt %d bytes>", len(out[i+1]))
		}
	}
	return strings.Join(out, " ")
}

