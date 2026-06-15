package pricing

import (
	"math"
	"testing"
)

// Static fixture representing a simplified Anthropic pricing table.
// Two model families (opus, haiku) with explicit Input/Output columns
// so the parser exercises the "derive cache from ratios" path.
const fixtureHTML = `
<html><body>
<table>
<tr><th>Model</th><th>Input</th><th>Output</th></tr>
<tr><td>Claude Opus 4.7</td><td>$5 / MTok</td><td>$25 / MTok</td></tr>
<tr><td>Claude Haiku 4.5</td><td>$1 / MTok</td><td>$5 / MTok</td></tr>
<tr><td>Claude Sonnet 4.6</td><td>$3 / MTok</td><td>$15 / MTok</td></tr>
</table>
</body></html>
`

// fixtureHTMLExplicitCache adds rows with 5 price columns to test the
// explicit-cache path (page lists all five tiers directly).
const fixtureHTMLExplicitCache = `
<html><body>
<table>
<tr><th>Model</th><th>Input</th><th>Output</th><th>Cache Write 5m</th><th>Cache Write 1h</th><th>Cache Read</th></tr>
<tr>
  <td>Claude Opus 4.7</td>
  <td>$5 / MTok</td><td>$25 / MTok</td>
  <td>$6.25 / MTok</td><td>$10 / MTok</td><td>$0.5 / MTok</td>
</tr>
<tr>
  <td>Claude Haiku 4.5</td>
  <td>$1 / MTok</td><td>$5 / MTok</td>
  <td>$1.25 / MTok</td><td>$2 / MTok</td><td>$0.1 / MTok</td>
</tr>
</table>
</body></html>
`

