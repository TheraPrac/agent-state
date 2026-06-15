package pricing

import (
	"errors"
	"go/parser"
	"go/token"
	"math"
	"strings"
	"testing"
)

func TestLookup_KnownModel(t *testing.T) {
	r, err := Lookup("claude-opus-4-7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Input != 5 || r.Output != 25 || r.CacheRead != 0.50 || r.CacheWrite5m != 6.25 {
		t.Errorf("opus-4-7 rates wrong: %+v", r)
	}
}

func TestLookup_NormalizesDateSuffix(t *testing.T) {
	// Haiku 4.5's snapshot form as referenced in CLAUDE.md
	r, err := Lookup("claude-haiku-4-5-20251001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Input != 1 || r.Output != 5 {
		t.Errorf("haiku-4-5 rates wrong: %+v", r)
	}
}

func TestLookup_NormalizesCase(t *testing.T) {
	r, err := Lookup("Claude-Sonnet-4-6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Input != 3 {
		t.Errorf("sonnet-4-6 case-normalized lookup wrong: %+v", r)
	}
}

func TestLookup_UnknownModelReturnsTypedError(t *testing.T) {
	_, err := Lookup("gpt-5-turbo")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	var unk *ErrUnknownModel
	if !errors.As(err, &unk) {
		t.Errorf("expected *ErrUnknownModel, got %T: %v", err, err)
	}
	if unk.Model != "gpt-5-turbo" {
		t.Errorf("error should preserve original model id, got %q", unk.Model)
	}
}

func TestComputeCost_Opus47(t *testing.T) {
	// 1M regular input  ($5) + 1M output ($25) + 1M cache read ($0.50) +
	// 1M cache write ($6.25) = $36.75
	cost, err := ComputeCost("claude-opus-4-7", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 36.75
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}
}

func TestComputeCost_Sonnet46(t *testing.T) {
	// 100k input ($0.30) + 50k output ($0.75) + 10k cache read ($0.003) +
	// 5k cache write ($0.01875) = $1.07175
	cost, err := ComputeCost("claude-sonnet-4-6", 100_000, 50_000, 10_000, 5_000, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 1.07175
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}
}

func TestComputeCost_Haiku45(t *testing.T) {
	// 1M input ($1) + 1M output ($5) = $6
	cost, err := ComputeCost("claude-haiku-4-5", 1_000_000, 1_000_000, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 6.0
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}
}

func TestComputeCost_ZeroTokensReturnsZero(t *testing.T) {
	cost, err := ComputeCost("claude-opus-4-7", 0, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected zero cost for zero tokens, got %.6f", cost)
	}
}

