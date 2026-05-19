package command

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// `st coordinate` (T-363): the Shape-3 coordinator loop. This file is the
// IMPERATIVE SHELL — it wires the pure decision core (internal/coordinator)
// to the real side effects (command.Spawn, the queue, escalation). The
// brain (boundary parse, B1/C2/D2, dedupe, dispatch eligibility) is in
// internal/coordinator and is unit-tested without exec'ing a worker; this
// file is deliberately thin and is exercised by --dry-run + live-verify
// (contract §13: prove the logic cheaply, prove the wiring live).

// CoordinateOpts are the `st coordinate` flags.
type CoordinateOpts struct {
	// Once: a single pick→spawn→supervise→(escalate|advance) pass, then
	// return. The test/inspection surface.
	Once bool
	// MaxItems caps how many items the loop will process before returning.
	// 0 ⇒ unbounded (a long-running coordinator). Default 1 (safe).
	MaxItems int
	// DryRun: resolve the boundary + pick the next item + print the
	// would-be spawn plan, then exit. Launches/registers/escalates
	// NOTHING (mirrors `st spawn --dry-run`; contract §11 read-only).
	DryRun bool
	// BudgetOverride lowers the per-item cap for the picked worker (the
	// live-verify "$1 throwaway" path). Forwarded verbatim to
	// command.Spawn, which rejects a value ABOVE the coordinator cap.
	BudgetOverride float64
	// PollInterval is the base supervision cadence. <=0 ⇒ 20s. Backs off
	// geometrically on no-change up to PollMaxIdle.
	PollInterval time.Duration
	PollMaxIdle  time.Duration // <=0 ⇒ 5m
}

// Coordinate runs the loop. Returns a process exit code.
func Coordinate(s *store.Store, cfg *config.Config, opts CoordinateOpts) int {
	bPath := coordinator.CoordinatorYAMLPath(cfg.Root())
	b, err := coordinator.LoadBoundary(bPath)
	if err != nil {
		// An unreadable boundary means the loop has no autonomy limits —
		// refuse to run (contract §11: never unbounded). Loud, not silent.
		fmt.Fprintf(os.Stderr, "coordinate: %v\n", err)
		return 1
	}

	maxItems := opts.MaxItems
	if opts.Once {
		maxItems = 1
	} else if maxItems <= 0 {
		maxItems = 1 // safe default: explicit --max-items 0 (unbounded) only via flag
	}

	base := opts.PollInterval
	if base <= 0 {
		base = 20 * time.Second
	}
	idleCap := opts.PollMaxIdle
	if idleCap <= 0 {
		idleCap = 5 * time.Minute
	}
	if idleCap < base {
		idleCap = base
	}

	dd := coordinator.NewDeduper()
	ex := &cmdEscalator{cfg: cfg, boundary: b}

	processed := 0
	for opts.MaxItems == 0 || processed < maxItems {
		item, why := selectNext(s, cfg)
		if item == nil {
			fmt.Printf("coordinate: no eligible item (%s)\n", why)
			return 0
		}

		if opts.DryRun {
			fmt.Println("DRY RUN — nothing launched, supervised, or escalated")
			fmt.Printf("boundary:    %s\n", bPath)
			fmt.Printf("  respawn_limit=%d per_item=$%g per_objective=$%g stuck_x=%g parallelism=%d dedupe=%dm\n",
				b.RespawnLimit, b.PerItemUSD, b.PerObjectiveUSD, b.StuckMultiplier, b.ParallelismCap, b.DedupeWindowMin)
			fmt.Printf("picked:      %s — %s\n", item.ID, item.Title)
			fmt.Printf("why:         %s\n", why)
			fmt.Printf("size-class:  %s (D2 baseline; stuck at ≥ %g×)\n",
				coordinator.SizeClassBaseline(item), b.StuckMultiplier)
			fmt.Println("next:        st spawn " + item.ID + spawnBudgetSuffix(opts.BudgetOverride))
			return 0
		}

		fmt.Printf("coordinate: dispatch rationale — %s\n", why)
		st := &coordinator.WorkerState{
			Item:      item.ID,
			SizeClass: coordinator.SizeClassBaseline(item),
		}
		rc := superviseItem(s, cfg, b, dd, ex, st, item.ID, opts, base, idleCap)
		if rc != 0 {
			return rc
		}
		processed++
		if opts.Once {
			break
		}
	}
	return 0
}

