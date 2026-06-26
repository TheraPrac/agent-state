package quality_test

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/quality"
)

func item(background string) *model.Item {
	return &model.Item{SBAR: model.SBAR{Background: background}}
}

func TestValidateBackgroundEvidenceClaims(t *testing.T) {
	t.Run("clean_background_passes", func(t *testing.T) {
		bg := "The hook was added in I-759. It guards against direct writes to st-owned paths."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations, got %d: %v", len(vs), vs)
		}
	})

	t.Run("empty_background_passes", func(t *testing.T) {
		vs := quality.ValidateBackgroundEvidenceClaims(item(""))
		if len(vs) != 0 {
			t.Fatalf("expected no violations, got %v", vs)
		}
	})

	t.Run("observation_without_evidence_blocked", func(t *testing.T) {
		// Mirrors I-755 SBAR: "Final persisted claim state on demo..."
		bg := "Final persisted claim state on demo 2026-05-20 shows six missing columns."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) == 0 {
			t.Fatal("expected a violation for empirical claim without evidence, got none")
		}
		if vs[0].Field != "sbar.background" {
			t.Errorf("wrong field: %s", vs[0].Field)
		}
	})

	t.Run("hypothesis_marker_exempts_sentence", func(t *testing.T) {
		// [hypothesis] prefix marks the sentence as speculative
		bg := "Likely the final persisted claim state was not saved correctly."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations for hypothesis-marked sentence, got %v", vs)
		}
	})

	t.Run("evidence_pointer_in_background_exempts", func(t *testing.T) {
		// A URL anywhere in the background grounds the whole section
		bg := "Final persisted claim state on demo shows six missing columns. " +
			"Source: https://github.com/TheraPrac/theraprac-api/pull/123"
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations when evidence pointer present, got %v", vs)
		}
	})

	t.Run("uuid_as_evidence_pointer", func(t *testing.T) {
		// A UUID grounds a specific row observation
		bg := "Final persisted claim state on demo: ea94525e-54ce-11f1-a4d6-336a9172a5ed shows six missing columns."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations when UUID present, got %v", vs)
		}
	})

	t.Run("db_read_as_evidence_pointer", func(t *testing.T) {
		bg := "Final persisted claim state was wrong. Direct DB read confirms zero rows in insurance_claims."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations when DB read present, got %v", vs)
		}
	})

	t.Run("round_trip_closed_blocked", func(t *testing.T) {
		bg := "The round-trip closed end-to-end on demo 2026-05-20."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) == 0 {
			t.Fatal("expected violation for round-trip claim without evidence")
		}
	})

	t.Run("round_trip_closed_with_evidence_passes", func(t *testing.T) {
		bg := "The round-trip closed end-to-end on demo 2026-05-20. Test run #4721 in CI."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations with test run cited, got %v", vs)
		}
	})

	// I-755 regression fixture: the actual background that caused the incident.
	t.Run("I755_regression_fixture", func(t *testing.T) {
		bg := strings.TrimSpace(`
Forcing function: I-715.

I-715 stated that the claim round-trip closed end-to-end on demo 2026-05-20.
Final persisted claim state on demo 2026-05-20: six columns labeled correctly.
Likely causes: MarkClaimSubmitted / MarkClaimAcceptedByID SQL missing updates.
`)
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) == 0 {
			t.Fatal("I-755 regression: expected violations for uncited observations, got none")
		}
	})

	t.Run("gate_firing_observation_blocked", func(t *testing.T) {
		bg := "The gate is firing in dev — confirmed by the operator."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) == 0 {
			t.Fatal("expected violation for 'the gate is firing' without evidence")
		}
	})

	t.Run("gate_firing_with_log_evidence_passes", func(t *testing.T) {
		bg := "The gate is firing in dev — confirmed by the operator. Log output: 2026-06-15 hook denied write."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) != 0 {
			t.Fatalf("expected no violations with log evidence, got %v", vs)
		}
	})

	t.Run("uuid_in_different_sentence_does_not_ground_section", func(t *testing.T) {
		// A UUID that is merely a tenant/row reference in a separate sentence
		// does not ground empirical claims elsewhere in the section.
		bg := "Tenant ea94525e-54ce-11f1-a4d6-336a9172a5ed had a sync issue. " +
			"Final persisted claim state was wrong."
		vs := quality.ValidateBackgroundEvidenceClaims(item(bg))
		if len(vs) == 0 {
			t.Fatal("expected violation: UUID in different sentence should not ground empirical claim")
		}
	})
}
