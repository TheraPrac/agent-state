package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// ProgressSnapshot is one sample of a supervised worker's SUBSTRATE GROUND
// TRUTH at a point in time. It deliberately contains nothing the worker
// self-reports: the single most important operational finding of the
// I-554 probe (contract §13 f1) is that a worker's narrative ≠ reality, so
// every field here comes from the registry, the session JSONL, or the
// item changelog — never the worker's prose.
type ProgressSnapshot struct {
	PIDAlive     bool      // registry PID still alive (agent.IsPIDLive)
	JSONLMtime   time.Time // newest mtime across the worker's session JSONL
	RowCount     int       // cumulative parsed JSONL rows (a turn proxy)
	LastErrSig   string    // signature of the most recent errored tool_result ("" = none)
	ChangelogLen int       // # changelog entries for the item (monotonic item progress)
	Stage        string    // item status / delivery stage
	SampledAt    time.Time // wall-clock of this sample
	AICostUSD    float64   // T-365: rolled-up cost (I-369 Option C). The
	//                       item's LIFETIME spend; 0 ⇒ no rollup yet (e.g.
	//                       before the worker's first SubagentStop fires).
	//                       T-380: D2 does NOT read this field directly —
	//                       Decide computes a per-attempt DELTA against
	//                       WorkerState.attemptStartCostUSD and passes
	//                       that to DetectStuckByCost. The "treat 0 as no
	//                       signal" guard now lives on the delta, in
	//                       DetectStuckByCost's deltaUSD <= 0 short-circuit.
}

// WorkerState is the cross-respawn state the loop carries for ONE item's
// worker. It is what makes B1 (looping across respawns) detectable — a
// single ProgressSnapshot cannot see a loop; the respawn history can.
type WorkerState struct {
	Item         string
	SessionID    string
	PID          int
	SpawnedAt    time.Time     // registry Started of the CURRENT attempt
	RespawnCount int           // respawn-with-context cycles already spent
	LastFailSig  string        // failure signature of the PRIOR terminated attempt
	SizeClass    time.Duration // wall-clock baseline driving C2's WedgeThreshold (sole use post-T-381)
	CostBaseline float64       // T-365: USD baseline for cost-based D2 (per SizeClassCostBaseline)
	Snaps        []ProgressSnapshot

	// attemptStartChangelog is the item's changelog length at the moment
	// the CURRENT attempt was spawned. Progress for B1 is measured per
	// attempt (a respawn reproducing the same failure must read as "no
	// progress THIS attempt"). Set via WorkerState.BeginAttempt (loop.go).
	attemptStartChangelog int

	// attemptStartCostUSD is the item's rolled-up cost at the moment the
	// CURRENT attempt was spawned (T-380). D2 measures THIS WORKER's
	// burn — `current - attemptStartCostUSD` — NOT item-lifetime spend,
	// which would accumulate across many sessions and falsely
	// silence/trigger D2 depending on the item's history. Sibling of
	// attemptStartChangelog (T-363's per-attempt progress baseline);
	// set via BeginAttempt.
	//
	// Respawn race (T-380): there is a bounded window between killWorker
	// SIGTERM and the next iteration's freshItem read where the dying
	// worker's last in-flight subagent-stop-metrics hook may still flush
	// and bump ai_cost_usd higher than the value freshItem captured. By
	// the time Decide reads the next delta, attemptStartCostUSD has
	// already been re-captured from postItem (BeginAttempt) — so at
	// worst, the NEXT supervision tick's delta looks $X lower than reality
	// for one poll (D2 under-fires by $X). The following tick observes
	// the full burn once the rollup catches up, so the under-fire is
	// self-correcting within one supervision interval. Cumulative respawn
	// spend is bounded by respawn_limit via ClassifyRespawn (B1/C2), not
	// by D2.
	attemptStartCostUSD float64
}

// Predicate is a contract-§7 escalation predicate the detectors raise.
type Predicate string