// selectNext mirrors `st queue next` (approved + unblocked + non-terminal)
// and adds the coordinator guards via coordinator.EligibleForDispatch. It
// returns the first eligible item, or (nil, reason) naming why nothing
// qualified (never an opaque empty — that blindness is what §1 removes).
func selectNext(s *store.Store, cfg *config.Config) (*model.Item, string) {
	entries := LoadQueue(cfg)
	g := deps.Build(s.All(), cfg)
	skipped := 0
	for _, e := range entries {
		it, ok := s.Get(e.ID)
		if !ok {
			skipped++
			continue
		}
		terminal := cfg.IsTerminalStatus(it.Type, it.Status)
		ok2, _ := coordinator.EligibleForDispatch(it, e.Approved, g.IsBlocked(e.ID), terminal)
		if !ok2 {
			skipped++
			continue
		}
		// HIT: queue order still decides the pick (operator authority is
		// load-bearing) — but the choice is no longer opaque. Return the
		// inspectable scoring rationale for THIS item (contract §4.2).
		return it, dispatchRationale(s, cfg, g, it)
	}
	return nil, fmt.Sprintf("%d queue entr(y/ies) examined, none approved+unblocked+unclaimed", skipped)
}

// dispatchRationale renders the inspectable "why this pick" for the
// coordinator's dispatch decision (contract §4.2 — never an opaque
// choice). It scores the SAME eligible-queue candidate set the
// `st recommend --queue` view uses, so the operator and the coordinator
// read an identical rationale. Operator queue order still wins: this
// only EXPLAINS selectNext's queue-order pick; if the score would rank a
// different eligible item first, that divergence is surfaced as a
// visible note, never silently acted on.
func dispatchRationale(s *store.Store, cfg *config.Config, g *deps.Graph, picked *model.Item) string {
	p := 2
	if picked.Priority != nil {
		p = *picked.Priority
	}
	fallback := fmt.Sprintf("priority p%d", p)

	sprints := loadSprintInfo(cfg, g)
	cands := recommendCandidates(s, cfg, g, RecommendOpts{Queue: true}, sprints)
	lev, names := unblockLeverage(g, cands)
	recs := coordinator.Recommend(cands, lev, sprints, time.Now())
	enrichUnblockDetail(recs, names)
	if len(recs) == 0 {
		return fallback // unreachable in practice (picked is eligible) — degrade safely
	}

	rat := fallback
	for _, r := range recs {
		if r.Item.ID == picked.ID {
			rat = r.Rationale()
			break
		}
	}
	if recs[0].Item.ID != picked.ID {
		rat += fmt.Sprintf(
			" | st recommend ranks %s higher — operator queue order honoured",
			recs[0].Item.ID)
	}
	return rat
}

func spawnBudgetSuffix(b float64) string {
	if b > 0 {
		return fmt.Sprintf(" --budget %g", b)
	}
	return ""
}

// startupGrace is how long after a (re)spawn the loop tolerates "no live
// registration yet" as still-starting rather than terminated — the worker
// registration is written a beat after the detached process forks
// (command.Spawn registers post-Start). Without this the first poll would
// misread startup as an instant termination and thrash a respawn.
const startupGrace = 90 * time.Second

