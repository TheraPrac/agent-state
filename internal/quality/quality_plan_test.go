package quality

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/plan"
)

func TestValidatePlan_PassesOnPopulated(t *testing.T) {
	p := &plan.Plan{
		Approach:   "Real approach paragraph describing the work.",
		ScopeRepos: []string{"as"},
		ACs: []string{
			"cmd: go test ./...",
			"cmd: go vet ./...",
		},
	}
	if v := ValidatePlan(p); len(v) != 0 {
		t.Errorf("expected no violations on populated plan, got %d: %v", len(v), v)
	}
}

func TestValidatePlan_FlagsEmptyApproach(t *testing.T) {
	p := &plan.Plan{
		Approach:   "",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}
	v := ValidatePlan(p)
	if !HasError(v) {
		t.Fatalf("expected error on empty approach; got %v", v)
	}
	found := false
	for _, vi := range v {
		if vi.Field == "plan.approach" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected violation on plan.approach; got %v", v)
	}
}

func TestValidatePlan_FlagsScaffoldApproach(t *testing.T) {
	for _, scaffold := range []string{"TODO", "TBD", "N/A", "none", "  todo  "} {
		t.Run(scaffold, func(t *testing.T) {
			p := &plan.Plan{
				Approach:   scaffold,
				ScopeRepos: []string{"as"},
				ACs:        []string{"cmd: go test ./..."},
			}
			v := ValidatePlan(p)
			if !HasError(v) {
				t.Errorf("expected error on scaffold approach %q; got %v", scaffold, v)
			}
		})
	}
}

func TestValidatePlan_FlagsACViolations(t *testing.T) {
	p := &plan.Plan{
		Approach:   "Real approach.",
		ScopeRepos: []string{"as"},
		ACs: []string{
			"the feature works", // vague
			"cmd: go test ./...", // verifiable
			"passes review",      // vague
		},
	}
	v := ValidatePlan(p)
	if !HasError(v) {
		t.Fatalf("expected errors on AC violations; got %v", v)
	}
	acViolations := 0
	for _, vi := range v {
		if strings.HasPrefix(vi.Field, "acceptance_criteria[") {
			acViolations++
		}
	}
	if acViolations != 2 {
		t.Errorf("expected 2 AC violations; got %d (all violations: %v)", acViolations, v)
	}
}

func TestValidatePlan_FlagsEmptyScopeRepos(t *testing.T) {
	p := &plan.Plan{
		Approach:   "Real approach.",
		ScopeRepos: nil,
		ACs:        []string{"cmd: go test ./..."},
	}
	v := ValidatePlan(p)
	if !HasError(v) {
		t.Fatalf("expected error on empty scope_repos; got %v", v)
	}
	found := false
	for _, vi := range v {
		if vi.Field == "plan.scope_repos" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected violation on plan.scope_repos; got %v", v)
	}
}

func TestValidatePlan_NilPlanReturnsViolation(t *testing.T) {
	v := ValidatePlan(nil)
	if !HasError(v) {
		t.Fatalf("expected error on nil plan; got %v", v)
	}
	if v[0].Field != "plan" {
		t.Errorf("expected violation on 'plan' field; got %q", v[0].Field)
	}
}

func TestValidateACList_DelegatesToPlanValidator(t *testing.T) {
	// Empty list → no violations (matches plan.ValidateACs semantics).
	if v := ValidateACList(nil); len(v) != 0 {
		t.Errorf("expected no violations on nil AC list; got %v", v)
	}
	// Verifiable AC → no violations.
	if v := ValidateACList([]string{"cmd: go test ./..."}); len(v) != 0 {
		t.Errorf("expected no violations on cmd: AC; got %v", v)
	}
	// Vague AC → one violation.
	v := ValidateACList([]string{"the feature works"})
	if len(v) != 1 {
		t.Fatalf("expected 1 violation on vague AC; got %d: %v", len(v), v)
	}
	if !strings.HasPrefix(v[0].Field, "acceptance_criteria[") {
		t.Errorf("expected field acceptance_criteria[i]; got %q", v[0].Field)
	}
	if v[0].Severity != SeverityError {
		t.Errorf("expected SeverityError; got %v", v[0].Severity)
	}
}
