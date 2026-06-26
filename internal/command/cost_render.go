package command

import (
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/pricing"
)

// SyntheticCostLabel is the canonical user-facing label for any number
// produced by EstimateItemCostUSD. The "estimate" qualifier matters: on Max
// plan there is no real per-call billing to compare against, and the rate
// table itself can drift relative to Anthropic's published prices.
const SyntheticCostLabel = "synthetic API cost estimate (current rates)"

// EstimateItemCostUSD computes the on-demand synthetic cost for an item from
// time_tracking.real_tokens × the current pricing rate table. I-569 step 5
// replaces the old "read accumulated time_tracking.ai_cost_usd" path: cost
// is no longer stored, so a price drop in pricing.go automatically shows up
// in every render without a migration.
//
// Returns 0 if real_tokens is absent (legacy data not yet reconciled by
// step 8's migration) or last_model isn't in the rate table. Renderers
// should treat 0 as "not available" and display "—" / omit the field
// rather than printing $0.00 — see stats.go / show.go updates in Step 5.
func EstimateItemCostUSD(item *model.Item) float64 {
	if item == nil {
		return 0
	}
	rt := readRealTokens(item)
	if rt == (realTokens{}) {
		return 0
	}
	model, _ := getNestedField(item, "time_tracking", "last_model")
	if model == "" {
		return 0
	}
	cost, err := pricing.EstimateSyntheticCostUSD(
		model, rt.Input, rt.Output, rt.CacheRead, rt.CacheCreation5m, rt.CacheCreation1h,
	)
	if err != nil {
		return 0
	}
	return cost
}

// EstimateItemCostBreakdown is the per-class form used by show.go's detailed
// time_tracking block. Same fallback rules as EstimateItemCostUSD; returns a
// zero-value CostBreakdown when data is missing.
func EstimateItemCostBreakdown(item *model.Item) pricing.CostBreakdown {
	if item == nil {
		return pricing.CostBreakdown{}
	}
	rt := readRealTokens(item)
	if rt == (realTokens{}) {
		return pricing.CostBreakdown{}
	}
	model, _ := getNestedField(item, "time_tracking", "last_model")
	if model == "" {
		return pricing.CostBreakdown{}
	}
	b, err := pricing.EstimateSyntheticCostBreakdown(
		model, rt.Input, rt.Output, rt.CacheRead, rt.CacheCreation5m, rt.CacheCreation1h,
	)
	if err != nil {
		return pricing.CostBreakdown{}
	}
	return b
}
