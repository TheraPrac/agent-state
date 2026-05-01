package quality

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

func TestValidateSBAR_AllPopulated_NoViolations(t *testing.T) {
	item := &model.Item{
		ID: "I-001",
		SBAR: model.SBAR{
			Situation:      "fresh signups fail",
			Background:     "RLS context not set on pool conn",
			Assessment:     "reproduces 100%",
			Recommendation: "switch to s.querier(ctx)",
		},
	}
	if v := ValidateSBAR(item); len(v) != 0 {
		t.Errorf("expected no violations, got %+v", v)
	}
}

func TestValidateSBAR_AllEmpty_FourViolations(t *testing.T) {
	item := &model.Item{ID: "I-002"}
	v := ValidateSBAR(item)
	if len(v) != 4 {
		t.Fatalf("expected 4 violations, got %d: %+v", len(v), v)
	}
	for _, vio := range v {
		if vio.Severity != SeverityError {
			t.Errorf("empty section should be error, got %+v", vio)
		}
		if !strings.HasPrefix(vio.Field, "sbar.") {
			t.Errorf("violation field should be sbar.*, got %q", vio.Field)
		}
		if !strings.Contains(vio.Message, "I-002") {
			t.Errorf("message should hint at the item id, got %q", vio.Message)
		}
	}
}

func TestValidateSBAR_AllScaffold_FourViolations(t *testing.T) {
	item := &model.Item{
		ID: "I-003",
		SBAR: model.SBAR{
			Situation:      model.SBARPlaceholders["situation"],
			Background:     model.SBARPlaceholders["background"],
			Assessment:     model.SBARPlaceholders["assessment"],
			Recommendation: model.SBARPlaceholders["recommendation"],
		},
	}
	v := ValidateSBAR(item)
	if len(v) != 4 {
		t.Fatalf("expected 4 violations on unmodified scaffold, got %d", len(v))
	}
	for _, vio := range v {
		if !strings.Contains(vio.Message, "TODO scaffold") {
			t.Errorf("scaffold message expected, got %q", vio.Message)
		}
	}
}

func TestValidateSBAR_PartialFill_FlagsOnlyMissing(t *testing.T) {
	item := &model.Item{
		ID: "I-004",
		SBAR: model.SBAR{
			Situation:      "real",
			Background:     "real",
			Assessment:     model.SBARPlaceholders["assessment"],
			Recommendation: "",
		},
	}
	v := ValidateSBAR(item)
	if len(v) != 2 {
		t.Fatalf("expected 2 violations (assessment scaffold + recommendation empty), got %d: %+v", len(v), v)
	}
	got := map[string]bool{}
	for _, vio := range v {
		got[vio.Field] = true
	}
	if !got["sbar.assessment"] {
		t.Error("assessment scaffold should fire")
	}
	if !got["sbar.recommendation"] {
		t.Error("recommendation empty should fire")
	}
}

// Whitespace-only counts as empty — the TrimSpace check matches the
// I-495 prime-output guard pattern, so a `" "` body is treated the
// same as "" everywhere in the codebase.
func TestValidateSBAR_WhitespaceOnly_TreatedAsEmpty(t *testing.T) {
	item := &model.Item{
		ID: "I-005",
		SBAR: model.SBAR{
			Situation:      " ",
			Background:     "\n\n",
			Assessment:     "\t",
			Recommendation: "real",
		},
	}
	v := ValidateSBAR(item)
	if len(v) != 3 {
		t.Fatalf("expected 3 violations, got %d", len(v))
	}
	for _, vio := range v {
		if vio.Field == "sbar.recommendation" {
			t.Errorf("populated recommendation should not flag, got %+v", vio)
		}
	}
}

func TestHasError(t *testing.T) {
	if HasError(nil) {
		t.Error("nil slice should not have errors")
	}
	if HasError([]Violation{{Severity: SeverityWarn}}) {
		t.Error("warn-only slice should not have errors")
	}
	if !HasError([]Violation{{Severity: SeverityWarn}, {Severity: SeverityError}}) {
		t.Error("mixed slice with one error should report true")
	}
}
