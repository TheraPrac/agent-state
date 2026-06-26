package command

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
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

// TestStatsCalibrationExcludesZeroEstimate verifies that closed items whose
// estimated_hours is 0 (the value st migrate backfills onto legacy items) are
// excluded from the calibration aggregate, so backfilled zeros don't dilute the
// avg-estimate signal. Only items with a real (> 0) estimate are counted.
func TestStatsCalibrationExcludesZeroEstimate(t *testing.T) {
	s, _ := setupTestEnv(t)

	// T-001: real estimate → counts.
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

	// T-002: backfilled zero estimate with real actual duration → must be excluded.
	if err := s.Mutate("T-002", func(item *model.Item) error {
		item.Status = "done"
		item.Doc.SetField("status", "done")
		item.SetNested("time_tracking", "estimated_hours", "0.00")
		item.SetNested("time_tracking", "total_duration_seconds", "7200")
		item.SetNested("time_tracking", "turn_count", "4")
		return nil
	}); err != nil {
		t.Fatalf("Mutate T-002: %v", err)
	}

	ts := computeTimeStats(s)
	if ts.EstimatedItems != 1 {
		t.Errorf("EstimatedItems = %d, want 1 (zero-estimate item must be excluded)", ts.EstimatedItems)
	}
	if ts.TotalEstimatedHrs != 2.0 {
		t.Errorf("TotalEstimatedHrs = %.2f, want 2.00 (only the real-estimate item)", ts.TotalEstimatedHrs)
	}
	// The 7200s actual from the zero-estimate item must NOT be summed in.
	wantActual := 5400.0 / 3600
	if ts.TotalActualHrs != wantActual {
		t.Errorf("TotalActualHrs = %.4f, want %.4f (zero-estimate item's actual excluded)", ts.TotalActualHrs, wantActual)
	}
}
