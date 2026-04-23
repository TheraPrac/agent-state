// Package pricing computes per-turn USD cost from Claude token counts.
//
// Rates are USD per million tokens (MTok), sourced from
// https://platform.claude.com/docs/en/about-claude/pricing — current as of
// the pricing-table commit. Rates MUST be revisited when Anthropic publishes
// new models. See issue I-367 (auto-update pricing table).
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

// table holds the rate table. Model IDs are canonical (e.g., "claude-opus-4-7").
// Lookups normalize to lowercase; version suffixes like "-20260215" are tolerated.
var table = map[string]Rate{
	// Opus 4.x current generation
	"claude-opus-4-7": {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50},
	"claude-opus-4-6": {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50},
	"claude-opus-4-5": {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50},

	// Opus 4.0/4.1 (pre-price-drop)
	"claude-opus-4-1": {Input: 15, Output: 75, CacheWrite5m: 18.75, CacheWrite1h: 30, CacheRead: 1.50},
	"claude-opus-4":   {Input: 15, Output: 75, CacheWrite5m: 18.75, CacheWrite1h: 30, CacheRead: 1.50},

	// Sonnet 4.x
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30},
	"claude-sonnet-4-5": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30},

	// Haiku
	"claude-haiku-4-5": {Input: 1, Output: 5, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.10},
	"claude-haiku-3-5": {Input: 0.80, Output: 4, CacheWrite5m: 1, CacheWrite1h: 1.6, CacheRead: 0.08},
}

// ErrUnknownModel is returned by Lookup / ComputeCost when the model id is not
// in the table. Callers should surface this (not silently treat as zero cost)
// and add the new model via a code update.
type ErrUnknownModel struct {
	Model string
}

func (e *ErrUnknownModel) Error() string {
	return fmt.Sprintf("pricing: unknown model %q — add to internal/pricing/pricing.go table", e.Model)
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

// ComputeCost returns the USD cost for a single turn given token counts.
// regIn:  regular (uncached) input tokens
// regOut: output tokens
// cacheIn:  tokens read from cache (cache hits)
// cacheOut5m: tokens written to the 5-minute cache (1.25x input rate)
// cacheOut1h: tokens written to the 1-hour cache (2x input rate)
func ComputeCost(model string, regIn, regOut, cacheIn, cacheOut5m, cacheOut1h int) (float64, error) {
	r, err := Lookup(model)
	if err != nil {
		return 0, err
	}
	const perToken = 1_000_000.0
	cost := float64(regIn)*r.Input/perToken +
		float64(regOut)*r.Output/perToken +
		float64(cacheIn)*r.CacheRead/perToken +
		float64(cacheOut5m)*r.CacheWrite5m/perToken +
		float64(cacheOut1h)*r.CacheWrite1h/perToken
	return cost, nil
}

// KnownModels returns the sorted list of model ids in the pricing table.
// Useful for diagnostic output and for tests asserting coverage.
func KnownModels() []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	// Simple sort without pulling in sort package — table is small and order
	// matters mainly for deterministic test output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
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
