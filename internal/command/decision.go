package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// recordStructuredDecision appends a native-structured decision entry
// (I-679 Phase B): captured verbatim as an immutable side-effect of an
// `st` command that, by construction, represents a settled fork — a plan
// approval, or a stack push whose --reason states why other work was
// interrupted. source=structured marks it authoritative.
//
// Phase C design intent (NOT yet enforced — Phase C is unbuilt): the
// extraction backstop will reconcile against SourceStructured entries and
// must never clobber one. Until it lands, that no-clobber guarantee is a
// design contract, not running code.
//
// This is the trust model the whole feature rests on: the decision is
// captured because the command ran, NOT because the agent remembered to
// record it (the exact mid-flight discipline the operator does not trust).
//
// trigger names the originating command (plan_approve | stack_push) so the
// resume renderer and a future reconciler can attribute the entry. An
// empty reason records nothing — a decision with no rationale is not a
// non-re-derivable fact, and a bare verdict is the useless half (I-679:
// "the why and the discarded options are the value").
// approachGist condenses a plan's Approach section into a one-line label
// for the plan_approve decision entry: first non-empty line, whitespace-
// collapsed, capped. It is a LABEL (the verdict — real signal), not a
// snapshot of the plan; the live plan body is surfaced separately by
// `st resume`. Empty in ⇒ empty out (caller falls back to a bare pointer).
func approachGist(approach string) string {
	for _, ln := range strings.Split(approach, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = strings.Join(strings.Fields(ln), " ")
		if len(ln) > 160 {
			ln = ln[:157] + "..."
		}
		return ln
	}
	return ""
}

// CaptureDecisionOpts controls `st capture-decision` (I-679 Phase B). It is
// the thin, hook-invoked entry point for native-structured decision capture
// from a PostToolUse hook (AskUserQuestion / ExitPlanMode). It deliberately
// holds no write logic of its own — it resolves the target item and delegates
// to recordStructuredDecision so the changelog write stays in the single
// tested place (one Source=structured / Kind=decision codepath, one
// never-silent failure report).
type CaptureDecisionOpts struct {
	ID      string // explicit item; empty ⇒ stack top, then this agent's first active item
	Trigger string // originating channel: ask_user_question | exit_plan_mode
	Reason  string // verbatim decision text; empty ⇒ nothing to capture
}

// CaptureDecision records a native-structured decision triggered from a
// PostToolUse hook. Item resolution mirrors `st resume`'s precedence exactly
// (resolveResumeTarget: stack top, then the CURRENT AGENT's first active
// item — agent-scoped, never a peer's) so the captured decision lands on
// whatever item the session is actually working — the same item `st resume`
// will replay it from next session. Returns a process exit
// code: 0 = captured or deliberately nothing to capture; 1 = could not be
// captured (no target item / unknown item). The *hook* always exits 0
// regardless — a PostToolUse failure must never break the tool call that
// already ran — but it inspects this code to decide whether to emit its loud
// "decision NOT captured" stderr line. Silence on this path is the failure
// mode the whole feature exists to prevent (operator silent-failure
// principle), so a non-capture is always reported here too.
func CaptureDecision(s *store.Store, cfg *config.Config, opts CaptureDecisionOpts) int {
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		// Not an error: an AskUserQuestion whose answer could not be
		// parsed, or an empty plan, is genuinely nothing to record. A
		// decision with no rationale is not a non-re-derivable fact
		// (matches recordStructuredDecision's own guard). Exit clean so
		// the hook stays quiet rather than crying wolf on every no-op.
		return 0
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "hook_decision"
	}

	id := strings.TrimSpace(opts.ID)
	if id == "" {
		id = resolveResumeTarget(s, cfg)
	}
	if id == "" {
		fmt.Fprintln(os.Stderr,
			"st capture-decision: no active/stack-top item to attribute this decision to — it was NOT recorded; push or start the relevant item so cross-session resume can capture forks.")
		return 1
	}
	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr,
			"st capture-decision: unknown item %q — decision NOT recorded.\n", id)
		return 1
	}

	recordStructuredDecision(cfg, id, trigger, reason)
	return 0
}

func recordStructuredDecision(cfg *config.Config, id, trigger, reason string) {
	if strings.TrimSpace(reason) == "" {
		return
	}
	// Best-effort but genuinely never silent. A failed write must not
	// break the command that triggered it, but it must NOT vanish: the
	// Phase A self-attestation audit only checks the exec/commit tape vs
	// git — it does NOT cover decision entries, so it cannot surface a
	// dropped decision. The only thing that makes this non-silent is
	// reporting the failure here, to stderr, the moment it happens.
	if err := changelog.Append(cfg, id, changelog.Entry{
		Op:     "decision",
		Field:  trigger,
		Kind:   changelog.KindDecision,
		Source: changelog.SourceStructured,
		Reason: reason,
	}); err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: failed to capture %s decision for %s (%v) — this rationale will NOT appear in `st resume`; re-state it via `st push --reason` or note it in the plan.\n",
			trigger, id, err)
	}
}
