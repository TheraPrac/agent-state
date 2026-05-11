// Package command — st decide is the operator handoff for the binary
// autonomy loop (T-346). When the classifier returns red, the agent
// pauses on status=awaiting_decision with a decision card attached;
// `st decide` is the single command the operator runs to resolve that
// pause.
package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/classify"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// DecideAction is the operator's verdict on a paused decision card.
type DecideAction string

const (
	DecideApprove DecideAction = "approve"
	DecideReject  DecideAction = "reject"
	DecideDefer   DecideAction = "defer"
)

// DecideOpts holds flags for the `st decide` command.
type DecideOpts struct {
	Action DecideAction
	Reason string
}

// AwaitingDecisionStatus is the canonical status name for the
// binary-autonomy pause state introduced in T-346.
const AwaitingDecisionStatus = "awaiting_decision"

// Decide resolves an item parked in `awaiting_decision`. Approve flips
// it back to active and resumes the loop. Reject closes it as
// abandoned (the "wontfix" outcome from the plan). Defer kicks it
// back to queued and clears the classification cache so the next
// `st classify` re-runs.
//
// Return codes:
//
//	0 — decision applied
//	1 — item not found, persist error, or unassigned/peer-agent guard tripped
//	2 — bad action, missing reason for reject, or item not in awaiting_decision
func Decide(s *store.Store, cfg *config.Config, id string, opts DecideOpts) int {
	if opts.Action != DecideApprove && opts.Action != DecideReject && opts.Action != DecideDefer {
		fmt.Fprintf(os.Stderr, "decide: unknown action %q — use approve|reject|defer\n", opts.Action)
		return 2
	}
	if opts.Action == DecideReject && strings.TrimSpace(opts.Reason) == "" {
		fmt.Fprintln(os.Stderr, "decide reject: --reason is required")
		return 2
	}

	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Status != AwaitingDecisionStatus {
		fmt.Fprintf(os.Stderr,
			"%s is %s, not %s — decide only acts on paused items\n",
			id, item.Status, AwaitingDecisionStatus)
		return 2
	}

	// T-346: refuse to decide on items owned by a peer agent. From
	// inside agent-c's workspace, an attempt to clear agent-a's pause
	// is almost always a mistake (cross-agent context confusion). The
	// guard is informational — set AS_AGENT_ID="" or run from the
	// workspace root to bypass.
	if currentAgent := cfg.AgentID(); currentAgent != "" && item.AssignedTo != "" && item.AssignedTo != currentAgent {
		fmt.Fprintf(os.Stderr,
			"%s is assigned to %s — refusing to decide from agent %s\n",
			id, item.AssignedTo, currentAgent)
		return 1
	}

	prevCard := readDecisionCard(item)

	var newStatus, opLabel, corpusAction string
	switch opts.Action {
	case DecideApprove:
		newStatus = "active"
		opLabel = "decide_approve"
		corpusAction = "approved"
	case DecideReject:
		newStatus = "abandoned"
		opLabel = "decide_reject"
		corpusAction = "rejected"
	case DecideDefer:
		newStatus = "queued"
		opLabel = "decide_defer"
		corpusAction = "deferred"
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("status", newStatus)
		it.Status = newStatus
		now := time.Now().UTC().Format(time.RFC3339)
		it.Doc.SetField("last_touched", now)
		if a := cfg.AgentID(); a != "" {
			it.Doc.SetField("last_touched_by", a)
		}
		// Defer clears the classification cache so the next `st classify`
		// re-runs the model with whatever the agent does next.
		if opts.Action == DecideDefer {
			it.Doc.RemoveNestedField("classification.verdict")
			it.Doc.RemoveNestedField("classification.reason")
			it.Doc.RemoveNestedField("classification.confidence")
			it.Doc.RemoveNestedField("classification.classified_at")
			it.Doc.RemoveNestedField("classification.classified_by")
			it.Doc.RemoveNestedField("classification.input_hash")
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "decide %s: %v\n", id, err)
		return 1
	}

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       opLabel,
		Field:    "status",
		OldValue: AwaitingDecisionStatus,
		NewValue: newStatus,
		Reason:   opts.Reason,
	})

	// Feedback corpus: the classifier consults the most recent
	// operator decisions as in-context examples on subsequent calls
	// (T-345 phase 2 already wires the read path). One entry per
	// `st decide` invocation.
	corpusPath := decisionCorpusPath(cfg)
	files := prevCard.FilesTouchedTop
	verdict := prevCard.ClassifierVerdict
	if verdict == "" {
		// Default verdict on the corpus entry: red, because the agent
		// only built a card on red. Cards built from green paths would
		// not have gone through `st decide`.
		verdict = classify.VerdictRed
	}
	if err := classify.AppendCorpus(corpusPath, classify.CorpusEntry{
		ItemID:         id,
		DecidedAt:      time.Now().UTC(),
		TouchedFiles:   files,
		Verdict:        verdict,
		OperatorAction: corpusAction,
		OperatorReason: opts.Reason,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: corpus append failed: %v\n", err)
	}

	fmt.Printf("Decided %s: %s → %s\n", id, opts.Action, newStatus)

	if err := s.GitSync(fmt.Sprintf("st decide %s: %s", id, opts.Action)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after decide failed: %v\n", err)
	}
	return 0
}

