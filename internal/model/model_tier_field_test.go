package model

import "testing"

func TestModelTierFieldCanonical(t *testing.T) {
	if !CanonicalTopLevelKeys["model_tier"] {
		t.Error("model_tier must be in CanonicalTopLevelKeys so st update can write it")
	}
}

func TestModelTierFieldRoundTrip(t *testing.T) {
	doc := &ParsedDocument{}
	doc.SetField("model_tier", "sonnet")
	got, ok := doc.GetField("model_tier")
	if !ok {
		t.Fatal("GetField(model_tier): not found after SetField")
	}
	if got != "sonnet" {
		t.Errorf("GetField(model_tier) = %q, want %q", got, "sonnet")
	}
}
