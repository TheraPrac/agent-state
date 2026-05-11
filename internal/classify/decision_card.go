package classify

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// DecisionCard is the structured artifact built when the classifier
// returns red and the binary autonomy loop pauses for an operator
// decision. It mirrors what the operator needs to read at handoff:
// what the classifier saw, what's at risk, what the agent wants to do
// next, and what would change the verdict.
//
// The card is persisted as nested fields on the item under
// `decision_card.*` so it round-trips through the YAML serializer
// without a custom schema layer.
type DecisionCard struct {
	RiskSummary       string
	FilesTouchedCount int
	FilesTouchedTop   []string // up to FilesTouchedTopLimit entries
	Ask               string
	UnblockCriteria   string

	ClassifierVerdict   Verdict
	ClassifierReason    string
	ClassifierBy        string
	ClassifiedAt        time.Time
	ClassifierInputHash string

	CardBuiltAt time.Time
}

// FilesTouchedTopLimit caps the number of file paths stored on the
// card body. The full file list is still available via the manifest;
// the card carries only the top-N so a busy diff doesn't blow up the
// card render.
const FilesTouchedTopLimit = 8

// BuildDecisionCard constructs a card from a classifier result and the
// touched-files list. `ask` is supplied by the caller because the
// natural-language ask varies by where in the delivery loop the pause
// fired ("approve to merge", "approve to push", etc.).
//
// `now` is injected for tests; pass time.Now().UTC() in production.
func BuildDecisionCard(res Result, files []string, ask string, now time.Time) DecisionCard {
	top := append([]string(nil), files...)
	sort.Strings(top)
	if len(top) > FilesTouchedTopLimit {
		top = top[:FilesTouchedTopLimit]
	}
	return DecisionCard{
		RiskSummary:         res.Reason,
		FilesTouchedCount:   len(files),
		FilesTouchedTop:     top,
		Ask:                 ask,
		ClassifierVerdict:   res.Verdict,
		ClassifierReason:    res.Reason,
		ClassifierBy:        res.ClassifiedBy,
		ClassifiedAt:        res.ClassifiedAt,
		ClassifierInputHash: res.InputHash,
		CardBuiltAt:         now,
	}
}

// AsNestedFields renders the card as a flat key→value map ready to
// hand to the item-storage layer (each entry becomes a
// `decision_card.<key>: <value>` line in the item file). All values
// are strings so the existing nested-scalar writer handles them
// without schema changes.
//
// FilesTouchedTop is rendered as a comma-joined string under
// `files_touched_top`; readers should split on ", " to recover the
// list. This keeps the on-disk shape simple and round-trippable.
func (c DecisionCard) AsNestedFields() map[string]string {
	out := map[string]string{
		"risk_summary":        c.RiskSummary,
		"files_touched_count": strconv.Itoa(c.FilesTouchedCount),
		"files_touched_top":   strings.Join(c.FilesTouchedTop, ", "),
		"ask":                 c.Ask,
		"unblock_criteria":    c.UnblockCriteria,
		"classifier_verdict":  string(c.ClassifierVerdict),
		"classifier_reason":   c.ClassifierReason,
		"classifier_by":       c.ClassifierBy,
		"classifier_hash":     c.ClassifierInputHash,
	}
	if !c.ClassifiedAt.IsZero() {
		out["classified_at"] = c.ClassifiedAt.Format(time.RFC3339)
	}
	if !c.CardBuiltAt.IsZero() {
		out["card_built_at"] = c.CardBuiltAt.Format(time.RFC3339)
	}
	return out
}

// ParseDecisionCard reads a card back from the flat nested-field map
// (the inverse of AsNestedFields). Returns a zero DecisionCard if the
// map carries no card fields. Missing fields stay at their zero value
// rather than failing — a partial card (e.g. one from an older schema)
// still renders.
func ParseDecisionCard(fields map[string]string) DecisionCard {
	c := DecisionCard{
		RiskSummary:         fields["risk_summary"],
		Ask:                 fields["ask"],
		UnblockCriteria:     fields["unblock_criteria"],
		ClassifierVerdict:   Verdict(fields["classifier_verdict"]),
		ClassifierReason:    fields["classifier_reason"],
		ClassifierBy:        fields["classifier_by"],
		ClassifierInputHash: fields["classifier_hash"],
	}
	if v := fields["files_touched_count"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.FilesTouchedCount = n
		}
	}
	if v := fields["files_touched_top"]; v != "" {
		for _, p := range strings.Split(v, ", ") {
			if p = strings.TrimSpace(p); p != "" {
				c.FilesTouchedTop = append(c.FilesTouchedTop, p)
			}
		}
	}
	if v := fields["classified_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			c.ClassifiedAt = t
		}
	}
	if v := fields["card_built_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			c.CardBuiltAt = t
		}
	}
	return c
}

// IsEmpty reports whether no card fields are populated — the signal
// that an item has never had a decision card built.
func (c DecisionCard) IsEmpty() bool {
	return c.ClassifierVerdict == "" && c.RiskSummary == "" && c.Ask == "" && c.CardBuiltAt.IsZero()
}
