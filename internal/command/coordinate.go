package command

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coordinator"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// workerResult reports a finished worker's item and terminal exit code back to
// the dispatch loop over the results channel. rc==0 is any clean stop (done OR
// escalated — an escalation is the loop doing its job); non-zero is an
// operational spawn failure that should stop the whole fan-out.
type workerResult struct {
	itemID string
	rc     int
}

// activeSlot tracks one in-flight worker for the dispatcher: the item, the
// per-objective cost baseline captured at dispatch (so D1 spend is a delta,
// not item-lifetime rollup), and the C1 conflict classes its plan touches (so
// later picks never dispatch a conflicting item concurrently — T-364).
type activeSlot struct {
	itemID          string
	startCostUSD    float64
	conflictClasses []string
}

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
	ex := escalatorFor(cfg, b)

	// Populate empirical cost baselines from the archive once at startup (T-383).
	coordinator.LoadEmpiricalBaselines(s.List(store.StatusFilter("done")), b)

	// --- Dry run: single-shot, side-effect-free (unchanged from T-363) ---
	if opts.DryRun {
		item, why := selectNext(s, cfg)
		if item == nil {
			fmt.Printf("coordinate: no eligible item (%s)\n", why)
			return 0
		}
		fmt.Println("DRY RUN — nothing launched, supervised, or escalated")
		fmt.Printf("boundary:    %s\n", bPath)
		fmt.Printf("  respawn_limit=%d per_item=$%g per_objective=$%g stuck_x=%g parallelism=%d dedupe=%dm\n",
			b.RespawnLimit, b.PerItemUSD, b.PerObjectiveUSD, b.StuckMultiplier, b.ParallelismCap, b.DedupeWindowMin)
		fmt.Printf("picked:      %s — %s\n", item.ID, item.Title)
		fmt.Printf("why:         %s\n", why)
		key := coordinator.CostBinKey(item)
		n := coordinator.EmpiricalSamplesForBin(key)
		costSrc := "heuristic"
		if n > 0 {
			costSrc = fmt.Sprintf("empirical N=%d", n)
		}
		fmt.Printf("size-class:  %s wall-clock · $%g cost [%s] (D2 cost-based; stuck at ≥ %g×)\n",
			coordinator.SizeClassBaseline(item),
			coordinator.SizeClassCostBaseline(item), costSrc, b.StuckMultiplier)
		fmt.Println("next:        st spawn " + item.ID + spawnBudgetSuffix(opts.BudgetOverride))
		return 0
	}

	// --- Concurrent fan-out: maintain up to parallelism_cap in-flight workers ---
	//
	// Concurrency-safety (T-364): the supervise loop reads via per-call fresh
	// stores (freshItem) and the dispatch read path (selectNextExcluding) also
	// reads a fresh store, so a peer goroutine's claim/completion is reflected
	// without racing the in-memory parent store. The genuinely shared mutable
	// state is process-global and is serialized by `mu`: worker spawn (git
	// worktree creation on the shared repo + the lock-free agent-registry
	// write) and escalation Fire (deduper map + escalator issue create/diff).
	parCap := b.ParallelismCap
	if parCap < 1 {
		parCap = 1
	}

	// mu serializes the two operations that touch PROCESS-GLOBAL shared state:
	// Spawn (git index + agent registry) and coordinator.Fire (deduper map +
	// escalator). Both are brief/rare; worker execution runs unserialized.
	var mu sync.Mutex
	cancel := make(chan struct{})
	var cancelOnce sync.Once
	stopAll := func() { cancelOnce.Do(func() { close(cancel) }) }

	results := make(chan workerResult, parCap*2)
	active := map[string]*activeSlot{}
	dispatched := 0
	completedSpendUSD := 0.0
	var lastSkip string

	// canDispatch caps total dispatches. --once is always exactly one (it must
	// not fan out to the cap even if MaxItems is left unbounded); otherwise
	// MaxItems==0 means unbounded and any positive value is the hard cap.
	canDispatch := func() bool {
		if opts.Once {
			return dispatched < 1
		}
		return opts.MaxItems == 0 || dispatched < maxItems
	}

	for {
		// Fill idle slots up to the cap with non-conflicting, unclaimed work.
		for len(active) < parCap && canDispatch() {
			occupied := make(map[string]bool, len(active))
			var inflightClasses []string
			for id, slot := range active {
				occupied[id] = true
				inflightClasses = append(inflightClasses, slot.conflictClasses...)
			}
			item, why := selectNextExcluding(cfg, occupied, inflightClasses)
			if item == nil {
				lastSkip = why
				break
			}
			slot := &activeSlot{
				itemID:          item.ID,
				startCostUSD:    costUSDOf(cfg, item.ID),
				conflictClasses: loadConflictClasses(cfg, item.ID),
			}
			active[item.ID] = slot
			dispatched++
			fmt.Printf("coordinate: dispatch rationale — %s\n", why)
			st := &coordinator.WorkerState{
				Item:         item.ID,
				SizeClass:    coordinator.SizeClassBaseline(item),     // kept for C2 wedge threshold
				CostBaseline: coordinator.SizeClassCostBaseline(item), // T-365: cost-based D2
			}
			itemID := item.ID
			go func() {
				rc := superviseFn(cfg, b, dd, ex, st, itemID, opts, base, idleCap, &mu, cancel)
				results <- workerResult{itemID: itemID, rc: rc}
			}()
		}

		// Quiescent: nothing in flight and nothing dispatchable → done.
		if len(active) == 0 {
			if lastSkip != "" {
				// The fill loop found nothing dispatchable — surface WHY.
				fmt.Printf("coordinate: no eligible item (%s)\n", lastSkip)
			} else {
				// Stopped because the dispatch budget (--once / --max-items)
				// is exhausted, not because the queue is empty.
				fmt.Printf("coordinate: dispatch budget reached (%d item(s) processed)\n", dispatched)
			}
			return 0
		}

		// Block until one worker reaches a terminal state.
		res := <-results
		slot := active[res.itemID]
		delete(active, res.itemID)

		// Operational spawn failure: stop dispatching, cancel + drain the rest,
		// surface the failure code.
		if res.rc != 0 {
			fmt.Fprintf(os.Stderr, "coordinate: worker %s failed (rc=%d) — stopping fan-out\n", res.itemID, res.rc)
			stopAll()
			drainResults(results, active)
			return res.rc
		}

		// Fold this worker's spend into the cumulative objective total.
		if slot != nil {
			if d := costUSDOf(cfg, res.itemID) - slot.startCostUSD; d > 0 {
				completedSpendUSD += d
			}
		}

		// D1: per-objective cumulative budget across ALL workers (completed +
		// live). Escalate-not-exceed (contract §7-D1): stop everything the
		// moment the cap is crossed.
		//
		// "Objective" = this coordinator run (all items it dispatches). Per-goal
		// / per-sprint budget partitioning is the cross-objective concern (C3),
		// explicitly operator-only and out of T-364 scope; for the MVP a single
		// run is one objective. The check fires at completion boundaries, so
		// in-flight spend can briefly exceed the cap before the next worker
		// returns — acceptable for an escalate-not-exceed stop.
		objective := completedSpendUSD + objectiveSpendUSD(cfg, active)
		if b.PerObjectiveUSD > 0 && objective >= b.PerObjectiveUSD {
			fmt.Printf("coordinate: per-objective spend $%.2f ≥ cap $%g — D1 escalate, stopping all workers\n",
				objective, b.PerObjectiveUSD)
			stopAll()
			for id := range active {
				killWorker(cfg, id)
			}
			esc := coordinator.Escalation{
				Predicate: coordinator.PredicateD1,
				Item:      res.itemID,
				Reason: fmt.Sprintf("Cumulative per-objective spend $%.2f reached the $%g cap across %d dispatched worker(s); coordinator stopped fan-out rather than exceed the boundary (contract §7-D1).",
					objective, b.PerObjectiveUSD, dispatched),
				At: time.Now(),
			}
			mu.Lock()
			fr := coordinator.Fire(esc, b, dd, ex, time.Now())
			mu.Unlock()
			reportFire(esc.Item, esc, fr)
			drainResults(results, active)
			return 0
		}

		// --once: a single dispatch + its terminal stop is the whole run.
		if opts.Once {
			stopAll()
			drainResults(results, active)
			return 0
		}
	}
}

