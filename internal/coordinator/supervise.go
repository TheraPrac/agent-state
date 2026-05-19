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
}

// WorkerState is the cross-respawn state the loop carries for ONE item's
// worker. It is what makes B1 (looping across respawns) detectable — a
// single ProgressSnapshot cannot see a loop; the respawn history can.
type WorkerState struct {
	Item         string
	SessionID    string
	PID          int
	SpawnedAt    time.Time // registry Started of the CURRENT attempt
	RespawnCount int       // respawn-with-context cycles already spent
	LastFailSig  string    // failure signature of the PRIOR terminated attempt
	SizeClass    time.Duration
	Snaps        []ProgressSnapshot

	// attemptStartChangelog is the item's changelog length at the moment
	// the CURRENT attempt was spawned. Progress for B1 is measured per
	// attempt (a respawn reproducing the same failure must read as "no
	// progress THIS attempt"). Set via WorkerState.BeginAttempt (loop.go).
	attemptStartChangelog int
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

// findWorkerReg returns the live worker registration for itemID, or nil.
// Match is Role=worker AND Scope=item:<id> — exactly what command.Spawn
// (workerRegisterOptions) writes.
func findWorkerReg(cfg *config.Config, itemID string) *agent.Registration {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		return nil
	}
	want := "item:" + itemID
	for _, r := range regs {
		if r != nil && r.Role == "worker" && r.Scope == want {
			return r
		}
	}
	return nil
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

// DetectStuck implements D2: an item consuming ≥ stuck_multiplier × the
// median for its size-class with no error (distinct from wedged — the
// worker may be making JSONL noise, just far over budget-of-time). This
// is the wall-clock proxy; cost-based D2 is T-365 (the per_item
// --max-budget-usd cap, set by st spawn, is the independent hard backstop
// regardless). Returns (reason, true) when elapsed ≥ mult × baseline.
func DetectStuck(spawnedAt, now time.Time, baseline time.Duration, mult float64) (string, bool) {
	if baseline <= 0 || mult <= 0 || spawnedAt.IsZero() {
		return "", false
	}
	limit := time.Duration(float64(baseline) * mult)
	elapsed := now.Sub(spawnedAt)
	if elapsed >= limit {
		return "elapsed " + elapsed.Round(time.Second).String() + " ≥ stuck_multiplier(" +
			trimFloat(mult) + ") × size-class baseline " + baseline.String() +
			" — stuck (contract §7-D2; wall-clock proxy, cost-based D2 is T-365)", true
	}
	return "", false
}

// ClassifyRespawn is the B1/C2 decision made when a worker has TERMINATED
// (PID gone) without completing the item. It decides — purely — whether
// the loop may respawn-with-context, or must escalate.
//
//   - made progress this attempt        → reset streak, respawn (if under limit)
//   - no progress, NEW failure signature → respawn (a different failure is
//     not yet a loop; give the informed retry a chance), if under limit
//   - no progress, SAME signature, and respawn budget exhausted
//     (RespawnCount ≥ RespawnLimit) → B1: looping on an unsatisfiable gate
//   - respawn budget exhausted on ANY no-progress terminal             → C2:
//     unrecoverable (the "respawned-with-context limit× still failing" arm)
func ClassifyRespawn(st *WorkerState, terminalSig string, madeProgress bool, b *Boundary) StallVerdict {
	if madeProgress {
		return StallVerdict{Predicate: PredicateNone, Reason: "made item progress this attempt — respawn permitted"}
	}
	sameSig := terminalSig != "" && terminalSig == st.LastFailSig
	if st.RespawnCount >= b.RespawnLimit {
		if sameSig {
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
				itoa(b.RespawnLimit) + ") and is still failing without progress — unrecoverable (contract §7-C2)",
		}
	}
	return StallVerdict{Predicate: PredicateNone, Reason: "no progress but respawn budget remains — respawn-with-context permitted"}
}

// SizeClassBaseline is the documented default median wall-clock for an
// item's size class, used by D2's wall-clock proxy. These are deliberately
// coarse heuristics keyed by type+priority — NOT empirically-derived
// medians (that needs the historical cost/time rollup tracked as T-365).
// They are intentionally generous so D2 catches genuine runaways, not
// normal variance; the per_item budget cap is the hard backstop.
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

// --- tiny formatting helpers (reason strings only) ---

func itoa(n int) string { return strconv.Itoa(n) }

// trimFloat renders a multiplier compactly: 3 → "3", 2.5 → "2.5".
func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