func TestComputeCost_UnknownModelReturnsError(t *testing.T) {
	// Critical: unknown model must NOT silently return zero cost. Any transcript
	// with a new Anthropic model must surface as an error so operators add it
	// to the table rather than under-count spend forever.
	cost, err := ComputeCost("claude-future-10-0", 1000, 500, 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	var unk *ErrUnknownModel
	if !errors.As(err, &unk) {
		t.Errorf("expected *ErrUnknownModel, got %T", err)
	}
	if cost != 0 {
		t.Errorf("cost on error should be 0, got %.6f", cost)
	}
}

func TestComputeCost_CacheReadCheaperThanRegularInput(t *testing.T) {
	// Sanity: 1M cache reads must cost exactly 0.1x of 1M regular input for Opus
	regCost, _ := ComputeCost("claude-opus-4-7", 1_000_000, 0, 0, 0, 0)
	cacheCost, _ := ComputeCost("claude-opus-4-7", 0, 0, 1_000_000, 0, 0)
	ratio := cacheCost / regCost
	if math.Abs(ratio-0.1) > 1e-9 {
		t.Errorf("cache_read should be 0.1x input; ratio = %.4f", ratio)
	}
}

func TestComputeCost_CacheWriteMoreExpensiveThanRegularInput(t *testing.T) {
	// Sanity: 1M cache writes (5m) must cost exactly 1.25x of 1M regular input
	regCost, _ := ComputeCost("claude-sonnet-4-6", 1_000_000, 0, 0, 0, 0)
	cacheCost, _ := ComputeCost("claude-sonnet-4-6", 0, 0, 0, 1_000_000, 0)
	ratio := cacheCost / regCost
	if math.Abs(ratio-1.25) > 1e-9 {
		t.Errorf("cache_write (5m) should be 1.25x input; ratio = %.4f", ratio)
	}
}

func TestComputeCost_1hCacheIsTwoTimesInput(t *testing.T) {
	// Sanity: 1M 1-hour cache writes must cost exactly 2x of 1M regular input
	regCost, _ := ComputeCost("claude-opus-4-7", 1_000_000, 0, 0, 0, 0)
	cache1hCost, _ := ComputeCost("claude-opus-4-7", 0, 0, 0, 0, 1_000_000)
	ratio := cache1hCost / regCost
	if math.Abs(ratio-2.0) > 1e-9 {
		t.Errorf("cache_write_1h should be 2x input; ratio = %.4f", ratio)
	}
}

func TestComputeCost_5mAnd1hSumCorrectly(t *testing.T) {
	// 100k 5m writes + 100k 1h writes on Sonnet 4.6:
	// 100k * 3.75/MTok + 100k * 6/MTok = 0.375 + 0.6 = 0.975
	cost, err := ComputeCost("claude-sonnet-4-6", 0, 0, 0, 100_000, 100_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 0.975
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}
}

func TestKnownModels_IncludesCurrentGeneration(t *testing.T) {
	required := []string{
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
	}
	known := KnownModels()
	have := make(map[string]bool, len(known))
	for _, m := range known {
		have[m] = true
	}
	for _, m := range required {
		if !have[m] {
			t.Errorf("KnownModels() missing required entry %q", m)
		}
	}
}

func TestKnownRates_ReturnsCopy(t *testing.T) {
	rates := KnownRates()
	// Must contain at least the three current-gen models
	for _, m := range []string{"claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5"} {
		if _, ok := rates[m]; !ok {
			t.Errorf("KnownRates() missing %q", m)
		}
	}
	// Mutating the returned map must not affect the package-level table
	rates["claude-opus-4-7"] = Rate{Input: 99}
	r2, _ := Lookup("claude-opus-4-7")
	if r2.Input == 99 {
		t.Error("KnownRates() returned the live table instead of a copy")
	}
}

func TestRenderTable_RoundTrip(t *testing.T) {
	original := KnownRates()
	src := RenderTable(original)

	// Must be valid Go source
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "table.go", src, 0)
	if err != nil {
		t.Fatalf("RenderTable() produced invalid Go: %v\nsource:\n%s", err, src)
	}

	// Must include the package declaration and var table
	if !strings.Contains(src, "package pricing") {
		t.Error("RenderTable() missing package declaration")
	}
	if !strings.Contains(src, "var table = map[string]Rate{") {
		t.Error("RenderTable() missing var table declaration")
	}

	// Every model from the original table must appear in the output
	for model := range original {
		if !strings.Contains(src, model) {
			t.Errorf("RenderTable() missing model %q", model)
		}
	}
}

func TestRenderTable_EmptyTable(t *testing.T) {
	src := RenderTable(map[string]Rate{})
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "table.go", src, 0)
	if err != nil {
		t.Fatalf("RenderTable({}) produced invalid Go: %v", err)
	}
}

func TestAllRatesInternallyConsistent(t *testing.T) {
	// For every model, cache_read must equal 0.1 × input and cache_write_5m
	// must equal 1.25 × input. If this fails, we've typo'd a rate.
	for model, r := range table {
		if math.Abs(r.CacheRead-r.Input*0.1) > 1e-9 {
			t.Errorf("%s: cache_read %.4f != 0.1 × input %.4f", model, r.CacheRead, r.Input)
		}
		if math.Abs(r.CacheWrite5m-r.Input*1.25) > 1e-9 {
			t.Errorf("%s: cache_write_5m %.4f != 1.25 × input %.4f", model, r.CacheWrite5m, r.Input)
		}
		if math.Abs(r.CacheWrite1h-r.Input*2) > 1e-9 {
			t.Errorf("%s: cache_write_1h %.4f != 2 × input %.4f", model, r.CacheWrite1h, r.Input)
		}
	}
}
