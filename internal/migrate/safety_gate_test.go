package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/parse"
)

// TestMigrationSafetyGate is the mandatory gate before running migration on real data.
// It verifies:
// 1. All real files parse without error
// 2. Canonical output re-parses cleanly
// 3. Canonical is idempotent (canonicalize twice = same output)
// 4. Key fields are preserved (no data loss)
func TestMigrationSafetyGate(t *testing.T) {
	dir := os.Getenv("AS_TEST_DIR")
	if dir == "" {
		t.Skip("AS_TEST_DIR not set — set to agent-state directory for migration safety gate")
	}

	cfg := safetyGateConfig()
	tmpDir := t.TempDir()

	var total, changed int

	for _, subdir := range []string{"tasks", "issues", "archive"} {
		subPath := filepath.Join(dir, subdir)
		entries, err := os.ReadDir(subPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("reading %s: %v", subPath, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(subPath, entry.Name())
			total++

			t.Run(entry.Name(), func(t *testing.T) {
				// Phase 1: Parse original file
				origItem, err := parse.File(path)
				if err != nil {
					t.Fatalf("parse original: %v", err)
					return
				}

				if origItem.ID == "" {
					t.Skipf("skipping %s: empty ID (likely YAML frontmatter format)", entry.Name())
					return
				}

				// Phase 2: Generate canonical output
				result := PlanFile(origItem, path, cfg)
				if result.After == "" {
					t.Fatalf("canonical output is empty")
				}

				if result.HasChanges() {
					changed++
				}

				// Phase 3: Re-parse canonical output
				tmpPath := filepath.Join(tmpDir, entry.Name())
				if err := os.WriteFile(tmpPath, []byte(result.After), 0644); err != nil {
					t.Fatalf("write temp: %v", err)
				}

				item2, err := parse.File(tmpPath)
				if err != nil {
					t.Fatalf("parse canonical: %v", err)
				}

				// Phase 4: Verify key fields preserved
				if item2.ID != origItem.ID {
					t.Errorf("ID changed: %q → %q", origItem.ID, item2.ID)
				}
				if item2.Title != origItem.Title {
					t.Errorf("Title changed: %q → %q", origItem.Title, item2.Title)
				}
				if item2.Status != origItem.Status {
					t.Errorf("Status changed: %q → %q", origItem.Status, item2.Status)
				}
				if item2.Type != origItem.Type {
					t.Errorf("Type changed: %q → %q", origItem.Type, item2.Type)
				}
				if len(origItem.DependsOn) > 0 {
					if len(item2.DependsOn) != len(origItem.DependsOn) {
						t.Errorf("DependsOn count changed: %d → %d", len(origItem.DependsOn), len(item2.DependsOn))
					}
				}

				// Phase 5: Verify idempotency
				second := Canonical(item2, cfg)
				if result.After != second {
					firstLines := strings.Split(result.After, "\n")
					secondLines := strings.Split(second, "\n")
					for i := 0; i < len(firstLines) || i < len(secondLines); i++ {
						var fl, sl string
						if i < len(firstLines) {
							fl = firstLines[i]
						}
						if i < len(secondLines) {
							sl = secondLines[i]
						}
						if fl != sl {
							t.Errorf("idempotency diff at line %d:\n  first:  %q\n  second: %q", i+1, fl, sl)
						}
					}
					t.Error("canonical output not idempotent")
				}
			})
		}
	}

	t.Logf("safety gate: %d files checked, %d would change", total, changed)

	if total < 240 {
		t.Errorf("expected >= 240 files, got %d (directory may be wrong)", total)
	}
}

// safetyGateConfig matches the theraprac production config.
func safetyGateConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      {Command: "cd theraprac-api && make test-unit"},
			"api_lint":      {Command: "cd theraprac-api && make lint"},
			"web_typecheck": {Command: "cd theraprac-web && make type-check"},
			"web_unit":      {Command: "cd theraprac-web && make test-unit"},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {Command: "cd theraprac-api && make integration-local"},
			"web_integration": {Command: "cd theraprac-web && make test-integration"},
			"web_e2e":         {Command: "cd theraprac-web && make e2e"},
		},
	}
	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "uat_approved", "closed"},
		ArchiveGate: "uat_approved",
	}
	return cfg
}