// superviseItem spawns ONE budget-capped worker on itemID and supervises
// it via the substrate to a terminal decision, applying the in-scope
// levers (respawn-with-context up to respawn_limit) and escalating per §7.
// Returns a process exit code: 0 on any clean stop (done OR escalated —
// an escalation is the loop doing its job, not an error), non-zero only
// on an operational failure (spawn could not launch at all).
func superviseItem(s *store.Store, cfg *config.Config, b *coordinator.Boundary,
	dd *coordinator.Deduper, ex coordinator.Escalator, st *coordinator.WorkerState,
	itemID string, opts CoordinateOpts, base, idleCap time.Duration) int {

	extraCtx := "" // empty on the first attempt; set on respawn-with-context
	for {
		// --- (re)spawn ---
		baseItem := freshItem(cfg, itemID)
		if baseItem == nil {
			fmt.Fprintf(os.Stderr, "coordinate: %s vanished from the store before spawn\n", itemID)
			return 1
		}
		if rc := Spawn(s, cfg, SpawnOpts{
			Item:           itemID,
			BudgetOverride: opts.BudgetOverride,
			ExtraContext:   extraCtx,
		}); rc != 0 {
			fmt.Fprintf(os.Stderr, "coordinate: spawn %s failed (rc=%d) — nothing supervised\n", itemID, rc)
			return rc
		}
		// Per-attempt progress baseline is captured AFTER Spawn returns:
		// command.Spawn runs `st start` on the first attempt, which writes
		// start/claim changelog entries. Sampling before Spawn would fold
		// st-start's own bookkeeping into attempt-1 "progress" and weaken
		// B1's "no monotonic progress" precision. Post-spawn the worker
		// has only just launched, so this reflects WORKER activity only
		// (code-review finding 3).
		postItem := freshItem(cfg, itemID)
		if postItem == nil {
			postItem = baseItem // store momentarily unreadable; degrade, don't crash
		}
		st.BeginAttempt(time.Now(), coordinator.SampleProgress(cfg, postItem).ChangelogLen)
		fmt.Printf("coordinate: spawned worker on %s (attempt %d, size-class %s, observe: st watch | st transcript %s)\n",
			itemID, st.RespawnCount+1, st.SizeClass, itemID)

		// --- supervise this attempt ---
		interval := base
		seenAlive := false
		var lastSig string
	superviseLoop:
		for {
			time.Sleep(interval)
			// Reap any exited worker FIRST so PID-liveness is truthful.
			// command.Spawn forks the worker then Process.Release()s it;
			// the short-lived `st spawn` path reparents to init (reaped
			// fine), but the long-running coordinator stays the worker's
			// parent and must wait() it itself — otherwise an exited worker
			// lingers as a <defunct> ZOMBIE that still answers signal-0, so
			// agent.IsPIDLive() (and thus the loop) would misread a cleanly
			// terminated worker as "still alive" and eventually misfire C2
			// "wedged". Surfaced by T-363 live-verify; unit tests over
			// synthetic snapshots cannot see an OS reaping interaction.
			reapDeadChildren()
			it := freshItem(cfg, itemID)
			if it == nil {
				fmt.Fprintf(os.Stderr, "coordinate: %s vanished mid-supervision\n", itemID)
				return 1
			}
			snap := coordinator.SampleProgress(cfg, it)
			st.Snaps = append(st.Snaps, snap)
			if snap.LastErrSig != "" {
				lastSig = snap.LastErrSig
			}
			if snap.PIDAlive {
				seenAlive = true
			}

			// Startup grace: tolerate "not yet registered" right after a
			// (re)spawn so we don't misread launch latency as termination.
			if !snap.PIDAlive && !seenAlive &&
				time.Since(st.SpawnedAt) < startupGrace {
				continue
			}

			completed := cfg.IsTerminalStatus(it.Type, it.Status)
			dec := coordinator.Decide(st, st.Snaps, b, completed, time.Now())

			switch dec.Action {
			case coordinator.ActionContinue:
				// Geometric backoff while nothing is changing (mirrors
				// st watch): cheap supervision, bounded by D2/budget.
				if len(st.Snaps) >= 2 && !progressed(st.Snaps[len(st.Snaps)-2], snap) {
					interval *= 2
					if interval > idleCap {
						interval = idleCap
					}
				} else {
					interval = base
				}

			case coordinator.ActionTerminalDone:
				fmt.Printf("coordinate: %s reached terminal status %q — worker done, advancing\n",
					itemID, it.Status)
				return 0

			case coordinator.ActionRespawn:
				killWorker(cfg, itemID)
				st.RecordRespawn(lastSig)
				extraCtx = fmt.Sprintf(
					"Your prior attempt (#%d) on %s terminated WITHOUT completing the "+
						"item and with no item-level progress. Last error signature: %s. "+
						"Do not repeat the same approach blindly — diagnose why it failed "+
						"first. The coordinator.yaml autonomy boundary still governs; "+
						"escalate per operating-contract §7 rather than exceed it.",
					st.RespawnCount, itemID, sigOrNone(lastSig))
				fmt.Printf("coordinate: %s — respawn-with-context (cycle %d/%d)\n",
					itemID, st.RespawnCount, b.RespawnLimit)
				break superviseLoop // → outer loop re-spawns with extraCtx

			case coordinator.ActionEscalate:
				esc := coordinator.Escalation{
					Predicate: dec.Verdict.Predicate,
					Item:      itemID,
					Reason:    dec.Verdict.Reason,
					FailSig:   lastSig,
					At:        time.Now(),
				}
				res := coordinator.Fire(esc, b, dd, ex, time.Now())
				reportFire(itemID, esc, res)
				return 0 // escalation IS a clean stop (contract §7)
			}
		}
	}
}

