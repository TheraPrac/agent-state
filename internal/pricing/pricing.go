// Package pricing computes per-turn USD cost from Claude token counts.
//
// Rates are USD per million tokens (MTok), sourced from
// https://docs.anthropic.com/en/docs/about-claude/pricing — updated
// automatically by `st pricing refresh` (I-367). The rate table lives in
// table.go; run `st pricing refresh --dry-run` to preview any drift.
//
// Cost computation assumes:
//   - Standard interactive/agentic mode (not Batch API 50% discount)
//   - Global endpoint (not US-only data residency 1.1x multiplier)
//   - Not Fast mode (6x Opus 4.6 premium)
//   - Both 5-minute (1.25x input) and 1-hour (2x input) prompt cache tiers
//     are supported — pass the token counts to ComputeCost separately.
package pricing

import (
	"fmt"
	"sort"
	"strings"
)

// Rate holds per-MTok USD rates for every token category we track.
// All values are USD per million tokens; divide by 1_000_000 to get per-token.
type Rate struct {
	Input        float64 // regular uncached input
	Output       float64
	CacheWrite5m float64 // 5-minute cache creation (1.25x input)
	CacheWrite1h float64 // 1-hour cache creation (2x input)
	CacheRead    float64 // cache hit / refresh (0.1x input)
}

// ErrUnknownModel is returned by Lookup / ComputeCost when the model id is not
// in the table. Callers should surface this (not silently treat as zero cost)
// and add the new model via `st pricing refresh` or a manual table.go update.
type ErrUnknownModel struct {
	Model string
}

func (e *ErrUnknownModel) Error() string {
	return fmt.Sprintf("pricing: unknown model %q — run `st pricing refresh` or add to internal/pricing/table.go", e.Model)
}

// Lookup returns the Rate for a model id. The id is normalized (lowercased,
// trailing "-YYYYMMDD" date suffix stripped) before lookup. Returns
// *ErrUnknownModel if no matching entry exists.
func Lookup(model string) (Rate, error) {
	key := normalize(model)
	if r, ok := table[key]; ok {
		return r, nil
	}
	return Rate{}, &ErrUnknownModel{Model: model}
}

// EstimateSyntheticCostUSD returns the USD cost estimate for a single turn
// given token counts. "Synthetic" because we apply the published rate table
// rather than reading a real billing line — on Max plan there is no real
// per-call billing to compare against, so this number is an estimate of what
// the same usage would have cost on the metered API.
//
// regIn:  regular (uncached) input tokens
// regOut: output tokens
// cacheIn:  tokens read from cache (cache hits)
// cacheOut5m: tokens written to the 5-minute cache (1.25x input rate)
// cacheOut1h: tokens written to the 1-hour cache (2x input rate)
func EstimateSyntheticCostUSD(model string, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h int) (float64, error) {
	b, err := EstimateSyntheticCostBreakdown(model, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h)
	if err != nil {
		return 0, err
	}
	return b.Total, nil
}

// CostBreakdown carries per-token-class USD costs alongside the total.
// Useful for renderers (Step 5 of I-569) that want to show "$0.05 of which
// $0.04 cache_read" without recomputing the line.
type CostBreakdown struct {
	Input            float64
	Output           float64
	CacheRead        float64
	CacheCreation5m  float64
	CacheCreation1h  float64
	Total            float64
}

// EstimateSyntheticCostBreakdown returns the per-class USD cost decomposition
// used by I-569's stats / show / status renderers. Same inputs and same rate
// lookup as EstimateSyntheticCostUSD.
func EstimateSyntheticCostBreakdown(model string, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h int) (CostBreakdown, error) {
	r, err := Lookup(model)
	if err != nil {
		return CostBreakdown{}, err
	}
	const perToken = 1_000_000.0
	b := CostBreakdown{
		Input:           float64(regIn) * r.Input / perToken,
		Output:          float64(regOut) * r.Output / perToken,
		CacheRead:       float64(cacheIn) * r.CacheRead / perToken,
		CacheCreation5m: float64(cacheOut5m) * r.CacheWrite5m / perToken,
		CacheCreation1h: float64(cacheOut1h) * r.CacheWrite1h / perToken,
	}
	b.Total = b.Input + b.Output + b.CacheRead + b.CacheCreation5m + b.CacheCreation1h
	return b, nil
}

// ComputeCost is kept as a thin alias to EstimateSyntheticCostUSD. All
// internal call sites have already moved to the explicit name; the alias
// only exists for any out-of-tree consumer compiled against the old API.
// Safe to delete once that audit completes.
//
// Deprecated: use EstimateSyntheticCostUSD.
func ComputeCost(model string, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h int) (float64, error) {
	return EstimateSyntheticCostUSD(model, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h)
}

// KnownModels returns the sorted list of model ids in the pricing table.
// Useful for diagnostic output and for tests asserting coverage.
func KnownModels() []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KnownRates returns a copy of the current rate table.
// Used by PricingRefresh to diff fetched rates against the hardcoded values.
func KnownRates() map[string]Rate {
	out := make(map[string]Rate, len(table))
	for k, v := range table {
		out[k] = v
	}
	return out
}

// RenderTable generates the complete Go source for table.go from rates.
// The output is ready to write atomically; includes package declaration and
// generated-code header so the refresh command can overwrite the file directly.
func RenderTable(rates map[string]Rate) string {
	keys := make([]string, 0, len(rates))
	for k := range rates {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("// Code generated by `st pricing refresh`. DO NOT EDIT manually.\n")
	b.WriteString("// To update rates: st pricing refresh (or --dry-run to preview)\n")
	b.WriteString("// Source: https://docs.anthropic.com/en/docs/about-claude/pricing\n")
	b.WriteString("package pricing\n\n")
	b.WriteString("// table holds per-model USD rates per million tokens (MTok).\n")
	b.WriteString("// Model IDs are canonical (e.g., \"claude-opus-4-7\"). Lookups normalize\n")
	b.WriteString("// to lowercase; version suffixes like \"-20260215\" are tolerated.\n")
	b.WriteString("var table = map[string]Rate{\n")
	for _, k := range keys {
		r := rates[k]
		fmt.Fprintf(&b, "\t%q: {Input: %g, Output: %g, CacheWrite5m: %g, CacheWrite1h: %g, CacheRead: %g},\n",
			k, r.Input, r.Output, r.CacheWrite5m, r.CacheWrite1h, r.CacheRead)
	}
	b.WriteString("}\n")
	return b.String()
}

// normalize strips the "-YYYYMMDD" date suffix that Anthropic appends to some
// snapshot ids (e.g. "claude-haiku-4-5-20251001") and lowercases the result.
func normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// Strip trailing "-YYYYMMDD"
	if len(m) > 9 {
		tail := m[len(m)-9:]
		if tail[0] == '-' && isAllDigits(tail[1:]) {
			m = m[:len(m)-9]
		}
	}
	return m
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