const (
	PredicateNone Predicate = ""   // no stall — keep supervising / allow respawn
	PredicateB1   Predicate = "B1" // looping: respawn_limit cycles, unchanged failure signature
	PredicateC2   Predicate = "C2" // wedged / unrecoverable
	PredicateD2   Predicate = "D2" // stuck: ≥ stuck_multiplier × size-class baseline
)

// StallVerdict is a detector's decision plus the human-readable reason
// that goes verbatim into the escalation record (observability-as-trust:
// the operator must see WHY without re-deriving it).
type StallVerdict struct {
	Predicate Predicate
	Reason    string
}

// SampleProgress captures a ProgressSnapshot for a live worker. Impure (it
// reads the registry, the on-disk session JSONL, and the item changelog);
// the DETECTORS below are pure over []ProgressSnapshot so the stall logic
// is unit-testable without exec'ing a worker.
//
// Worker resolution is via the agent REGISTRY (Role=worker,
// Scope=item:<id>) — the same record `command.Spawn` writes — not the
// worker's word. A swept registration (PID dead) yields PIDAlive=false,
// which is itself the terminal signal the loop needs.
func SampleProgress(cfg *config.Config, item *model.Item) ProgressSnapshot {
	now := time.Now()
	snap := ProgressSnapshot{SampledAt: now, Stage: item.Status}

	// T-365: read I-369's rolled-up cost off the item. Pure parsing
	// extracted into parseCostUSD so the type-switch is unit-testable
	// without spinning a config / changelog / registry.
	if item.TimeTracking != nil {
		snap.AICostUSD = parseCostUSD(item.TimeTracking["ai_cost_usd"])
	}

	if entries, err := changelog.Read(cfg, item.ID); err == nil {
		snap.ChangelogLen = len(entries)
	}

	reg := findWorkerReg(cfg, item.ID)
	if reg == nil {
		// No live registration: worker not yet up, or terminated+swept.
		// PIDAlive stays false — the loop reads that as "terminal".
		return snap
	}
	snap.PIDAlive = agent.IsPIDLive(reg.PID)

	for _, p := range transcript.ResolveSessionByID(reg.SessionID) {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			if fi.ModTime().After(snap.JSONLMtime) {
				snap.JSONLMtime = fi.ModTime()
			}
		}
		rows, err := transcript.ReadFile(p)
		if err != nil {
			continue
		}
		snap.RowCount += len(rows)
		for _, r := range rows {
			if r.Kind == transcript.KindToolResult && r.ToolResult != nil && r.ToolResult.IsError {
				snap.LastErrSig = FailSignature(r.ToolResult.Content)
			}
		}
	}
	return snap
}

// findWorkerReg returns the CURRENT-attempt worker registration for
// itemID, or nil. Match is Role=worker AND Scope=item:<id> — what
// command.Spawn (workerRegisterOptions) writes.
//
// Respawn-with-context creates a NEW nextSuffix registration each attempt
// (agent-b-1, agent-b-2, …) and the prior one is not always swept yet, so
// MULTIPLE matches can coexist. ListRegistrations sorts ascending by
// AgentID, so a naive first-match returns the OLDEST (dead prior attempt)
// while the live current worker is misread as terminated → respawn thrash
// + an orphaned running worker. Resolution must therefore pick the
// CURRENT attempt: (1) any live-PID match wins; (2) otherwise the most
// recently Started match (a just-spawned-not-yet-alive or just-died
// current attempt beats an ancient one). Defensive against the stale-reg
// class even though killWorker also deregisters dead attempts now.
func findWorkerReg(cfg *config.Config, itemID string) *agent.Registration {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		return nil
	}
	want := "item:" + itemID
	var best *agent.Registration
	var bestStarted time.Time
	for _, r := range regs {
		if r == nil || r.Role != "worker" || r.Scope != want {
			continue
		}
		if agent.IsPIDLive(r.PID) {
			// A live match is unambiguously the current worker; among
			// (pathological) multiple-live, the newest Started wins.
			if best == nil || !agent.IsPIDLive(best.PID) || startedAfter(r, best) {
				best, bestStarted = r, parseStarted(r.Started)
			}
			continue
		}
		// No live match yet: track the most-recently-Started dead one so
		// a just-spawned/just-died CURRENT attempt beats an ancient one.
		if best == nil || (!agent.IsPIDLive(best.PID) && parseStarted(r.Started).After(bestStarted)) {
			best, bestStarted = r, parseStarted(r.Started)
		}
	}
	return best
}

