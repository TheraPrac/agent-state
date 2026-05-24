package model

import "testing"

func TestGoalsFieldCanonical(t *testing.T) {
	if !CanonicalTopLevelKeys["goals"] {
		t.Error(`CanonicalTopLevelKeys["goals"] = false, want true`)
	}
}