// freshItem re-reads itemID from disk (a fresh store) so supervision sees
// the worker's committed progress + status — the in-memory parent store
// does not auto-reload (substrate ground truth, not a stale snapshot).
func freshItem(cfg *config.Config, id string) *model.Item {
	fs, err := store.New(cfg)
	if err != nil {
		return nil
	}
	it, ok := fs.Get(id)
	if !ok {
		return nil
	}
	return it
}

// progressed reports forward motion between two consecutive snapshots
// (used only to drive the backoff cadence — the authoritative item-level
// progress signal for B1 is coordinator.MadeItemProgress).
func progressed(a, b coordinator.ProgressSnapshot) bool {
	return b.RowCount > a.RowCount || b.ChangelogLen > a.ChangelogLen || b.JSONLMtime.After(a.JSONLMtime)
}

// reapDeadChildren non-blockingly wait()s every exited child of THIS
// coordinator process, clearing zombies so a subsequent
// agent.IsPIDLive(pid) is accurate (a zombie still answers signal-0).
// The coordinator's only forked children are spawned workers — the
// escalator's `st` subprocess is reaped by exec.Output() itself, and
// killWorker only signals — so a blanket Wait4(-1, WNOHANG) reaps exactly
// the workers we want and nothing we still need. Safe + idempotent: no
// children ⇒ ECHILD ⇒ no-op (returns immediately).
func reapDeadChildren() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		// pid>0: reaped one, look for more. pid==0: live children but
		// none exited. pid<0 (ECHILD): no children. err: stop (don't spin).
		if pid <= 0 || err != nil {
			return
		}
	}
}

// killWorker terminates AND deregisters every worker registration for
// itemID (the kill lever, §6 — PID-based liveness). Deregistration is the
// load-bearing half: command.Spawn writes a new nextSuffix registration
// per attempt and never removes the prior one, so without this each
// respawn leaves a stale dead registration behind and findWorkerReg can
// resolve the wrong (old) one. Clearing them here means the NEXT attempt's
// registration is the only one for this scope. Best-effort: a
// dead/absent/already-deregistered worker is the desired post-state.
func killWorker(cfg *config.Config, itemID string) {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		return
	}
	want := "item:" + itemID
	for _, r := range regs {
		if r == nil || r.Role != "worker" || r.Scope != want {
			continue
		}
		if agent.IsPIDLive(r.PID) {
			if proc, err := os.FindProcess(r.PID); err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		}
		// Remove the registration file so it cannot shadow the next
		// attempt's worker in findWorkerReg (the stale-reg bug).
		_ = agent.DeregisterSelf(cfg, r.AgentID)
	}
}

