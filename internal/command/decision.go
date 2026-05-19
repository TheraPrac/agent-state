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
// No-clobber is ENFORCED (Phase C shipped): the extraction backstop
// (command.ExtractDecisions) reconciles against existing decision entries —
// structured AND prior extracted — and only appends uncovered forks, so a
// verbatim structured record can never be overwritten or duplicated.
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
	// ID is an explicit item. Empty ⇒ stack top, then (when an agent
	// identity resolves) this agent's first active item; with no agent
	// identity, the global first-active fallback — see resolveResumeTarget.
	ID      string
	Trigger string // originating channel: ask_user_question | exit_plan_mode
	Reason  string // verbatim decision text; empty ⇒ nothing to capture
}

// resolveCaptureTarget is the shared item resolver + cross-agent write guard
// for BOTH decision writers (CaptureDecision / ExtractDecisions). Keeping it
// in one place is deliberate: the "never write a peer's changelog"
// coordination rule must not drift between the structured and extracted
// paths. Returns (id, 0) on success, or ("", 1) with a loud stderr line
// already emitted (every non-resolution is reported so the calling hook can
// key its loud path off the exit code — operator silent-failure principle).
// `who` prefixes the stderr lines so the operator sees which command failed.
func resolveCaptureTarget(s *store.Store, cfg *config.Config, explicitID, who string) (string, int) {
	id := strings.TrimSpace(explicitID)
	if id == "" {
		// Agent-scoped: stack top, then THIS agent's first active item.
		// Never a peer's (see resolveResumeTarget).
		id = resolveResumeTarget(s, cfg)
	}
	if id == "" {
		fmt.Fprintf(os.Stderr,
			"%s: no active/stack-top item to attribute this decision to — it was NOT recorded; push or start the relevant item so cross-session resume can capture forks.\n", who)
		return "", 1
	}
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: unknown item %q — decision NOT recorded.\n", who, id)
		return "", 1
	}
	// Explicit-ID peer guard. resolveResumeTarget already scopes the
	// fallback; an explicit id bypasses it, so enforce the same
	// coordination rule here. Only refuse when an agent identity resolves
	// AND the item is assigned to a *different* agent — an unassigned item
	// is nobody's claimed work (no peer to violate), and a no-identity
	// context has no peers, both consistent with the fallback resolver.
	if me := cfg.AgentID(); me != "" && item.AssignedTo != "" && item.AssignedTo != me {
		fmt.Fprintf(os.Stderr,
			"%s: %s is assigned to peer %q (you are %q) — decision NOT recorded; never write a peer's changelog.\n",
			who, id, item.AssignedTo, me)
		return "", 1
	}
	return id, 0
}

// CaptureDecision records a native-structured decision triggered from a
// PostToolUse hook.
//
// Item resolution: an explicit ID is used as given; otherwise it mirrors
// `st resume`'s "stack beats active" precedence via resolveResumeTarget
// (stack top, then — when an agent identity resolves — this agent's first
// active item; the guard is deliberately relaxed to the global first-active
// item only when no agent identity is resolvable, e.g. the as-CLI-only repo,
// where there are no peers to collide with). So the capture lands on
// whatever item the session is actually working — the same item `st resume`
// will replay next session.
//
// Cross-agent safety: this is a WRITE, and writing onto a peer's changelog
// is the coordination-rule violation ("never edit a peer's item") this whole
// path exists to prevent. The fallback resolver already skips peer items;
// the explicit-ID path is guarded here too so the "never a peer's" property
// holds on EVERY path, not just the fallback (an explicit --item naming a
// peer's in-flight item is refused, loudly, when an agent identity resolves).
//
// Returns a process exit code: 0 = captured, or a deliberate no-op (empty
// reason — genuinely nothing to record); 1 = NOT captured (no target item /
// unknown item / refused peer item / the changelog write itself failed). A
// failed write is a non-capture exactly like the others, so it returns 1
// too — the asymmetry of "unknown item is loud but a lost write is silent"
// is the precise operator-silent-failure trap this contract avoids; unlike
// the exec tape, the decision tape has no git-style self-attestation audit
// (Phase C adds an EXTRACTION backstop, not write-failure attestation), so
// a dropped write must be loud at the moment it happens. The *hook*
// always exits 0 regardless (a PostToolUse failure must never break the tool
// call that already ran) but keys its loud "decision NOT captured" line off
// this code, so every non-capture must be reported through it.
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

	id, rc := resolveCaptureTarget(s, cfg, opts.ID, "st capture-decision")
	if rc != 0 {
		return rc
	}

	if err := recordStructuredDecision(cfg, id, trigger, reason); err != nil {
		// recordStructuredDecision already emitted the never-silent
		// stderr warning; surface it as a non-capture so the hook's loud
		// path fires consistently with the other rc-1 cases above.
		return 1
	}
	return 0
}

// recordStructuredDecision appends the decision and returns the changelog
// write error (nil on success or on a deliberate empty-reason skip). It is
// the single Source=structured / Kind=decision write site for the whole
// feature (plan-approve, stack-push, and the PostToolUse hook). It is
// genuinely never silent: a failed write must not break the command that
// triggered it, but it must NOT vanish — the Phase A self-attestation audit
// checks only the exec/commit tape vs git, NOT decision entries, and Phase C
// adds an extraction backstop, not write-failure attestation, so a dropped
// decision write has no other backstop at all. The stderr warning here is
// that backstop for the in-`st` callers (plan/stack); CaptureDecision
// additionally turns the returned error into a loud non-capture exit code so
// the hook path is covered too.
func recordStructuredDecision(cfg *config.Config, id, trigger, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return nil
	}
	err := changelog.Append(cfg, id, changelog.Entry{
		Op:     "decision",
		Field:  trigger,
		Kind:   changelog.KindDecision,
		Source: changelog.SourceStructured,
		Reason: reason,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: failed to capture %s decision for %s (%v) — this rationale will NOT appear in `st resume`; re-state it via `st push --reason` or note it in the plan.\n",
			trigger, id, err)
	}
	return err
}