// drainResults waits for every still-active worker to report in, discarding
// the codes — used after the loop has decided to stop (D1 budget or an
// operational failure) and has signalled cancellation. Guarantees no goroutine
// is left writing to the results channel after Coordinate returns.
func drainResults(results <-chan workerResult, active map[string]*activeSlot) {
	for len(active) > 0 {
		r := <-results
		delete(active, r.itemID)
	}
}

// selectNext derives the top-ranked eligible item from item properties
// (g.Ready() + ClaimedBy filter), scored by coordinator.Recommend with
// operator queue-pins applied. For autonomous dispatch ONLY, items without
// an approved plan are skipped — this restores the I-491 "no plan, no
// dispatch" guard that the removal of the queue-approval gate would otherwise
// silently drop. Returns (nil, reason) when nothing qualifies (never an
// opaque empty — that blindness is what §1 removes).
func selectNext(s *store.Store, cfg *config.Config) (*model.Item, string) {
	// Single-shot / dry-run pick: read the in-memory store the caller built at
	// Coordinate entry (current as of startup; a dry run launches nothing, so
	// no concurrent worker can stale it). No exclusions.
	return selectFrom(s, cfg, nil, nil)
}

// selectNextExcluding is the concurrency-aware dispatch pick (T-364). It reads
// a FRESH store each call — never the shared in-memory parent — so it (a) does
// not race goroutine Spawns that mutate their own stores, (b) correctly drops
// items a peer worker just completed (committed to disk), and (c) sees fresh
// claim state. Exclusions skip in-flight items and C1-conflicting candidates.
func selectNextExcluding(cfg *config.Config, occupied map[string]bool, inflightClasses []string) (*model.Item, string) {
	fs, err := store.New(cfg)
	if err != nil {
		return nil, fmt.Sprintf("store unreadable: %v", err)
	}
	return selectFrom(fs, cfg, occupied, inflightClasses)
}

