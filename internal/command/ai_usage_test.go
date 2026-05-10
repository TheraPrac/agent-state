package command

import (
	"encoding/json"
	"testing"
)

func TestClaudeUsageAdapter_NormalizesEnvelope(t *testing.T) {
	raw, err := json.Marshal(ClaudeResult{
		Type:         "result",
		Subtype:      "success",
		TotalCostUSD: 0.0123,
		DurationMs:   2500,
		SessionID:    "claude-session",
		Usage: ClaudeUsage{
			InputTokens:              1000,
			OutputTokens:             500,
			CacheCreationInputTokens: 200,
			CacheReadInputTokens:     300,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := (ClaudeUsageAdapter{}).Normalize(raw, UsageMeta{Model: "claude-sonnet-4-6", Step: "implement", ProcessMs: 3000})
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if got.Provider != AIProviderClaude {
		t.Errorf("Provider = %q, want %q", got.Provider, AIProviderClaude)
	}
	if got.Model != "claude-sonnet-4-6" || got.SessionID != "claude-session" || got.Step != "implement" {
		t.Errorf("metadata not preserved: %+v", got)
	}
	if got.RegInputTokens != 1000 || got.CachedInTokens != 300 || got.CacheWriteTokens != 200 || got.RegOutputTokens != 500 {
		t.Errorf("token mapping wrong: %+v", got)
	}
	if got.TotalTokens != 2000 {
		t.Errorf("TotalTokens = %d, want 2000", got.TotalTokens)
	}
	if got.CostSource != CostSourceProvided {
		t.Errorf("CostSource = %q, want %q", got.CostSource, CostSourceProvided)
	}
}

func TestOpenAIResponsesUsageAdapter_NormalizesResponsesUsage(t *testing.T) {
	raw := []byte(`{
		"id": "resp_123",
		"model": "gpt-5.2",
		"usage": {
			"input_tokens": 1200,
			"input_tokens_details": {"cached_tokens": 400},
			"output_tokens": 300,
			"output_tokens_details": {"reasoning_tokens": 75},
			"total_tokens": 1500
		}
	}`)

	got, err := (OpenAIResponsesUsageAdapter{}).Normalize(raw, UsageMeta{SessionID: "codex-session", Step: "code_review", ProcessMs: 7000})
	if err != nil {
		t.Fatalf("Normalize error: %v", err)
	}
	if got.Provider != AIProviderOpenAI {
		t.Errorf("Provider = %q, want %q", got.Provider, AIProviderOpenAI)
	}
	if got.ResponseID != "resp_123" || got.Model != "gpt-5.2" || got.SessionID != "codex-session" {
		t.Errorf("metadata mapping wrong: %+v", got)
	}
	if got.RegInputTokens != 800 || got.CachedInTokens != 400 || got.RegOutputTokens != 300 {
		t.Errorf("token mapping wrong: %+v", got)
	}
	if got.ReasoningTokens != 75 || got.TotalTokens != 1500 {
		t.Errorf("reasoning/total mapping wrong: %+v", got)
	}
	if got.CostSource != CostSourceUnknown || got.CostUSD != 0 {
		t.Errorf("OpenAI cost should be unknown unless provided, got source=%q cost=%.6f", got.CostSource, got.CostUSD)
	}
}

func TestSessionLogPayloadFromUsage(t *testing.T) {
	u := AIUsage{
		Provider:        AIProviderOpenAI,
		Model:           "gpt-5.2",
		SessionID:       "s",
		ResponseID:      "resp",
		Step:            "implement",
		ProcessMs:       1000,
		AIMs:            900,
		RegInputTokens:  10,
		CachedInTokens:  20,
		RegOutputTokens: 30,
		ReasoningTokens: 7,
		TotalTokens:     60,
		CostSource:      CostSourceUnknown,
	}
	got := sessionLogPayloadFromUsage(u, "T-001")
	if got.Provider != AIProviderOpenAI || got.ResponseID != "resp" || got.ItemID != "T-001" {
		t.Errorf("payload metadata wrong: %+v", got)
	}
	if got.RegInputTokens != 10 || got.CacheReadInputTokens != 20 || got.RegOutputTokens != 30 ||
		got.ReasoningTokens != 7 || got.TotalTokens != 60 {
		t.Errorf("payload token mapping wrong: %+v", got)
	}
}
