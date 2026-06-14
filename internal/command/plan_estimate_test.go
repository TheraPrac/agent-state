package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

// TestPlanApproveEstimateGate verifies the --require-estimate gate on st plan approve.
func TestPlanApproveEstimateGate(t *testing.T) {
	s, cfg := setupTestEnv(t)

	t.Run("RequireEstimate=true without hours → rc=2", func(t *testing.T) {
		var rc int
		stderr := captureStderrStr(t, func() {
			rc = PlanApprove(s, cfg, "T-001", PlanApproveOpts{
				RequireEstimate: true,
			})
		})
		if rc != 2 {
			t.Errorf("PlanApprove() = %d, want 2 (estimate gate)", rc)
		}
		if !strings.Contains(stderr, "estimate gate failed") {
			t.Errorf("stderr should mention estimate gate, got:\n%s", stderr)
		}
	})

	t.Run("RequireEstimate=true with hours → estimate gate passes", func(t *testing.T) {
		// Write estimated_hours onto T-001 so the gate passes.
		if err := s.Mutate("T-001", func(item *model.Item) error {
			item.SetNested("time_tracking", "estimated_hours", "2.00")
			return nil
		}); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
		// With estimate present, gate passes and plan approve proceeds normally.
		// (It may fail on other gates like missing .plans file — that's fine;
		// rc != 2 means the estimate gate was cleared.)
		rc := PlanApprove(s, cfg, "T-001", PlanApproveOpts{
			RequireEstimate: true,
			BypassReview:    true, // skip sub-agent review
		})
		if rc == 2 {
			t.Errorf("PlanApprove() = 2, estimate gate should have passed with hours set")
		}
	})
}
