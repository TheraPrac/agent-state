package command

import (
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
)

// recordStructuredDecision appends a native-structured decision entry
// (I-679 Phase B): captured verbatim as an immutable side-effect of an
// `st` command that, by construction, represents a settled fork — a plan
// approval, or a stack push whose --reason states why other work was
// interrupted. source=structured marks it authoritative; the Phase C
// extraction backstop reconciles against these and can never clobber one.
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
func recordStructuredDecision(cfg *config.Config, id, trigger, reason string) {
	if strings.TrimSpace(reason) == "" {
		return
	}
	// Best-effort + never silent: a failed decision write must not break
	// the command that triggered it, but it also must not vanish — the
	// Phase A self-attestation audit (st resume) is what surfaces a
	// capture gap, so a dropped write degrades to a visible gap there
	// rather than a silent loss here.
	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:     "decision",
		Field:  trigger,
		Kind:   changelog.KindDecision,
		Source: changelog.SourceStructured,
		Reason: reason,
	})
}