func sigOrNone(s string) string {
	if s == "" {
		return "(none captured)"
	}
	return s
}

func reportFire(itemID string, e coordinator.Escalation, res coordinator.FireResult) {
	if !res.Fired {
		fmt.Printf("coordinate: %s — §7 %s collapsed by dedupe (prior escalation is the record)\n",
			itemID, e.Predicate)
		return
	}
	fmt.Printf("coordinate: %s — ESCALATED §7 %s: %s\n", itemID, e.Predicate, e.Reason)
	if res.IssueID != "" {
		fmt.Printf("  filed blocker: %s (dep-linked to %s)\n", res.IssueID, itemID)
	}
	if res.Pinged {
		fmt.Println("  active-ping fired (category-E / budget cap)")
	}
	for _, err := range res.Errs {
		fmt.Fprintf(os.Stderr, "  WARNING escalation side-effect failed (surfaced, not swallowed): %v\n", err)
	}
}

// cmdEscalator is the real coordinator.Escalator. It reuses the running
// `st` binary (os.Executable — the dispatcher already resolved the correct
// per-agent binary) for the STRUCTURED operations (create issue, set SBAR,
// dep-link) so there is zero reinvention of the gate-aware creation path,
// and uses changelog/mail directly for the always-on durable + channel
// records. osascript powers the active-ping.
type cmdEscalator struct {
	cfg      *config.Config
	boundary *coordinator.Boundary
}

func (c *cmdEscalator) self() (string, error) { return os.Executable() }

