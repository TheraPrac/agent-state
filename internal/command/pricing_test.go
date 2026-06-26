package command

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/pricing"
)

// pricingTestFetcher returns a fixed rate map without any HTTP calls.
func pricingTestFetcher(rates map[string]pricing.Rate) func(*http.Client) (map[string]pricing.Rate, error) {
	return func(_ *http.Client) (map[string]pricing.Rate, error) {
		return rates, nil
	}
}

// pricingNoopRunner records calls but does nothing (simulates successful build/git ops).
func pricingNoopRunner(t *testing.T) func(dir string, args ...string) error {
	t.Helper()
	return func(dir string, args ...string) error {
		t.Logf("pricingNoopRunner: dir=%s cmd=%v", dir, args)
		return nil
	}
}

// pricingTestSetup creates a minimal config + temp table.go path for pricing tests.
// All tests set AsDir and TablePath in opts, so cfg.AgentRoot() is never called.
func pricingTestSetup(t *testing.T) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "paths:\n  root: .\n"
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(cfgYAML), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	tablePath := filepath.Join(root, "table.go")
	initial := pricing.RenderTable(pricing.KnownRates())
	if err := os.WriteFile(tablePath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	return cfg, tablePath
}

func TestPricingRefreshDryRun(t *testing.T) {
	cfg, tablePath := pricingTestSetup(t)
	original, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}

	// 10% increase on all rates — within the 50% sanity bound
	changed := pricing.KnownRates()
	for k, r := range changed {
		r.Input = r.Input * 1.10
		r.Output = r.Output * 1.10
		r.CacheRead = r.CacheRead * 1.10
		r.CacheWrite5m = r.CacheWrite5m * 1.10
		r.CacheWrite1h = r.CacheWrite1h * 1.10
		changed[k] = r
	}

	code := PricingRefresh(cfg, PricingRefreshOpts{
		DryRun:    true,
		SanityPct: 50,
		TablePath: tablePath,
		AsDir:     filepath.Dir(tablePath),
		Fetcher:   pricingTestFetcher(changed),
		RunCmd:    pricingNoopRunner(t),
	})
	if code != 0 {
		t.Errorf("dry run should return 0, got %d", code)
	}

	// Table file must remain unchanged
	after, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("dry run must not modify table.go")
	}
}

func TestPricingRefreshDryRunBypassesSanity(t *testing.T) {
	cfg, tablePath := pricingTestSetup(t)
	original, _ := os.ReadFile(tablePath)

	// 60% increase exceeds the sanity bound — dry-run must still exit 0 without
	// filing a GitHub issue or modifying the table.
	exploded := pricing.KnownRates()
	for k, r := range exploded {
		r.Input = r.Input * 1.60
		exploded[k] = r
	}

	code := PricingRefresh(cfg, PricingRefreshOpts{
		DryRun:    true,
		SanityPct: 50,
		TablePath: tablePath,
		AsDir:     filepath.Dir(tablePath),
		Fetcher:   pricingTestFetcher(exploded),
		// No RunCmd — if the sanity path were hit it would call gh issue create
		// via exec.Command, which would fail loudly. RunCmd is intentionally nil.
	})
	if code != 0 {
		t.Errorf("dry run must exit 0 regardless of sanity bound, got %d", code)
	}
	after, _ := os.ReadFile(tablePath)
	if string(after) != string(original) {
		t.Error("dry run must not modify table.go")
	}
}

func TestPricingRefreshSanityBlock(t *testing.T) {
	cfg, tablePath := pricingTestSetup(t)

	// 60% price jump — exceeds the 50% sanity bound
	exploded := pricing.KnownRates()
	for k, r := range exploded {
		r.Input = r.Input * 1.60
		r.Output = r.Output * 1.60
		r.CacheRead = r.CacheRead * 1.60
		r.CacheWrite5m = r.CacheWrite5m * 1.60
		r.CacheWrite1h = r.CacheWrite1h * 1.60
		exploded[k] = r
	}

	code := PricingRefresh(cfg, PricingRefreshOpts{
		SanityPct: 50,
		TablePath: tablePath,
		AsDir:     filepath.Dir(tablePath),
		Fetcher:   pricingTestFetcher(exploded),
		RunCmd:    pricingNoopRunner(t), // intercepts gh issue create + any build/commit
	})
	if code != 1 {
		t.Errorf("sanity-blocked refresh should return 1, got %d", code)
	}

	// Table file must be unchanged
	after, _ := os.ReadFile(tablePath)
	orig := pricing.RenderTable(pricing.KnownRates())
	if string(after) != orig {
		t.Error("table.go must not be modified when sanity check fails")
	}
}

func TestPricingRefreshNoChange(t *testing.T) {
	cfg, tablePath := pricingTestSetup(t)

	code := PricingRefresh(cfg, PricingRefreshOpts{
		SanityPct: 50,
		TablePath: tablePath,
		AsDir:     filepath.Dir(tablePath),
		Fetcher:   pricingTestFetcher(pricing.KnownRates()),
		RunCmd:    pricingNoopRunner(t),
	})
	if code != 0 {
		t.Errorf("no-change refresh should return 0, got %d", code)
	}
}
