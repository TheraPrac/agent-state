// Package quality holds content-depth checks the st CLI runs at item
// create time and at plan-approval time. The first rule set lands
// SBAR substance validation per I-149 — the gate that prevents the
// I-487 SBAR scaffold (TODO placeholders) from carrying through to
// plan approval unfilled.
//
// Future rule sets (item title/summary thresholds, plan-quality
// rules beyond I-511's AC verifiability check) can be added as
// additional entry points without disturbing the existing surface.
package quality

import (
	"fmt"
	"io"
	"strings"

	"github.com/jfinlinson/agent-state/internal/model"
)

// Severity classifies a Violation. errors block the gate they are
// surfaced at; warnings print but allow the operation to continue.
type Severity int

const (
	SeverityWarn Severity = iota
	SeverityError
)

// String returns a human-readable label suitable for stderr output.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	default:
		return "warning"
	}
}

// Violation describes a single content-quality finding.
type Violation struct {
	Severity Severity
	Field    string // dotted path, e.g. "sbar.situation"
	Message  string
}

// String formats the finding as "<severity>: <field> — <message>".
func (v Violation) String() string {
	return v.Severity.String() + ": " + v.Field + " — " + v.Message
}

// ValidateSBAR reports a Violation per SBAR sub-field that is empty
// or still equal to its TODO placeholder. All four sub-fields are
// checked. Returns nil when the item is fully populated.
//
// SBAR is only required on tasks and issues per the I-487 schema —
// ideas and promotions never carry SBAR, so callers should
// short-circuit on those types before calling here. The validator
// itself is type-agnostic; it just checks whatever SBAR struct it
// is handed.
//
// This is the I-149 substance gate. I-487 made SBAR a required
// composite field; I-492 / I-493 added scaffold + editor flows;
// I-149 closes the loop by surfacing items where the scaffold was
// never replaced with real content.
func ValidateSBAR(item *model.Item) []Violation {
	var out []Violation
	for _, sec := range []struct {
		key, body string
	}{
		{"situation", item.SBAR.Situation},
		{"background", item.SBAR.Background},
		{"assessment", item.SBAR.Assessment},
		{"recommendation", item.SBAR.Recommendation},
	} {
		body := strings.TrimSpace(sec.body)
		if body == "" {
			out = append(out, Violation{
				Severity: SeverityError,
				Field:    "sbar." + sec.key,
				Message:  "section is empty — fill via `st update " + item.ID + " sbar`",
			})
			continue
		}
		if body == model.SBARPlaceholders[sec.key] {
			out = append(out, Violation{
				Severity: SeverityError,
				Field:    "sbar." + sec.key,
				Message:  "section still contains the TODO scaffold — replace with real content",
			})
		}
	}
	return out
}

// PrintWarnings emits each violation as a "warning:" line under the
// supplied header. Used by callers (e.g. `st create`, non-strict
// `st plan approve`) that surface the violations as informational
// nudges rather than blocking errors. Without this helper the
// caller would have to choose between printing the raw violation
// (which leads with "error:" — confusing when the command exits 0)
// or duplicating the formatting logic.
func PrintWarnings(w io.Writer, header string, vs []Violation) {
	if len(vs) == 0 {
		return
	}
	if header != "" {
		fmt.Fprintln(w, header)
	}
	for _, v := range vs {
		fmt.Fprintf(w, "  warning: %s — %s\n", v.Field, v.Message)
	}
}

// HasError reports whether any Violation in the slice has
// SeverityError. Useful at gate sites that warn-only by default and
// hard-block under a `--strict` flag.
func HasError(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity == SeverityError {
			return true
		}
	}
	return false
}
