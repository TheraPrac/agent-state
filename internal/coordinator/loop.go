package coordinator

import (
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

// This file is the pure DECISION CORE of the coordinator loop. The
// imperative shell (load the queue, exec command.Spawn, poll the
// substrate, drive git/escalation side effects) lives in
// internal/command/coordinate.go — it cannot live here without an import
// cycle (command → coordinator for the cobra command). Keeping the brain
// pure makes every stall/dispatch rule unit-testable with synthetic
// snapshots, never an exec'd worker (the T-360/§13 lesson: prove the logic
// without spending money).

// EligibleForDispatch is the pure dispatch filter, mirroring `st queue
// next` semantics (approved + unblocked + non-terminal) PLUS the
// coordinator-specific guards: never pick an item already claimed or
// assigned to another agent (the single-in-flight invariant + the
// CLAUDE.md "never start a peer's item" coordination rule). Returns
// (false, reason) so the loop can log WHY an item was skipped (an opaque
// "nothing to do" is the §1 blindness this whole design removes).
func EligibleForDispatch(item *model.Item, approved, blocked, terminal bool) (bool, string) {
	if item == nil {
		return false, "item not in store"
	}
	if !approved {
		return false, "queue entry not operator-approved (I-490)"
	}
	if blocked {
		return false, "blocked by an open dependency"
	}
	if terminal {
		return false, "already in a terminal status"
	}
	if item.ClaimedBy != "" {
		return false, "already claimed by session " + item.ClaimedBy
	}
	if item.AssignedTo != "" {
		return false, "assigned to " + item.AssignedTo + " (peer item — coordination rule)"
	}
	return true, ""
}

// Action is what the loop should do after the latest supervision sample.
type Action int

const (
	ActionContinue     Action = iota // keep supervising on the backoff
	ActionTerminalDone               // worker exited having progressed the item → advance
	ActionRespawn                    // worker exited w/o progress, budget remains → respawn-with-context
	ActionEscalate                   // a §7 predicate fired → Fire + STOP this item
)

// SuperviseDecision is the decision-core verdict for one poll tick.
type SuperviseDecision struct {
	Action  Action
	Verdict StallVerdict // populated only when Action == ActionEscalate
}

// WedgeThreshold derives the C2 "no transcript progress" window from the
// item's size class: a quarter of the size-class baseline, floored at 5m
// so a tiny-baseline item can't flap into a false wedge on normal think
// time. Documented + pure so the live-verify "deliberately wedged worker"
// can reason about how long to stall.
func WedgeThreshold(sizeClass time.Duration) time.Duration {
	w := sizeClass / 4
	if w < 5*time.Minute {
		w = 5 * time.Minute
	}
	return w
}

// Decide is the loop's brain for one poll tick. Inputs are SUBSTRATE
// GROUND TRUTH only (snapshots, item completion from status — never the
// worker's word, §13 f1). Order of checks matters: a terminated worker is
// classified first (B1/C2 across respawns); a live worker is checked for
// wedge (C2) then stuck (D2); otherwise keep going.
//
//   - snaps must be time-ordered and non-empty.
//   - itemCompleted: the loop resolved the item to a terminal status
//     (cfg.IsTerminalStatus) — the only trustworthy "done" signal.
func Decide(st *WorkerState, snaps []ProgressSnapshot, b *Boundary, itemCompleted bool, now time.Time) SuperviseDecision {
	if len(snaps) == 0 {
		return SuperviseDecision{Action: ActionContinue}
	}
	last := snaps[len(snaps)-1]

	// --- terminated worker (PID gone): B1/C2 across respawn history ---
	if !last.PIDAlive {
		if itemCompleted {
			return SuperviseDecision{Action: ActionTerminalDone}
		}
		madeProgress := last.ChangelogLen > st.attemptStartChangelog
		v := ClassifyRespawn(st, last.LastErrSig, madeProgress, b)
		if v.Predicate != PredicateNone {
			return SuperviseDecision{Action: ActionEscalate, Verdict: v}
		}
		// No §7 predicate yet: progressed-but-not-complete, or budget
		// remains for an informed retry.
		if madeProgress {
			// Forward motion but the item isn't terminal — let the loop
			// respawn-with-context to carry it the rest of the way.
			return SuperviseDecision{Action: ActionRespawn}
		}
		return SuperviseDecision{Action: ActionRespawn}
	}

	// --- live worker: wedge (C2) then stuck (D2) ---
	if reason, wedged := DetectWedged(snaps, WedgeThreshold(st.SizeClass)); wedged {
		return SuperviseDecision{Action: ActionEscalate, Verdict: StallVerdict{Predicate: PredicateC2, Reason: reason}}
	}
	// T-365 + T-380: D2 is cost-based AND per-attempt. Compares THIS
	// WORKER's spend since spawn (delta) against the heuristic baseline.
	// Item-lifetime rollup is wrong-unit here — see attemptStartCostUSD's
	// comment on WorkerState. Negative delta (rollup oddity, e.g. a
	// substrate edit) clamps to 0 so D2 cannot mis-fire on a phantom
	// negative.
	delta := last.AICostUSD - st.attemptStartCostUSD
	if delta < 0 {
		delta = 0
	}
	if reason, stuck := DetectStuckByCost(delta, st.CostBaseline, b.StuckMultiplier); stuck {
		return SuperviseDecision{Action: ActionEscalate, Verdict: StallVerdict{Predicate: PredicateD2, Reason: reason}}
	}
	return SuperviseDecision{Action: ActionContinue}
}

// BeginAttempt records the per-attempt baselines + spawn time. Called by
// the loop immediately after a (re)spawn so Decide measures progress
// (T-363: changelog delta) AND cost burn (T-380: cost delta) relative
// to THIS attempt, not the item's whole history.
func (st *WorkerState) BeginAttempt(spawnedAt time.Time, changelogLen int, costUSD float64) {
	st.SpawnedAt = spawnedAt
	st.attemptStartChangelog = changelogLen
	st.attemptStartCostUSD = costUSD
}

// RecordRespawn advances the respawn bookkeeping after the loop has
// decided (via Decide → ActionRespawn) to kill+respawn-with-context. It
// captures the failure signature of the attempt just ended so the NEXT
// ClassifyRespawn can see "same signature, no progress" = B1.
func (st *WorkerState) RecordRespawn(endedSig string) {
	st.LastFailSig = endedSig
	st.RespawnCount++
}