// selectFrom is the shared scoring+filtering core. It runs the same recommend
// pipeline selectNext always has, then drops candidates without an approved
// plan (I-491), candidates already in flight (occupied), and candidates whose
// plan's C1 conflict classes overlap an in-flight worker's (serialize the same
// OpenAPI/migration surface instead of running concurrently — T-364). Returns
// (nil, reason) when nothing is dispatchable.
func selectFrom(s *store.Store, cfg *config.Config, occupied map[string]bool, inflightClasses []string) (*model.Item, string) {
	g := deps.Build(s.All(), cfg)
	sprints := loadSprintInfo(cfg, g)
	cands := recommendCandidates(s, cfg, g, RecommendOpts{}, sprints)
	if len(cands) == 0 {
		return nil, "0 ready items (unblocked, unassigned, unclaimed)"
	}
	lev, names := unblockLeverage(g, cands)
	pins := loadQueuePins(cfg)
	priorityOverrides := buildPriorityOverrides(g, cands, pins)
	recs := coordinator.Recommend(cands, lev, sprints, loadGoalWeights(s), priorityOverrides, time.Now())
	enrichUnblockDetail(recs, names)
	enrichPriorityDetail(recs, priorityOverrides, g.Items)
	serialized := 0
	for _, r := range recs {
		if !r.Item.PlanApproved {
			continue // I-491: autonomous dispatch requires an approved plan
		}
		if occupied[r.Item.ID] {
			continue // already in flight
		}
		if coordinator.C1Conflicts(inflightClasses, loadConflictClasses(cfg, r.Item.ID)) {
			serialized++
			continue // C1: defer until the conflicting in-flight worker finishes
		}
		return r.Item, r.Rationale()
	}
	if serialized > 0 {
		return nil, fmt.Sprintf("%d planned candidate(s) deferred — C1-conflicting with an in-flight worker (same OpenAPI/migration surface)", serialized)
	}
	return nil, fmt.Sprintf("%d candidate(s) ready but none have an approved plan (I-491) or all are in flight", len(recs))
}

