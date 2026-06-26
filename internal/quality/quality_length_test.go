package quality

import (
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// TestValidateSBARLength_PassesLongEnough verifies that SBAR content meeting
// all per-field minimums produces zero violations.
func TestValidateSBARLength_PassesLongEnough(t *testing.T) {
	item := &model.Item{
		SBAR: model.SBAR{
			Situation:      "Fresh signups fail on POST /patients with 500 error.",
			Background:     "RLS context not set on pool connection; internal/db/pool.go SetContext() not called before query.",
			Assessment:     "Query runs as `app` user without tenant context, causing RLS to reject all rows.",
			Recommendation: "Call SetContext(ctx, tenantID) in internal/db/pool.go before every query execution.",
		},
	}
	if vs := ValidateSBARLength(item); len(vs) != 0 {
		t.Errorf("expected no violations for long-enough SBAR, got %+v", vs)
	}
}

// TestValidateSBARLength_FailsShortSituation verifies that a situation field
// under the 20-char minimum triggers an error violation.
func TestValidateSBARLength_FailsShortSituation(t *testing.T) {
	item := &model.Item{
		SBAR: model.SBAR{
			Situation:      "Bug in login.",    // 13 chars, under 20
			Background:     "RLS context not set on pool connection; internal/db/pool.go SetContext() missing.",
			Assessment:     "Query runs without tenant context, RLS blocks all rows.",
			Recommendation: "Call SetContext(ctx, tenantID) before query in pool.go.",
		},
	}
	vs := ValidateSBARLength(item)
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation for short situation, got %d: %+v", len(vs), vs)
	}
	if vs[0].Field != "sbar.situation" {
		t.Errorf("expected field sbar.situation, got %q", vs[0].Field)
	}
	if vs[0].Severity != SeverityError {
		t.Errorf("expected SeverityError, got %v", vs[0].Severity)
	}
}

// TestValidateSBARLength_FailsAllShort verifies that all four fields below
// their minimums each produce an error violation.
func TestValidateSBARLength_FailsAllShort(t *testing.T) {
	item := &model.Item{
		SBAR: model.SBAR{
			Situation:      "short",        // 5 chars, under 20
			Background:     "also short",   // 10 chars, under 40
			Assessment:     "too vague",    // 9 chars, under 30
			Recommendation: "fix it",       // 6 chars, under 30
		},
	}
	vs := ValidateSBARLength(item)
	if len(vs) != 4 {
		t.Fatalf("expected 4 violations for all-short SBAR, got %d: %+v", len(vs), vs)
	}
	fields := map[string]bool{}
	for _, v := range vs {
		fields[v.Field] = true
		if v.Severity != SeverityError {
			t.Errorf("expected SeverityError for %s, got %v", v.Field, v.Severity)
		}
	}
	for _, f := range []string{"sbar.situation", "sbar.background", "sbar.assessment", "sbar.recommendation"} {
		if !fields[f] {
			t.Errorf("expected violation for %s", f)
		}
	}
}

// TestValidateSBARLength_SkipsEmptyFields verifies that empty fields do not
// produce length violations — ValidateSBAR catches those, not ValidateSBARLength.
func TestValidateSBARLength_SkipsEmptyFields(t *testing.T) {
	item := &model.Item{
		SBAR: model.SBAR{
			Situation:      "",
			Background:     "",
			Assessment:     "",
			Recommendation: "",
		},
	}
	vs := ValidateSBARLength(item)
	if len(vs) != 0 {
		t.Errorf("expected no length violations for empty fields (ValidateSBAR handles empty), got %+v", vs)
	}
}

// TestValidateSBARLength_ExactlyAtMinimum verifies that content at exactly
// the minimum character floor passes (boundary inclusive).
func TestValidateSBARLength_ExactlyAtMinimum(t *testing.T) {
	item := &model.Item{
		SBAR: model.SBAR{
			Situation:      "20 chars exactly here", // exactly 21 chars — over 20
			Background:     "Exactly forty characters long for background.",  // ≥40
			Assessment:     "Exactly thirty chars for assess.",               // ≥30
			Recommendation: "Exactly thirty chars for recomm.",               // ≥30
		},
	}
	vs := ValidateSBARLength(item)
	if len(vs) != 0 {
		t.Errorf("expected no violations at/above minimum floors, got %+v", vs)
	}
}