// readDecisionCard pulls the decision_card.* fields off the item and
// returns them as a parsed DecisionCard. Returns a zero card when no
// card is present (e.g. a manually-set awaiting_decision with no
// classifier history).
func readDecisionCard(item *model.Item) classify.DecisionCard {
	fields := map[string]string{}
	if item.Doc == nil {
		return classify.DecisionCard{}
	}
	for _, key := range []string{
		"risk_summary", "files_touched_count", "files_touched_top",
		"ask", "unblock_criteria",
		"classifier_verdict", "classifier_reason", "classifier_by", "classifier_hash",
		"classified_at", "card_built_at",
	} {
		if v, ok := item.Doc.GetNestedField("decision_card." + key); ok {
			fields[key] = v
		}
	}
	return classify.ParseDecisionCard(fields)
}

// WriteDecisionCard persists the card to the item under
// `decision_card.*` nested fields. Exposed for the classify gate
// integration (the loop builder calls this when a red verdict comes
// back). Idempotent — re-running with the same card overwrites in
// place rather than appending duplicate fields.
func WriteDecisionCard(s *store.Store, id string, card classify.DecisionCard) error {
	return s.Mutate(id, func(it *model.Item) error {
		for k, v := range card.AsNestedFields() {
			it.SetNested("decision_card", k, v)
		}
		return nil
	})
}

// FlipToAwaitingDecision is the canonical entry point for the binary
// autonomy loop's "halt on red" path: writes the decision card and
// flips status to awaiting_decision in a single Mutate. Callers
// (typically inside the delivery loop) construct the card via
// classify.BuildDecisionCard and pass it through here so the status
// flip and the card content land together — no half-written state if
// either step fails.
func FlipToAwaitingDecision(s *store.Store, cfg *config.Config, id string, card classify.DecisionCard) error {
	err := s.Mutate(id, func(it *model.Item) error {
		for k, v := range card.AsNestedFields() {
			it.SetNested("decision_card", k, v)
		}
		it.Doc.SetField("status", AwaitingDecisionStatus)
		it.Status = AwaitingDecisionStatus
		it.Doc.SetField("last_touched", time.Now().UTC().Format(time.RFC3339))
		if a := cfg.AgentID(); a != "" {
			it.Doc.SetField("last_touched_by", a)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "awaiting_decision",
		Field:    "status",
		NewValue: AwaitingDecisionStatus,
		Reason:   card.RiskSummary,
	})
	return nil
}

// decisionCorpusPath is where `st decide` invocations land. T-345's
// classifier reads from the same file on its next call, so operator
// decisions feed back into the model's in-context examples.
func decisionCorpusPath(cfg *config.Config) string {
	return filepath.Join(cfg.Root(), ".as", "classify-corpus.jsonl")
}
