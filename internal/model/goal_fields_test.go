package model

import "testing"

func TestGoalFieldsCanonical(t *testing.T) {
	if !CanonicalTopLevelKeys["weight"] {
		t.Error("CanonicalTopLevelKeys missing weight")
	}
	if !CanonicalTopLevelKeys["success_criterion"] {
		t.Error("CanonicalTopLevelKeys missing success_criterion")
	}
}

func TestGoalItemHasWeightAndSuccessCriterion(t *testing.T) {
	w := 40
	it := &Item{
		ID:               "G-001",
		Type:             "goal",
		Status:           "active",
		Title:            "Alpha Go-Live",
		Weight:           &w,
		SuccessCriterion: "all must_do items complete",
	}
	if *it.Weight != 40 {
		t.Errorf("Weight = %d, want 40", *it.Weight)
	}
	if it.SuccessCriterion != "all must_do items complete" {
		t.Errorf("SuccessCriterion = %q", it.SuccessCriterion)
	}
}
