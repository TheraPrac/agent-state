package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
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