func parseStarted(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func startedAfter(a, b *agent.Registration) bool {
	return parseStarted(a.Started).After(parseStarted(b.Started))
}

// FailSignature reduces an errored tool_result body to a stable short
// signature: first non-empty line, lower-cased, whitespace-collapsed, hashed.
// Two attempts that fail "the same way" (same gate, same error) produce the
// same signature; a different failure produces a different one — the basis
// of B1's "unchanged failure signature / no monotonic progress".
func FailSignature(body string) string {
	first := ""
	for _, ln := range strings.Split(body, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			first = strings.ToLower(strings.Join(strings.Fields(s), " "))
			break
		}
	}
	if first == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(first))
	return hex.EncodeToString(sum[:8])
}

// MadeItemProgress reports whether b shows forward ITEM progress over a
// (item-level, not JSONL chatter — a looping worker emits rows without
// progressing the item). Monotonic signal: more changelog entries OR a
// changed delivery stage.
func MadeItemProgress(a, b ProgressSnapshot) bool {
	return b.ChangelogLen > a.ChangelogLen || (b.Stage != "" && b.Stage != a.Stage)
}

// DetectWedged implements C2's "wedged" arm: PID alive but no transcript
// progress for ≥ wedge. snaps must be time-ordered. Returns ("", false)
// when not wedged; (reason, true) when the latest sample is alive and
// neither JSONL mtime, row count, nor changelog advanced across the whole
// trailing ≥ wedge window.
func DetectWedged(snaps []ProgressSnapshot, wedge time.Duration) (string, bool) {
	if len(snaps) < 2 {
		return "", false
	}
	last := snaps[len(snaps)-1]
	if !last.PIDAlive {
		return "", false // dead PID is terminal, not wedged — different path
	}
	// Walk back to the first sample ≥ wedge older than last.
	var base *ProgressSnapshot
	for i := len(snaps) - 2; i >= 0; i-- {
		if last.SampledAt.Sub(snaps[i].SampledAt) >= wedge {
			base = &snaps[i]
			break
		}
	}
	if base == nil {
		return "", false // not enough elapsed wall-clock yet
	}
	if last.RowCount == base.RowCount &&
		!last.JSONLMtime.After(base.JSONLMtime) &&
		last.ChangelogLen == base.ChangelogLen {
		return "PID alive but JSONL + changelog static for ≥ " + wedge.String() +
			" (no transcript progress) — wedged (contract §7-C2)", true
	}
	return "", false
}

