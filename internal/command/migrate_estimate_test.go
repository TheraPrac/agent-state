package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/migrate"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/parse"
)

// TestMigrateEstimateDefaults verifies that migrate.Canonical injects
// estimated_hours: 0 into items that have a time_tracking block but no
// estimated_hours field (the I-591 migration backfill).
func TestMigrateEstimateDefaults(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Write some time_tracking data onto T-001 without estimated_hours.
	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.SetNested("time_tracking", "turn_count", "5")
		item.SetNested("time_tracking", "ai_cost_usd", "0.012345")
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	path, _ := s.Path("T-001")
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse.File: %v", err)
	}

	// Run Canonical migration.
	canonical := migrate.Canonical(item, cfg)
	if !strings.Contains(canonical, "estimated_hours: 0") {
		t.Errorf("Canonical output should contain 'estimated_hours: 0', got:\n%s", canonical)
	}

	t.Run("idempotent — second migration does not duplicate field", func(t *testing.T) {
		// Write canonical to temp file, parse again, run Canonical.
		tmp := filepath.Join(t.TempDir(), "item.md")
		if err := os.WriteFile(tmp, []byte(canonical), 0644); err != nil {
			t.Fatalf("write tmp: %v", err)
		}
		item2, err := parse.File(tmp)
		if err != nil {
			t.Fatalf("parse.File(canonical): %v", err)
		}
		canonical2 := migrate.Canonical(item2, cfg)
		count := strings.Count(canonical2, "estimated_hours:")
		if count != 1 {
			t.Errorf("canonical2 has %d 'estimated_hours:' occurrences, want exactly 1:\n%s", count, canonical2)
		}
	})
}
