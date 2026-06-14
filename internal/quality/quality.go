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
	"github.com/jfinlinson/agent-state/internal/plan"
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

// ValidateSBARLength reports a Violation for each SBAR sub-field whose
// trimmed content is below the per-field minimum character floor.
// Run after ValidateSBAR (empty/placeholder check) so this only fires
// on fields with real content that is still too thin to be actionable.
// Floors: situation ≥20, background ≥40, assessment ≥30, recommendation ≥30.
//
// Empty fields are skipped intentionally — ValidateSBAR already catches
// those with a more specific message. This function only fires when there
// is content present but it is insufficiently detailed.
//
// I-908.
func ValidateSBARLength(item *model.Item) []Violation {
	var out []Violation
	for _, f := range []struct {
		key, body string
		min       int
	}{
		{"situation", item.SBAR.Situation, 20},
		{"background", item.SBAR.Background, 40},
		{"assessment", item.SBAR.Assessment, 30},
		{"recommendation", item.SBAR.Recommendation, 30},
	} {
		body := strings.TrimSpace(f.body)
		if body == "" {
			continue // ValidateSBAR handles empty; skip here to avoid double-reporting
		}
		if len(body) < f.min {
			out = append(out, Violation{
				Severity: SeverityError,
				Field:    "sbar." + f.key,
				Message:  fmt.Sprintf("too short (%d chars, minimum %d) — add specifics (file paths, function names, behavioral descriptions)", len(body), f.min),
			})
		}
	}
	return out
}

// scaffoldApproach values that should NOT pass as a real plan
// approach. Match is whole-string case-insensitive after TrimSpace.
var scaffoldApproach = map[string]bool{
	"todo":  true,
	"tbd":   true,
	"n/a":   true,
	"none":  true,
	"":      true,
}

// ValidatePlan reports Violations against the substance of a plan
// sidecar. Rules:
//   - empty or scaffold Approach (TODO/TBD/N/A/None or empty) → error
//   - empty ScopeRepos → error
//   - any plan.ValidateACs finding → error (one per finding)
//
// I-710 — the analog of ValidateSBAR for plan sidecars. Reuses
// plan.ValidateACs (I-511) as the single source of truth for AC
// verifiability rules; this function bundles AC findings into the
// Violation shape so quality consumers (PlanApprove, PlanCheck) get
// one uniform surface.
func ValidatePlan(p *plan.Plan) []Violation {
	var out []Violation
	if p == nil {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan",
			Message:  "plan sidecar missing — write .plans/<id>.md before approving",
		})
		return out
	}
	approach := strings.TrimSpace(p.Approach)
	if scaffoldApproach[strings.ToLower(approach)] {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan.approach",
			Message:  "approach is empty or scaffold (TODO/TBD/N/A/None) — describe the technical approach in one or two paragraphs",
		})
	}
	if len(p.ScopeRepos) == 0 {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan.scope_repos",
			Message:  "scope_repos is empty — list every repo this plan touches (as, theraprac-api, theraprac-web, theraprac-infra) in the frontmatter or `## Scope` section",
		})
	}
	// T-394: require ## Tests, ## Out-of-scope, and ## Risks sections.
	tests := strings.TrimSpace(p.Tests)
	if scaffoldApproach[strings.ToLower(tests)] {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan.tests",
			Message:  "## Tests section is missing or empty — describe what tests cover this work (unit, integration, e2e, or 'None — existing coverage sufficient: <reason>')",
		})
	}
	outOfScope := strings.TrimSpace(p.OutOfScope)
	if outOfScope == "" {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan.out_of_scope",
			Message:  "## Out-of-scope section is missing — list out-of-scope items as linked issues (I-XXX: description), or write 'None' to explicitly acknowledge no out-of-scope work",
		})
	}
	risks := strings.TrimSpace(p.Risks)
	if scaffoldApproach[strings.ToLower(risks)] {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    "plan.risks",
			Message:  "## Risks section is missing or empty — list risks and mitigations, or write 'None — low-risk change: <reason>'",
		})
	}
	out = append(out, ValidateACList(p.ACs)...)
	return out
}

// ValidateACList wraps plan.ValidateACs (I-511) and returns its
// findings in the Violation shape used elsewhere in this package.
// Centralizes the public Violation surface so callers outside the
// plan package (notably command.Update for I-713) can validate AC
// lists without importing the plan package directly.
//
// I-710.
func ValidateACList(acs []string) []Violation {
	var out []Violation
	for _, f := range plan.ValidateACs(acs) {
		out = append(out, Violation{
			Severity: SeverityError,
			Field:    fmt.Sprintf("acceptance_criteria[%d]", f.Index),
			Message:  f.Reason + ": " + fmt.Sprintf("%q", f.AC),
		})
	}
	return out
}
