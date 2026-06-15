package pricing

import "fmt"

// OpenAITableVersion tags the snapshot date of the rates below. Bump when
// updating prices so callers can distinguish old-vs-new estimates in logs.
const OpenAITableVersion = "2025-06-01"

type openAIRate struct {
	Input       float64 // per million input tokens (USD)
	Output      float64 // per million output tokens (USD)
	CachedInput float64 // per million cached-read input tokens (USD)
}

// openAITable holds per-million-token USD rates for models the Codex CLI
// may use. Rates are estimates — mark cost records with CostSourceEstimated.
var openAITable = map[string]openAIRate{
	"codex-mini-latest": {Input: 1.50, Output: 6.00, CachedInput: 0.375},
	"codex-mini":        {Input: 1.50, Output: 6.00, CachedInput: 0.375},
	"gpt-4.1-mini":      {Input: 0.40, Output: 1.60, CachedInput: 0.10},
	"gpt-4.1":           {Input: 2.00, Output: 8.00, CachedInput: 0.50},
	"gpt-4.5":           {Input: 75.00, Output: 150.00, CachedInput: 37.50},
	"gpt-5.5":           {Input: 100.00, Output: 300.00, CachedInput: 25.00},
	"o3":                {Input: 10.00, Output: 40.00, CachedInput: 2.50},
	"o4-mini":           {Input: 1.10, Output: 4.40, CachedInput: 0.275},
}

// EstimateOpenAICostUSD returns an estimated cost for the given model and
// token counts. Returns an error for unknown models; callers should record
// cost as zero (not unknown) when the model is not in the table yet.
func EstimateOpenAICostUSD(model string, inputTokens, outputTokens, cachedInputTokens int) (float64, error) {
	r, ok := openAITable[model]
	if !ok {
		return 0, fmt.Errorf("openai pricing: unknown model %q (table version %s)", model, OpenAITableVersion)
	}
	const perMillion = 1_000_000.0
	cost := float64(inputTokens)*r.Input/perMillion +
		float64(outputTokens)*r.Output/perMillion +
		float64(cachedInputTokens)*r.CachedInput/perMillion
	return cost, nil
}
