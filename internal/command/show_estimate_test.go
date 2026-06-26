package command

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// TestShowEstimateVsActual verifies that renderTimeTracking emits estimate/actual
// when estimated_hours and total_duration_seconds are both set.
func TestShowEstimateVsActual(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Write estimated_hours=2.0 and total_duration_seconds=5400 (1.5h) onto T-001.
	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.SetNested("time_tracking", "estimated_hours", "2.00")
		item.SetNested("time_tracking", "total_duration_seconds", "5400")
		item.SetNested("time_tracking", "turn_count", "1")
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	item, _ := s.Get("T-001")
	out := captureStdout(t, func() {
		Show(s, cfg, "T-001", ShowOpts{})
	})

	if !strings.Contains(out, "estimated:") {
		t.Errorf("show output should contain 'estimated:', got:\n%s", out)
	}
	if !strings.Contains(out, "actual:") {
		t.Errorf("show output should contain 'actual:', got:\n%s", out)
	}
	// 5400s = 1.5h out of 2.0h estimate = 75%
	if !strings.Contains(out, "75%") {
		t.Errorf("show output should contain '75%%' (actual/estimate ratio), got:\n%s", out)
	}
	_ = item // used above via Mutate
}

// TestShowEstimateOnly verifies that 'estimated' is shown without 'actual' when no
// total_duration_seconds is available.
func TestShowEstimateOnly(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.SetNested("time_tracking", "estimated_hours", "3.00")
		item.SetNested("time_tracking", "turn_count", "1")
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	out := captureStdout(t, func() {
		Show(s, cfg, "T-001", ShowOpts{})
	})

	if !strings.Contains(out, "estimated:") {
		t.Errorf("show output should contain 'estimated:', got:\n%s", out)
	}
	if strings.Contains(out, "actual:") {
		t.Errorf("show output should NOT contain 'actual:' (no duration), got:\n%s", out)
	}
}