func TestParseAnthropicHTML_Valid(t *testing.T) {
	rates, err := parseAnthropicHTML(fixtureHTML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opus, ok := rates["claude-opus-4-7"]
	if !ok {
		t.Fatalf("claude-opus-4-7 not found; got %v", rates)
	}
	if opus.Input != 5 || opus.Output != 25 {
		t.Errorf("opus rates wrong: %+v", opus)
	}
	// Derived cache rates
	if math.Abs(opus.CacheWrite5m-6.25) > 1e-9 {
		t.Errorf("opus cache_write_5m wrong: %g", opus.CacheWrite5m)
	}
	if math.Abs(opus.CacheRead-0.5) > 1e-9 {
		t.Errorf("opus cache_read wrong: %g", opus.CacheRead)
	}

	haiku, ok := rates["claude-haiku-4-5"]
	if !ok {
		t.Fatalf("claude-haiku-4-5 not found")
	}
	if haiku.Input != 1 || haiku.Output != 5 {
		t.Errorf("haiku rates wrong: %+v", haiku)
	}

	if _, ok := rates["claude-sonnet-4-6"]; !ok {
		t.Error("claude-sonnet-4-6 not found")
	}
}

func TestParseAnthropicHTML_ExplicitCacheColumns(t *testing.T) {
	rates, err := parseAnthropicHTML(fixtureHTMLExplicitCache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opus := rates["claude-opus-4-7"]
	if math.Abs(opus.CacheWrite5m-6.25) > 1e-9 {
		t.Errorf("explicit cache_write_5m wrong: %g", opus.CacheWrite5m)
	}
	if math.Abs(opus.CacheWrite1h-10) > 1e-9 {
		t.Errorf("explicit cache_write_1h wrong: %g", opus.CacheWrite1h)
	}
	if math.Abs(opus.CacheRead-0.5) > 1e-9 {
		t.Errorf("explicit cache_read wrong: %g", opus.CacheRead)
	}
}

func TestParseAnthropicHTML_MissingModels(t *testing.T) {
	truncated := `<html><body><table><tr><td>Claude Opus 4.7</td><td>$5</td><td>$25</td></tr></table></body></html>`
	_, err := parseAnthropicHTML(truncated)
	if err == nil {
		t.Fatal("expected error for single-family parse result")
	}
}

func TestDiffRates_Unchanged(t *testing.T) {
	rates := map[string]Rate{
		"claude-opus-4-7": {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5},
	}
	diffs := DiffRates(rates, rates)
	if len(diffs) != 0 {
		t.Errorf("expected empty diff for identical tables, got %v", diffs)
	}
}

func TestDiffRates_PriceChange(t *testing.T) {
	old := map[string]Rate{
		"claude-opus-4-7": {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5},
	}
	newR := map[string]Rate{
		"claude-opus-4-7": {Input: 10, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5},
	}
	diffs := DiffRates(old, newR)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d: %v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Field != "input" {
		t.Errorf("expected field 'input', got %q", d.Field)
	}
	if d.Old != 5 || d.New != 10 {
		t.Errorf("wrong old/new: %g → %g", d.Old, d.New)
	}
	if math.Abs(d.PctChange-100) > 1e-9 {
		t.Errorf("expected 100%% change, got %.2f", d.PctChange)
	}
}

func TestDiffRates_NewModel(t *testing.T) {
	old := map[string]Rate{}
	newR := map[string]Rate{
		"claude-opus-4-7": {Input: 5, Output: 25},
	}
	diffs := DiffRates(old, newR)
	if len(diffs) == 0 {
		t.Fatal("expected diffs for new model")
	}
	for _, d := range diffs {
		if d.Old != 0 {
			t.Errorf("new model entry should have Old=0, got %g", d.Old)
		}
	}
}

func TestSanityCheck_Within(t *testing.T) {
	diffs := []RateDiff{
		{Old: 5, New: 6.5, PctChange: 30},
		{Old: 5, New: 4, PctChange: -20},
	}
	if !SanityCheck(diffs, 50) {
		t.Error("expected sanity check to pass for changes ≤50%")
	}
}

func TestSanityCheck_Over(t *testing.T) {
	diffs := []RateDiff{
		{Old: 5, New: 6.5, PctChange: 30},
		{Old: 5, New: 8, PctChange: 60}, // exceeds 50%
	}
	if SanityCheck(diffs, 50) {
		t.Error("expected sanity check to fail for change >50%")
	}
}

func TestSanityCheck_Empty(t *testing.T) {
	if !SanityCheck(nil, 50) {
		t.Error("empty diffs should always pass sanity check")
	}
}

func TestSanityCheck_NewModelAlwaysAllowed(t *testing.T) {
	// New model: Old==0, PctChange==100; must never block regardless of maxPct.
	diffs := []RateDiff{
		{Old: 0, New: 5, PctChange: 100},
		{Old: 0, New: 25, PctChange: 100},
	}
	if !SanityCheck(diffs, 50) {
		t.Error("new model additions (Old==0) must always pass SanityCheck")
	}
}

func TestSanityCheck_ZeroPctDisables(t *testing.T) {
	// maxPct <= 0 means no limit — everything passes.
	diffs := []RateDiff{
		{Old: 5, New: 500, PctChange: 9900},
	}
	if !SanityCheck(diffs, 0) {
		t.Error("SanityCheck with maxPct=0 should always pass (no limit)")
	}
}

func TestAnthropicNameToID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Claude Opus 4.7", "claude-opus-4-7"},
		{"Claude Haiku 3.5", "claude-haiku-3-5"},
		{"Claude Sonnet 4.6", "claude-sonnet-4-6"},
		{"claude opus 4", "claude-opus-4"},
		{"GPT-4", ""},           // not claude
		{"", ""},                // empty
		{"Claude Opus", "claude-opus"}, // no version
		// Annotation stripping
		{"Claude Haiku 3.5 (retired, except on Bedrock and Vertex AI)", "claude-haiku-3-5"},
		{"Claude Opus 4 (deprecated)", "claude-opus-4"},
		// Slash-combined entries are rejected
		{"Claude Opus 4.6 / Claude Opus 4.7", ""},
	}
	for _, tc := range cases {
		got := anthropicNameToID(tc.in)
		if got != tc.want {
			t.Errorf("anthropicNameToID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