// stRun execs the running st binary with args, ST_ROOT anchored to the
// resolved workspace and the operator's expiring AWS SSO profile stripped
// (same hardening as command.Spawn). Returns trimmed stdout.
func (c *cmdEscalator) stRun(stdin string, args ...string) (string, error) {
	bin, err := c.self()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = withoutEnv(withEnv(os.Environ(), "ST_ROOT", c.cfg.Root()),
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("st %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *cmdEscalator) FileBlocker(e coordinator.Escalation) (string, error) {
	// "before" must be read from a FRESH store, not the loop's possibly
	// stale in-memory one: a stale set would miss a recently-filed issue
	// and make newIssueID() return the wrong (older) id — filing the SBAR
	// against the wrong issue. Diff fresh-before vs fresh-after.
	pre, err := store.New(c.cfg)
	if err != nil {
		return "", fmt.Errorf("pre-create rescan: %w", err)
	}
	before := issueIDSet(pre)
	title := coordinator.EscalationTitle(e)
	if _, err := c.stRun("", "create", "issue", title); err != nil {
		return "", err
	}
	// Re-scan via a fresh store read so the new id is visible (st create
	// ran in a subprocess and committed it).
	fresh, err := store.New(c.cfg)
	if err != nil {
		return "", fmt.Errorf("post-create rescan: %w", err)
	}
	id := newIssueID(before, fresh)
	if id == "" {
		return "", fmt.Errorf("filed escalation issue but could not resolve its id")
	}
	// Deep-but-substantive SBAR synthesised from the incident (the
	// item-quality policy applies even to an auto-filed issue — this is a
	// real incident with real context, not scaffold).
	sit := fmt.Sprintf("Coordinator raised contract-§7 %s (category %s) on %s: %s",
		e.Predicate, coordinator.Category(e.Predicate), e.Item, e.Reason)
	bg := fmt.Sprintf("Filed by the Shape-3 coordinator loop (T-363) supervising %s. "+
		"Failure signature: %q. The autonomy boundary at .as/coordinator.yaml governs; "+
		"the loop STOPPED rather than exceed it (respawn_limit=%d).",
		e.Item, e.FailSig, c.boundary.RespawnLimit)
	as := fmt.Sprintf("Predicate %s is lever-unresolvable by the coordinator and "+
		"genuinely needs operator judgment (contract §7) — the loop did not work "+
		"around it. Confidence is high the trigger is real: it is read from substrate "+
		"ground truth (registry PID / session JSONL / changelog), never worker self-report.",
		e.Predicate)
	rec := fmt.Sprintf("Operator: triage %s. Resolve the root cause, then either "+
		"re-queue %s or close it. This issue BLOCKS %s until resolved.",
		e.Item, e.Item, e.Item)
	for field, val := range map[string]string{
		"sbar.situation":      sit,
		"sbar.background":     bg,
		"sbar.assessment":     as,
		"sbar.recommendation": rec,
	} {
		if _, err := c.stRun(val, "update", id, field, "--stdin"); err != nil {
			return id, fmt.Errorf("filed %s but SBAR %s failed: %w", id, field, err)
		}
	}
	// Dependency-link so the graph captures it (st dep add <item> <new> ⇒
	// item depends on the blocker). Never lost in prose (§7/§13).
	if _, err := c.stRun("", "dep", "add", e.Item, id); err != nil {
		return id, fmt.Errorf("filed %s but dep-link to %s failed: %w", id, e.Item, err)
	}
	return id, nil
}

func (c *cmdEscalator) Log(e coordinator.Escalation, issueID string) error {
	reason := e.Reason
	if issueID != "" {
		reason += " [filed " + issueID + "]"
	}
	return changelog.Append(c.cfg, e.Item, changelog.Entry{
		Timestamp: time.Now().Format(time.RFC3339),
		Agent:     c.cfg.Identity().ID,
		Op:        "escalate",
		Field:     string(e.Predicate),
		NewValue:  issueID,
		Reason:    reason,
	})
}

func (c *cmdEscalator) Mail(e coordinator.Escalation, issueID string) error {
	id := c.cfg.Identity().ID
	if id == "" {
		id = "coordinator"
	}
	body := fmt.Sprintf("[coordinator §7 %s] %s — %s", e.Predicate, e.Item, e.Reason)
	if issueID != "" {
		body += " (filed " + issueID + ")"
	}
	_, err := mail.Send(c.cfg, mail.Message{
		From: id,
		To:   id, // self/operator inbox — the conversation-channel source (§8.2)
		Kind: mail.KindAlert,
		At:   time.Now().Format(time.RFC3339),
		Body: body,
		Item: e.Item,
	})
	return err
}

func (c *cmdEscalator) Notify(e coordinator.Escalation) error {
	if runtime.GOOS != "darwin" {
		return nil // best-effort; the durable record is the changelog/issue
	}
	title := osaQuote("Coordinator §7 " + string(e.Predicate) + " on " + e.Item)
	body := osaQuote(e.Reason)
	script := "display notification " + body + " with title " + title
	// Best-effort: a failed notification must not abort the loop (the
	// alerts-band substrate is the durable record); surface as the error.
	return exec.Command("osascript", "-e", script).Run()
}

// osaQuote renders s as a safe AppleScript double-quoted string literal:
// interior newlines folded to spaces and embedded quotes/backslashes
// escaped, so a multi-line detector reason can't break the script (a
// broken script would silently drop the active-ping — exactly the
// silent-failure class this whole feature exists to kill).
func osaQuote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// --- store id-diff helpers (resolve the just-filed issue id) ---

func issueIDSet(s *store.Store) map[string]bool {
	out := map[string]bool{}
	for id, it := range s.All() {
		if it.Type == "issue" {
			out[id] = true
		}
	}
	return out
}

func newIssueID(before map[string]bool, s *store.Store) string {
	for id, it := range s.All() {
		if it.Type == "issue" && !before[id] {
			return id
		}
	}
	return ""
}
