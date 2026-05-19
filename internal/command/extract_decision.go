package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/extract"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// ExtractDecisionsOpts controls `st extract-decisions` (I-679 Phase C). It is
// the hook-invoked entry point for the LOSSY backstop writer: PreCompact
// (primary, harness-reliable) and Stop (secondary, best-effort) feed the
// about-to-be-summarized transcript here so prose forks that never passed
// through a structured channel are recovered before the window is lost.
type ExtractDecisionsOpts struct {
	TranscriptPath string // JSONL path from the PreCompact/Stop hook stdin
	ID             string // explicit item; empty ⇒ agent-scoped resolution
	Trigger        string // precompact | precompact_<t> | stop
	Session        string // session_id from hook stdin; tags entries + the
	// stop finalize marker so resume can detect a prior session that was
	// KILLED (Stop never ran ⇒ no marker) vs one that cleanly finalized.
}

// sessionFinalizedOp marks that a session's Stop hook DID run and mined its
// window — its ABSENCE for a prior session (with the session having real
// activity) is how resume detects a kill/interrupt that skipped Stop. Stop
// is best-effort by design, so absence is the signal, not an error.
const sessionFinalizedOp = "session_finalized"

// triggerIsStop reports whether this invocation came from the Stop hook (vs
// PreCompact, which fires mid-session and must NOT write a finalize marker).
func triggerIsStop(trigger string) bool {
	return strings.HasPrefix(strings.TrimSpace(trigger), "stop")
}

// composeExtractedReason renders a Candidate into a single changelog Reason
// line. The verdict alone is the useless half (I-679); the why and the
// discarded options are the value, so they are appended when present.
func composeExtractedReason(c extract.Candidate) string {
	r := c.Text
	// Only append the why / discarded options when the verdict text does
	// not already contain them — the extractor's Text is often the whole
	// sentence (which already includes the "because …" clause), so a blind
	// append produces an ugly "X because Y — because Y" stutter.
	lower := strings.ToLower(r)
	if c.Rationale != "" && !strings.Contains(lower, strings.ToLower(c.Rationale)) {
		r += " — because " + c.Rationale
	}
	if c.RejectedAlts != "" && !strings.Contains(lower, strings.ToLower(c.RejectedAlts)) {
		r += " (rejected: " + c.RejectedAlts + ")"
	}
	return strings.TrimSpace(r)
}

// decisionAlreadyCovered reports whether an extracted candidate is already
// represented by an existing changelog decision entry — a Phase B
// source=structured capture of the same fork, OR a prior source=extracted
// entry (so re-running PreCompact on the same/overlapping transcript is
// idempotent and never duplicates). Reconcile-before-write is the property
// that lets the lossy extractor never clobber and never spam: it only
// appends forks no decision entry covers yet. Match uses the SAME
// normalization/containment as intra-extract dedup (extract.SameFork) so
// "already captured" cannot drift from "duplicate".
func decisionAlreadyCovered(existing []changelog.Entry, candText string) bool {
	want := extract.Norm(candText)
	for _, e := range existing {
		if e.EffectiveKind() != changelog.KindDecision {
			continue
		}
		if extract.SameFork(extract.Norm(e.Reason), want) {
			return true
		}
	}
	return false
}

// recordExtractedDecision appends one machine-inferred decision as
// Kind=decision Source=extracted with its Confidence. It is the single
// extracted-decision write site (sibling of recordStructuredDecision's
// single structured site) and is genuinely never silent — a failed write of
// the lossy backstop still must not vanish (the decision tape has no
// self-attestation audit). Returns the changelog error so the caller turns
// it into a loud non-capture exit code.
func recordExtractedDecision(cfg *config.Config, id, trigger string, c extract.Candidate) error {
	reason := composeExtractedReason(c)
	if reason == "" {
		return nil
	}
	err := changelog.Append(cfg, id, changelog.Entry{
		Op:         "decision",
		Field:      trigger,
		Kind:       changelog.KindDecision,
		Source:     changelog.SourceExtracted,
		Confidence: c.Confidence,
		Reason:     reason,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: failed to record extracted decision for %s (%v) — a prose fork recovered from the transcript will NOT appear in `st resume`.\n",
			id, err)
	}
	return err
}