// parseCostUSD reads a time_tracking.ai_cost_usd value from the
// map[string]interface{} substrate. session_log.go currently writes a
// 6-decimal stringified float, so string is the live path — but we
// defensively handle float64 and int too (matches the floatField
// pattern in internal/command/itemmetrics.go) so a future
// storage-format change does not silently blind D2. Unknown / missing
// types return 0 (the "rollup not yet populated" signal — see
// DetectStuckByCost).
func parseCostUSD(raw any) float64 {
	switch v := raw.(type) {
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// DetectStuckByCost implements D2 (T-365 + T-380): a WORKER burning ≥
// stuck_multiplier × cost-baseline-for-the-item's-size-class IN THIS
// ATTEMPT. The first argument is the PER-ATTEMPT cost delta (see
// WorkerState.attemptStartCostUSD), NOT the item's lifetime rollup —
// caller (Decide) computes `last.AICostUSD - st.attemptStartCostUSD`
// before invoking. Dollar-denominated, consistent with K1 (per_item
// budget cap is dollars) and matching the §7-D2 predicate text
// ("consuming ≥ K2 × median for size-class"). The underlying cost
// data is I-369's Option C rollup, surfaced into ProgressSnapshot by
// SampleProgress.
//
// Preconditions enforced by the caller (Decide in loop.go), NOT by this
// function:
//   - Only fires while PIDAlive==true (Decide's "live worker" branch).
//     A terminated worker is classified by ClassifyRespawn (B1/C2), not D2.
//   - D2 measures THIS ATTEMPT's burn — the caller passes the per-attempt
//     delta `last.AICostUSD - st.attemptStartCostUSD`, NOT item lifetime
//     spend. BeginAttempt resets attemptStartCostUSD on each respawn, so
//     D2 itself naturally resets across respawns. Cumulative cross-respawn
//     spend is bounded by respawn_limit via ClassifyRespawn — D2 doesn't
//     try to backstop unbounded respawn cycles; that's B1/C2's job.
//
// deltaUSD == 0 returns (false, "") — TWO legitimate sources of zero both
// reduce to "no D2 signal":
//  1. The rollup is not yet populated (e.g. before the worker's first
//     SubagentStop fires) — same as pre-T-380.
//  2. This attempt has not spent anything yet (post-T-380: on a fresh
//     attempt or respawn, last.AICostUSD == attemptStartCostUSD until
//     the worker burns its first cent).
//
// Neither case is a genuine "stuck at $0" signal. A worker that never
// spends a cent is either wedged (C2's domain) or making instant
// progress (no D2 needed).
//
// The startup blind window (between spawn and first SubagentStop) is
// covered by K1's per_item --max-budget-usd hard cap, set by st spawn
// regardless of D2 state. K1 fires before any runaway exceeds budget.
func DetectStuckByCost(deltaUSD, baselineUSD, mult float64) (string, bool) {
	if deltaUSD <= 0 || baselineUSD <= 0 || mult <= 0 {
		return "", false
	}
	limit := baselineUSD * mult
	if deltaUSD >= limit {
		// T-380: render "attempt cost" not bare "cost" so an operator
		// reading the changelog doesn't confuse this dollar figure with
		// the item's lifetime ai_cost_usd field.
		return "attempt cost $" + trimFloat(deltaUSD) + " ≥ stuck_multiplier(" +
			trimFloat(mult) + ") × cost-baseline $" + trimFloat(baselineUSD) +
			" — stuck (contract §7-D2; per-attempt delta, T-365+T-380)", true
	}
	return "", false
}

// (T-381: wall-clock DetectStuck deleted — superseded by
// DetectStuckByCost in T-365; production migration met; no remaining
// callers. SizeClassBaseline below is retained: it still feeds C2's
// WedgeThreshold for the transcript-silence window. See git history
// pre-T-381 if the wall-clock D2 proxy is ever needed back, e.g.
// for substrates pre-dating I-369.)

// ClassifyRespawn is the B1/C2 decision made when a worker has TERMINATED
// (PID gone) without completing the item. It decides — purely — whether
// the loop may respawn-with-context, or must escalate.
//
// respawn_limit is a HARD CAP and is checked FIRST — it bounds TOTAL
// respawn cycles regardless of progress. (A worker that makes a trivial
// bit of progress then exits every attempt must NOT respawn unboundedly:
// that would burn unbounded cumulative budget with no operator
// escalation, violating the bounded-autonomy invariant — contract §9/§11.
// D2 cannot backstop it because D2 only fires while the PID is alive.)
//
//   - RespawnCount ≥ RespawnLimit, SAME signature, no progress → B1:
//     looping on an unsatisfiable gate.
//   - RespawnCount ≥ RespawnLimit, anything else (incl. progressing-but-
//     never-completing, or different-signature churn) → C2: exhausted the
//     respawn budget, unrecoverable.
//   - Under the limit: made progress → respawn permitted; no progress →
//     respawn-with-context permitted (an informed retry, the lever).
func ClassifyRespawn(st *WorkerState, terminalSig string, madeProgress bool, b *Boundary) StallVerdict {
	sameSig := terminalSig != "" && terminalSig == st.LastFailSig
	if st.RespawnCount >= b.RespawnLimit {
		if sameSig && !madeProgress {
			return StallVerdict{
				Predicate: PredicateB1,
				Reason: "worker burned " + itoa(st.RespawnCount) + " respawn cycle(s) (limit " +
					itoa(b.RespawnLimit) + ") with the SAME failure signature and no item progress — " +
					"looping on an unsatisfiable gate (contract §7-B1)",
			}
		}
		return StallVerdict{
			Predicate: PredicateC2,
			Reason: "worker respawned-with-context " + itoa(st.RespawnCount) + "× (limit " +
				itoa(b.RespawnLimit) + ") and still has not completed the item — respawn budget " +
				"exhausted, unrecoverable (contract §7-C2)",
		}
	}
	if madeProgress {
		return StallVerdict{Predicate: PredicateNone, Reason: "made item progress this attempt and respawn budget remains — respawn permitted"}
	}
	return StallVerdict{Predicate: PredicateNone, Reason: "no progress but respawn budget remains — respawn-with-context permitted"}
}

// SizeClassBaseline is the documented default median wall-clock for an
// item's size class. After T-381's deletion of the wall-clock DetectStuck
// proxy, this function is consumed solely by WedgeThreshold for C2's
// transcript-silence window (D2 is now cost-based via DetectStuckByCost
// + SizeClassCostBaseline). These are deliberately coarse heuristics
// keyed by type+priority — NOT empirically-derived medians; they are
// intentionally generous so C2 only fires for genuine wedges, not
// normal variance.
func SizeClassBaseline(item *model.Item) time.Duration {
	pri := 2
	if item.Priority != nil {
		pri = *item.Priority
	}
	switch item.Type {
	case "issue":
		if pri <= 1 {
			return 25 * time.Minute
		}
		return 35 * time.Minute
	default: // task and anything else: builds skew longer
		if pri <= 1 {
			return 40 * time.Minute
		}
		return 50 * time.Minute
	}
}

// SizeClassCostBaseline is the heuristic median USD spend for an item's
// size class, used by DetectStuckByCost (T-365). Values are tuned so D2
// fires comfortably below K1's per_item cap of $40 even at K2=3 (the
// highest baseline below × 3 = $30, leaving $10 of headroom). Same
// type+priority shape as SizeClassBaseline; documented as heuristic.
// Empirical per-session deltas (the right shape — see T-380's archive
// finding) are tracked as T-383.
//
// NOTE: the K1-headroom guarantee holds ONLY when K2 ≤ 3. If the
// operator raises stuck_multiplier in coordinator.yaml above 3, the
// max baseline × K2 may exceed K1's cap; D2 would then never fire
// before K1 — see TestSizeClassCostBaseline_BelowK1Cap which proves
// the K2=3 case and TestSizeClassCostBaseline_K2CeilingBreach which
// proves K2=4 breaks the invariant for at least one size class.
//
// Real data anchoring these picks (2026-05 archive sample): T-345 = $3,
// T-369 (this session) ≈ $8-12 typical, T-352 = $43 (the runaway K1
// would now catch). The defaults below would catch a T-352-class
// runaway WELL before the $40 cap, which is the whole point.
func SizeClassCostBaseline(item *model.Item) float64 {
	pri := 2
	if item.Priority != nil {
		pri = *item.Priority
	}
	switch item.Type {
	case "issue":
		if pri <= 1 {
			return 3.0
		}
		return 4.0
	default: // task and anything else
		if pri <= 1 {
			return 6.0
		}
		return 10.0
	}
}

// --- tiny formatting helpers (reason strings only) ---

func itoa(n int) string { return strconv.Itoa(n) }

// trimFloat renders a multiplier compactly: 3 → "3", 2.5 → "2.5".
func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