// loadConflictClasses returns the C1 conflict classes an item's approved plan
// touches (empty if no plan / unreadable). Used to serialize concurrent
// workers that would otherwise collide on the OpenAPI contract or migration
// changelog.
func loadConflictClasses(cfg *config.Config, itemID string) []string {
	p, err := plan.Load(cfg.PlansDir(), itemID)
	if err != nil || p == nil {
		return nil
	}
	return coordinator.ConflictSensitivePaths(p)
}

// costUSDOf is the indirection seam for reading an item's current AI spend.
// Defaults to currentCostUSD (fresh disk read); tests override it to script
// per-item spend deterministically without plumbing nested time_tracking
// values onto disk. Used by every per-objective (D1) cost read.
var costUSDOf = currentCostUSD

// currentCostUSD reads an item's current rolled-up AI spend from a fresh store
// (the same ai_cost_usd source coordinator.SampleProgress uses for D2). 0 if
// the item can't be read.
func currentCostUSD(cfg *config.Config, itemID string) float64 {
	it := freshItem(cfg, itemID)
	if it == nil {
		return 0
	}
	return coordinator.SampleProgress(cfg, it).AICostUSD
}

// objectiveSpendUSD sums each live worker's burn since dispatch (current cost
// minus the slot's captured baseline, floored at 0). The dispatch loop adds
// this to the completed-worker total for the D1 per-objective budget check.
func objectiveSpendUSD(cfg *config.Config, slots map[string]*activeSlot) float64 {
	var sum float64
	for id, slot := range slots {
		if d := costUSDOf(cfg, id) - slot.startCostUSD; d > 0 {
			sum += d
		}
	}
	return sum
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
// escalatorFor builds the production escalator (cmdEscalator, which reuses the
// real `st` binary for gate-aware issue creation). It is a package var so tests
// can inject a fake escalator and exercise the D1 fan-out escalation path
// without execing subprocesses — the same testability rationale the Escalator
// interface was introduced for.
var escalatorFor = func(cfg *config.Config, b *coordinator.Boundary) coordinator.Escalator {
	return &cmdEscalator{cfg: cfg, boundary: b}
}

// superviseFn is the indirection seam the dispatch loop calls. It defaults to
// the real superviseItem; tests override it with a fast fake so the concurrent
// dispatch / D1-budget / C1-serialization logic can be exercised under the
// race detector WITHOUT forking real `claude` workers (the real superviseItem
// is proven by live-verify, per the T-363 precedent — its poll loop assumes a
// genuinely registered OS worker that a unit test cannot faithfully fork).
var superviseFn = superviseItem

func superviseItem(cfg *config.Config, b *coordinator.Boundary,
	dd *coordinator.Deduper, ex coordinator.Escalator, st *coordinator.WorkerState,
	itemID string, opts CoordinateOpts, base, idleCap time.Duration,
	mu *sync.Mutex, cancel <-chan struct{}) int {

	extraCtx := "" // empty on the first attempt; set on respawn-with-context
	for {
		// Honor a coordinator-wide stop (D1 budget / peer failure) BEFORE
		// (re)spawning — otherwise a worker that just broke out of the supervise
		// loop to respawn would launch a NEW worker after the dispatch loop
		// already killed everything, orphaning a budget-burning process past the
		// boundary the stop exists to enforce.
		select {
		case <-cancel:
			return 0
		default:
		}

		// --- (re)spawn ---
		baseItem := freshItem(cfg, itemID)
		if baseItem == nil {
			fmt.Fprintf(os.Stderr, "coordinate: %s vanished from the store before spawn\n", itemID)
			return 1
		}
		// Each worker spawns through its OWN in-memory store, but Spawn→Start is
		// NOT fully isolated: it mutates process-global resources — the shared
		// git index (`git worktree add` on the main repo) and the agent registry
		// (a lock-free read-then-write of the next worker suffix). Concurrent
		// Spawns would collide on .git/index.lock and clobber each other's
		// registration files. So serialize the whole Spawn under the shared
		// mutex: worktree creation + launch is brief (seconds), while the
		// worker EXECUTION that follows runs fully concurrent (T-364).
		gs, err := store.New(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "coordinate: %s store init failed before spawn: %v\n", itemID, err)
			return 1
		}
		mu.Lock()
		// Re-check cancel under the lock: the dispatch loop closes cancel
		// (stopAll) BEFORE taking mu for the D1 Fire, so any worker that was
		// blocked here waiting on mu will see the stop and bail rather than
		// launch a process after the budget already tripped. (A worker already
		// mid-Spawn when cancel closes cannot be caught — its per-item cap still
		// bounds the overrun.)
		select {
		case <-cancel:
			mu.Unlock()
			return 0
		default:
		}
		rc := Spawn(gs, cfg, SpawnOpts{
			Item:           itemID,
			BudgetOverride: opts.BudgetOverride,
			ExtraContext:   extraCtx,
		})
		mu.Unlock()
		if rc != 0 {
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
			// Store momentarily unreadable. baseItem is the right fallback
			// for COST: ai_cost_usd is only mutated by subagent-stop-metrics
			// hooks (I-369/T-330), which fire from a worker's own subagent
			// calls — and Spawn between baseItem and postItem only forks
			// the worker, it never invokes a subagent itself. So
			// baseItem.ai_cost_usd == postItem.ai_cost_usd in normal
			// operation; the cost reading does NOT regress to wrong-unit
			// (item-lifetime) semantics on this degrade path. Changelog
			// length CAN differ (Spawn writes st-start claim rows on
			// attempt 1), which costs B1 a touch of precision on
			// progress-detection during a store hiccup — acceptable
			// degrade, not a cost-semantic regression.
			postItem = baseItem
		}
		// T-380: capture post-spawn cost too, so D2 measures THIS WORKER's
		// burn (delta from this baseline) instead of item-lifetime rollup.
		postSnap := coordinator.SampleProgress(cfg, postItem)
		st.BeginAttempt(time.Now(), postSnap.ChangelogLen, postSnap.AICostUSD)
		fmt.Printf("coordinate: spawned worker on %s (attempt %d, size-class %s, observe: st watch | st transcript %s)\n",
			itemID, st.RespawnCount+1, st.SizeClass, itemID)

		// --- supervise this attempt ---
		interval := base
		seenAlive := false
		var lastSig string
	superviseLoop:
		for {
			time.Sleep(interval)
			// Coordinator-wide stop (D1 budget crossed or a peer worker hit an
			// operational failure): exit promptly WITHOUT respawning, so the
			// kill the loop just issued is not undone by this worker's own
			// respawn-with-context path (T-364).
			select {
			case <-cancel:
				return 0
			default:
			}
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
				// killWorker → next-iter freshItem race is bounded; see
				// WorkerState.attemptStartCostUSD doc in supervise.go for
				// the D2-correctness argument.
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
				// Serialize Fire across concurrent workers: it mutates the
				// shared deduper map and drives the escalator's create-then-
				// diff issue-ID resolution, neither of which is concurrency
				// safe (T-364).
				mu.Lock()
				res := coordinator.Fire(esc, b, dd, ex, time.Now())
				mu.Unlock()
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
