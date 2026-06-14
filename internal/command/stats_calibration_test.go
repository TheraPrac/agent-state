package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

// TestStatsEstimatesCalibration verifies that st stats --time surfaces estimate
// calibration data for closed items with both estimated_hours and total_duration_seconds.
func TestStatsEstimatesCalibration(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Mark T-001 done with estimated 2h and actual 1.5h (5400s).
	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.Status = "done"
		item.Doc.SetField("status", "done")
		item.SetNested("time_tracking", "estimated_hours", "2.00")
		item.SetNested("time_tracking", "total_duration_seconds", "5400")
		item.SetNested("time_tracking", "turn_count", "3")
		return nil
	}); err != nil {
		t.Fatalf("Mutate T-001: %v", err)
	}

	ts := computeTimeStats(s)
	if ts.EstimatedItems != 1 {
		t.Errorf("EstimatedItems = %d, want 1", ts.EstimatedItems)
	}
	if ts.TotalEstimatedHrs != 2.0 {
		t.Errorf("TotalEstimatedHrs = %.2f, want 2.00", ts.TotalEstimatedHrs)
	}
	wantActual := 5400.0 / 3600
	if ts.TotalActualHrs != wantActual {
		t.Errorf("TotalActualHrs = %.4f, want %.4f", ts.TotalActualHrs, wantActual)
	}

	// Verify the calibration section appears in Stats() output.
	out := captureStdout(t, func() {
		Stats(s, cfg, StatsOpts{Time: true})
	})
	if !strings.Contains(out, "calibration") {
		t.Errorf("stats --time output should contain 'calibration', got:\n%s", out)
	}
}
