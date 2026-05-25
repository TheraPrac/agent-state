package model

import (
	"slices"
	"testing"
)

func TestDropReasonsVocabulary(t *testing.T) {
	// Exact set — order matters for deterministic error messages.
	want := []string{
		"superseded",
		"premise-invalid",
		"out-of-strategy",
		"duplicate",
		"unactionable",
	}
	if len(ValidDropReasons) != len(want) {
		t.Fatalf("ValidDropReasons len = %d, want %d: %v", len(ValidDropReasons), len(want), ValidDropReasons)
	}
	for i, r := range want {
		if ValidDropReasons[i] != r {
			t.Errorf("ValidDropReasons[%d] = %q, want %q", i, ValidDropReasons[i], r)
		}
	}

	// "aged" must never appear.
	if slices.Contains(ValidDropReasons, "aged") {
		t.Error("ValidDropReasons must not contain 'aged'")
	}
}

func TestIsValidDropReason(t *testing.T) {
	for _, r := range ValidDropReasons {
		if !IsValidDropReason(r) {
			t.Errorf("IsValidDropReason(%q) = false, want true", r)
		}
	}
	for _, bad := range []string{"aged", "no longer needed", "", "SUPERSEDED", "out_of_strategy"} {
		if IsValidDropReason(bad) {
			t.Errorf("IsValidDropReason(%q) = true, want false", bad)
		}
	}
}