// ExtractDecisions reads the transcript, extracts prose decision forks,
// reconciles them against the item's existing decision entries (never
// clobbering a structured record — design decision #4), and appends only
// the uncovered ones as source=extracted.
//
// Exit code contract mirrors CaptureDecision exactly so the PreCompact/Stop
// hooks key their loud path off it: 0 = appended, or a deliberate no-op
// (nothing to extract / all forks already covered — genuinely fine, stay
// quiet); 1 = every genuine failure (no/unknown/peer item; transcript
// unreadable; a changelog write failed). The hooks ALWAYS exit 0 themselves
// (a PreCompact/Stop hook must never block compaction/stop) but surface
// rc=1 loudly.
func ExtractDecisions(s *store.Store, cfg *config.Config, opts ExtractDecisionsOpts) int {
	path := strings.TrimSpace(opts.TranscriptPath)
	if path == "" {
		fmt.Fprintln(os.Stderr,
			"st extract-decisions: no transcript path given — nothing to scan; the about-to-be-summarized window was NOT mined for prose forks.")
		return 1
	}
	rows, err := transcript.ReadFile(path)
	if err != nil {
		// A missing/unreadable transcript is a genuine failure (the
		// window is about to be lost) — loud, never silent.
		fmt.Fprintf(os.Stderr,
			"st extract-decisions: cannot read transcript %q (%v) — prose forks in the to-be-summarized window were NOT recovered.\n",
			path, err)
		return 1
	}

	// Tag every entry this invocation writes (extracted forks AND the stop
	// finalize marker) with the hook-supplied session id, so resume can
	// tell a session that cleanly finalized from one that was killed
	// before Stop ran. Process-scoped: this is a single CLI subprocess.
	if sid := strings.TrimSpace(opts.Session); sid != "" {
		changelog.ActiveSessionID = sid
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "extracted"
	}
	isStop := triggerIsStop(trigger)

	cands := extract.Extract(rows)
	if len(cands) == 0 && !isStop {
		return 0 // nothing to record, not a finalizing stop — quiet no-op
	}

	// Resolve the item. With forks to record, a resolution failure is
	// loud (the fork would be lost). For a Stop with nothing to record we
	// still want a finalize marker, but Stop fires on EVERY turn end
	// including sessions not working an item — a loud "no item" there
	// would be constant noise, so that path resolves quietly and just
	// skips the marker (nothing to finalize for a no-item session).
	var id string
	if len(cands) == 0 && isStop {
		id = resolveResumeTarget(s, cfg)
		if id == "" {
			return 0 // no item this session — nothing to finalize, quiet
		}
	} else {
		var rc int
		id, rc = resolveCaptureTarget(s, cfg, opts.ID, "st extract-decisions")
		if rc != 0 {
			return rc
		}
	}

	existing, err := changelog.Read(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"st extract-decisions: cannot read existing changelog for %s (%v) — refusing to append blind (could duplicate or clobber); forks NOT recorded.\n",
			id, err)
		return 1
	}

	failed := false
	for _, c := range cands {
		if decisionAlreadyCovered(existing, c.Text) {
			continue // already structured-captured or previously extracted
		}
		if werr := recordExtractedDecision(cfg, id, trigger, c); werr != nil {
			failed = true
			continue
		}
		// Reflect the just-written entry so a later candidate that is the
		// same fork reconciles against it within this same run too.
		existing = append(existing, changelog.Entry{
			Op: "decision", Kind: changelog.KindDecision,
			Source: changelog.SourceExtracted, Reason: composeExtractedReason(c),
		})
	}

	// Stop finalize marker: record that THIS session's Stop hook ran and
	// mined its window. Idempotent — at most one per session. Its absence
	// for a prior session that had real activity is exactly how resume
	// detects a kill/interrupt that skipped best-effort Stop.
	if isStop && opts.Session != "" {
		already := false
		for _, e := range existing {
			if e.Op == sessionFinalizedOp && e.SessionID == opts.Session {
				already = true
				break
			}
		}
		if !already {
			if werr := changelog.Append(cfg, id, changelog.Entry{
				Op:     sessionFinalizedOp,
				Kind:   changelog.KindTransition,
				Reason: "session Stop hook finalized the decision record",
			}); werr != nil {
				fmt.Fprintf(os.Stderr,
					"warning: failed to write session-finalized marker for %s (%v) — next resume may falsely report this session as killed.\n",
					id, werr)
				failed = true
			}
		}
	}

	if failed {
		return 1
	}
	return 0
}
