package command

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	AIProviderClaude = "claude"
	AIProviderOpenAI = "openai"

	CostSourceProvided  = "provided"
	CostSourceComputed  = "computed"
	CostSourceUnknown   = "unknown"
	CostSourceEstimated = "estimated" // cost from our own pricing table, not provider-reported
)

// AIUsage is the provider-neutral per-turn usage record. It is intentionally
// close to SessionLogPayload because SessionLog is the durable accumulator.
type AIUsage struct {
	Provider         string
	Model            string
	SessionID        string
	ResponseID       string
	Step             string
	ProcessMs        int64
	AIMs             int64
	RegInputTokens   int
	CachedInTokens   int
	CacheWriteTokens int
	RegOutputTokens  int
	ReasoningTokens  int
	TotalTokens      int
	CostUSD          float64
	CostSource       string
}

// UsageAdapter normalizes one provider's response envelope into AIUsage.
type UsageAdapter interface {
	Provider() string
	Normalize(raw []byte, meta UsageMeta) (AIUsage, error)
}

type UsageMeta struct {
	Model     string
	SessionID string
	Step      string
	ProcessMs int64
}

type ClaudeUsageAdapter struct{}

func (ClaudeUsageAdapter) Provider() string { return AIProviderClaude }

func (ClaudeUsageAdapter) Normalize(raw []byte, meta UsageMeta) (AIUsage, error) {
	result, err := parseClaudeOutput(raw)
	if err != nil {
		return AIUsage{}, err
	}
	return usageFromClaudeResult(result, meta), nil
}

func usageFromClaudeResult(result *ClaudeResult, meta UsageMeta) AIUsage {
	if result == nil {
		return AIUsage{}
	}
	model := meta.Model
	sessionID := result.SessionID
	if sessionID == "" {
		sessionID = meta.SessionID
	}
	total := result.Usage.InputTokens +
		result.Usage.CacheReadInputTokens +
		result.Usage.CacheCreationInputTokens +
		result.Usage.OutputTokens
	source := ""
	if result.TotalCostUSD > 0 {
		source = CostSourceProvided
	}
	return AIUsage{
		Provider:         AIProviderClaude,
		Model:            model,
		SessionID:        sessionID,
		Step:             meta.Step,
		ProcessMs:        meta.ProcessMs,
		AIMs:             result.DurationMs,
		RegInputTokens:   result.Usage.InputTokens,
		CachedInTokens:   result.Usage.CacheReadInputTokens,
		CacheWriteTokens: result.Usage.CacheCreationInputTokens,
		RegOutputTokens:  result.Usage.OutputTokens,
		TotalTokens:      total,
		CostUSD:          result.TotalCostUSD,
		CostSource:       source,
	}
}

type OpenAIResponsesUsageAdapter struct{}

func (OpenAIResponsesUsageAdapter) Provider() string { return AIProviderOpenAI }

func (OpenAIResponsesUsageAdapter) Normalize(raw []byte, meta UsageMeta) (AIUsage, error) {
	var envelope openAIResponsesEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return AIUsage{}, err
	}
	if envelope.Usage == nil {
		return AIUsage{}, fmt.Errorf("openai responses usage: missing usage")
	}
	model := envelope.Model
	if model == "" {
		model = meta.Model
	}
	responseID := envelope.ID
	if responseID == "" {
		responseID = envelope.ResponseID
	}
	return usageFromOpenAIResponsesUsage(model, responseID, *envelope.Usage, meta), nil
}

type openAIResponsesEnvelope struct {
	ID         string                `json:"id"`
	ResponseID string                `json:"response_id"`
	Model      string                `json:"model"`
	Usage      *openAIResponsesUsage `json:"usage"`
}

type openAIResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func usageFromOpenAIResponsesUsage(model, responseID string, usage openAIResponsesUsage, meta UsageMeta) AIUsage {
	cached := usage.InputTokensDetails.CachedTokens
	regIn := usage.InputTokens - cached
	if regIn < 0 {
		regIn = 0
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	return AIUsage{
		Provider:        AIProviderOpenAI,
		Model:           model,
		SessionID:       meta.SessionID,
		ResponseID:      responseID,
		Step:            meta.Step,
		ProcessMs:       meta.ProcessMs,
		AIMs:            meta.ProcessMs,
		RegInputTokens:  regIn,
		CachedInTokens:  cached,
		RegOutputTokens: usage.OutputTokens,
		ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens,
		TotalTokens:     total,
		CostSource:      CostSourceUnknown,
	}
}

func sessionLogPayloadFromUsage(u AIUsage, itemID string) SessionLogPayload {
	source := strings.TrimSpace(u.CostSource)
	if source == "" && u.CostUSD > 0 {
		source = CostSourceProvided
	}
	return SessionLogPayload{
		Provider:        u.Provider,
		SessionID:       u.SessionID,
		ResponseID:      u.ResponseID,
		Model:           u.Model,
		ProcessMs:       u.ProcessMs,
		AIMs:            u.AIMs,
		RegInputTokens:  u.RegInputTokens,
		RegOutputTokens: u.RegOutputTokens,
		CacheReadInputTokens:   u.CachedInTokens,
		CacheCreation5mInputTokens:  u.CacheWriteTokens,
		ReasoningTokens: u.ReasoningTokens,
		TotalTokens:     u.TotalTokens,
		CostUSD:         u.CostUSD,
		CostSource:      source,
		ItemID:          itemID,
		Step:            u.Step,
	}
}

func applyUsageToStepResult(sr *StepResult, u AIUsage) {
	if sr == nil {
		return
	}
	sr.Provider = u.Provider
	sr.Model = u.Model
	sr.ClaudeSessionID = u.SessionID
	sr.ResponseID = u.ResponseID
	sr.AIDurationMs = u.AIMs
	sr.RegInputTokens = u.RegInputTokens
	sr.CacheReadInputTokens = u.CachedInTokens
	sr.CacheCreation5mInputTokens = u.CacheWriteTokens
	sr.OutputTokens = u.RegOutputTokens
	sr.ReasoningTokens = u.ReasoningTokens
	sr.TotalTokens = u.TotalTokens
	sr.CostUSD = u.CostUSD
	sr.CostSource = u.CostSource
	sr.InputTokens = sr.RegInputTokens + sr.CacheReadInputTokens + sr.CacheCreation5mInputTokens
}
