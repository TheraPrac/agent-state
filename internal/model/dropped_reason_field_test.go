package model

import "testing"

func TestDroppedReasonFieldCanonical(t *testing.T) {
	if !CanonicalTopLevelKeys["dropped_reason"] {
		t.Error("dropped_reason must be in CanonicalTopLevelKeys")
	}
}
