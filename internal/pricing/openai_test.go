package pricing

import (
	"math"
	"testing"
)

func TestOpenAIPricing_GPT55EstimatedCost(t *testing.T) {
	// gpt-5.5: $100/M input, $300/M output, $25/M cached
	// 1000 input, 500 output, 200 cached = 0.10 + 0.15 + 0.005 = 0.255
	got, err := EstimateOpenAICostUSD("gpt-5.5", 1000, 500, 200)
	if err != nil {
		t.Fatalf("EstimateOpenAICostUSD(gpt-5.5): %v", err)
	}
	want := 0.255
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("gpt-5.5 cost = %.9f, want %.9f", got, want)
	}
}

func TestOpenAIPricing_CodexMiniLatest(t *testing.T) {
	// codex-mini-latest: $1.50/M input, $6.00/M output, $0.375/M cached
	// 1000 input, 400 output, 600 cached = 0.0015 + 0.0024 + 0.000225 = 0.004125
	got, err := EstimateOpenAICostUSD("codex-mini-latest", 1000, 400, 600)
	if err != nil {
		t.Fatalf("EstimateOpenAICostUSD(codex-mini-latest): %v", err)
	}
	want := 0.004125
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("codex-mini-latest cost = %.9f, want %.9f", got, want)
	}
}

func TestOpenAIPricing_UnknownModelReturnsError(t *testing.T) {
	_, err := EstimateOpenAICostUSD("gpt-999-turbo", 100, 100, 0)
	if err == nil {
		t.Error("unknown model should return error")
	}
}

func TestOpenAIPricing_ZeroTokensIsZeroCost(t *testing.T) {
	got, err := EstimateOpenAICostUSD("o3", 0, 0, 0)
	if err != nil {
		t.Fatalf("EstimateOpenAICostUSD(o3, 0,0,0): %v", err)
	}
	if got != 0 {
		t.Errorf("zero tokens cost = %f, want 0", got)
	}
}
