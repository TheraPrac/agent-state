package command

import (
	"strings"
	"testing"
)

// TestCreateEstimateGate verifies the --require-estimate gate on st create.
func TestCreateEstimateGate(t *testing.T) {
	s, cfg := setupTestEnv(t)

	t.Run("RequireEstimate=true without hours → rc=1", func(t *testing.T) {
		var rc int
		stderr := captureStderrStr(t, func() {
			rc = Create(s, cfg, "task", "Estimate gate task", CreateOpts{
				RequireEstimate: true,
				EstimatedHours:  0,
			})
		})
		if rc != 1 {
			t.Errorf("Create() = %d, want 1 (require-estimate gate)", rc)
		}
		if !strings.Contains(stderr, "require-estimate") {
			t.Errorf("stderr should mention --require-estimate, got:\n%s", stderr)
		}
	})

	t.Run("RequireEstimate=true with hours → rc=0 and field written", func(t *testing.T) {
		var id string
		rc := Create(s, cfg, "task", "Estimate gate task with hours", CreateOpts{
			RequireEstimate: true,
			EstimatedHours:  3.0,
			IDOut:           &id,
		})
		if rc != 0 {
			t.Fatalf("Create() = %d, want 0", rc)
		}
		if id == "" {
			t.Fatal("IDOut not set")
		}
		item, ok := s.Get(id)
		if !ok {
			t.Fatalf("item %s not found after create", id)
		}
		estHrs := readFloatField(item, "time_tracking", "estimated_hours")
		if estHrs != 3.0 {
			t.Errorf("estimated_hours = %.2f, want 3.00", estHrs)
		}
	})

	t.Run("EstimatedHours without RequireEstimate → written but not enforced", func(t *testing.T) {
		var id string
		rc := Create(s, cfg, "task", "Estimate optional task", CreateOpts{
			EstimatedHours: 1.5,
			IDOut:          &id,
		})
		if rc != 0 {
			t.Fatalf("Create() = %d, want 0", rc)
		}
		item, ok := s.Get(id)
		if !ok {
			t.Fatalf("item %s not found", id)
		}
		estHrs := readFloatField(item, "time_tracking", "estimated_hours")
		if estHrs != 1.5 {
			t.Errorf("estimated_hours = %.2f, want 1.50", estHrs)
		}
	})
}
