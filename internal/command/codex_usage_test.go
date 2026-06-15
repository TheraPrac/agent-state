package command

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

// realCodexJSONOutput is a realistic --json capture from codex-cli 0.125.0.
const realCodexJSONOutput = `{"type":"thread.started","thread_id":"019ec933-96db-78c2-adbb-f8218dea0795"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Hello"}}
{"type":"turn.completed","usage":{"input_tokens":28463,"cached_input_tokens":2432,"output_tokens":17,"reasoning_output_tokens":10}}
`

func TestCodexUsage_ParsesJSONTurnCompleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  codexTokenCounts
	}{
		{
			name:  "real capture — full turn",
			input: realCodexJSONOutput,
			want: codexTokenCounts{
				Input:           28463,
				CachedInput:     2432,
				Output:          17,
				ReasoningOutput: 10,
			},
		},
		{
			name:  "two turns are summed",
			input: `{"type":"thread.started","thread_id":"abc"}` + "\n" +
				`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":20}}` + "\n" +
				`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":40}}` + "\n",
			want: codexTokenCounts{Input: 300, CachedInput: 50, Output: 60},
		},
		{
			name:  "no turn.completed returns zeros",
			input: `{"type":"thread.started","thread_id":"xyz"}` + "\n",
			want:  codexTokenCounts{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCodexTurnUsage([]byte(tc.input))
			if got != tc.want {
				t.Errorf("parseCodexTurnUsage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCodexEngine_ExtractsAndStoresSessionIDFromThreadStarted(t *testing.T) {
	const fakeThreadID = "019ec933-96db-78c2-adbb-f8218dea0795"
	var gotArgs [][]string

	engine := RunEngine{
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			gotArgs = append(gotArgs, append([]string{}, args...))
			return []byte(realCodexJSONOutput), 0, nil
		},
	}

	env := testutil.NewEnv(t)
	step := config.RunStepDef{Type: "claude", Prompt: "do the work"}
	step.SetName("implement")

	ce := CodexEngine{engine: engine}
	sr := ce.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{NoCoordination: true}, engine, t.TempDir(), "", false)

	if !sr.Passed {
		t.Fatalf("CodexEngine.Run failed: %s", sr.Error)
	}

	// thread_id should be stored on the item under delivery.codex_session_id
	env.Reload(t)
	item, ok := env.S.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after run")
	}
	stored, _ := getNestedField(item, "delivery", "codex_session_id")
	if stored != fakeThreadID {
		t.Errorf("delivery.codex_session_id = %q, want %q", stored, fakeThreadID)
	}

	// First call must NOT be a resume
	if len(gotArgs) == 0 {
		t.Fatal("RunCodex not called")
	}
	for _, a := range gotArgs[0] {
		if a == "resume" {
			t.Errorf("first call should not contain 'resume': %v", gotArgs[0])
		}
	}
}

func TestCodexEngine_BuildsExecArgsJSONPositionalPromptSkipGitCheck(t *testing.T) {
	var capturedArgs [][]string
	engine := RunEngine{
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			capturedArgs = append(capturedArgs, append([]string{}, args...))
			return []byte(realCodexJSONOutput), 0, nil
		},
	}

	env := testutil.NewEnv(t)
	step := config.RunStepDef{Type: "claude", Prompt: "say hello"}
	step.SetName("implement")

	ce := CodexEngine{engine: engine}

	// First run
	_ = ce.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{NoCoordination: true}, engine, t.TempDir(), "", false)
	// Resume run
	_ = ce.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{NoCoordination: true}, engine, t.TempDir(), "019ec933-96db-78c2-adbb-f8218dea0795", true)

	if len(capturedArgs) < 2 {
		t.Fatalf("expected 2 RunCodex calls, got %d", len(capturedArgs))
	}

	firstArgs := capturedArgs[0]
	// Must contain: exec, --json, --skip-git-repo-check, --sandbox, read-only, <prompt>
	mustContain := []string{"--json", "--skip-git-repo-check"}
	for _, required := range mustContain {
		found := false
		for _, a := range firstArgs {
			if a == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("first call missing %q in args: %v", required, firstArgs)
		}
	}
	// Prompt must be positional (last arg, not after -p)
	lastArg := firstArgs[len(firstArgs)-1]
	if lastArg == "-p" || lastArg == "--profile" {
		t.Errorf("prompt appears to be a flag value, not positional: %v", firstArgs)
	}
	if !strings.Contains(lastArg, "say hello") {
		t.Errorf("last arg (prompt) = %q, want it to contain \"say hello\": %v", lastArg, firstArgs)
	}
	// First call must not contain 'resume'
	for _, a := range firstArgs {
		if a == "resume" {
			t.Errorf("first call should not contain 'resume': %v", firstArgs)
		}
	}

	// Resume call: must be [exec resume <thread_id> --json ...]
	secondArgs := capturedArgs[1]
	if len(secondArgs) < 3 || secondArgs[0] != "exec" || secondArgs[1] != "resume" {
		t.Errorf("resume args should start with [exec resume <id>]: %v", secondArgs)
	}
}

func TestCodexEngine_HandlesInvocationFailure(t *testing.T) {
	engine := RunEngine{
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return nil, 1, nil // non-zero exit code
		},
	}
	env := testutil.NewEnv(t)
	step := config.RunStepDef{Type: "claude"}
	step.SetName("implement")

	ce := CodexEngine{engine: engine}
	sr := ce.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{NoCoordination: true}, engine, t.TempDir(), "", false)

	if sr.Passed {
		t.Error("expected Passed=false on non-zero exit code")
	}
	if sr.Error == "" {
		t.Error("expected Error to be set on non-zero exit code")
	}
}

func TestCodexEngine_ResolvesOpenAIModelForPricing(t *testing.T) {
	// Output with turn.completed usage — should produce non-zero cost when
	// CodexModel is set to a model in the OpenAI pricing table.
	output := `{"type":"thread.started","thread_id":"test-thread-1"}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":0,"output_tokens":500}}
`
	engine := RunEngine{
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return []byte(output), 0, nil
		},
	}
	env := testutil.NewEnv(t)
	step := config.RunStepDef{Type: "claude"}
	step.SetName("implement")

	ce := CodexEngine{engine: engine}
	// Pass an explicit OpenAI model so EstimateOpenAICostUSD can look it up.
	sr := ce.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{
		NoCoordination: true,
		CodexModel:     "codex-mini-latest",
	}, engine, t.TempDir(), "", false)

	if !sr.Passed {
		t.Fatalf("CodexEngine.Run failed: %s", sr.Error)
	}
	if sr.CostUSD <= 0 {
		t.Errorf("CostUSD = %f, want > 0 (expected OpenAI estimated cost)", sr.CostUSD)
	}
}

func TestCodexUsage_CumulativeDeltaAccounting(t *testing.T) {
	env := testutil.NewEnv(t)
	sessionID := "sess-delta-test"

	// First turn: usage counts from this invocation.
	first := codexTokenCounts{Input: 120, Output: 80, CachedInput: 10}
	usage1, err := computeCodexUsageDelta(env.S, env.Cfg, "T-001", "codex-mini-latest", sessionID, first)
	if err != nil {
		t.Fatalf("first delta: %v", err)
	}
	// Previous cursors are all zero → delta == cur.
	if usage1.RegOutputTokens != 80 {
		t.Errorf("first delta RegOutputTokens = %d, want 80", usage1.RegOutputTokens)
	}
	if usage1.CachedInTokens != 10 {
		t.Errorf("first delta CachedInTokens = %d, want 10", usage1.CachedInTokens)
	}

	env.Reload(t)

	// Second invocation (resume): if codex re-emits prior turns cumulatively,
	// values will be higher. Delta should only record the new portion.
	second := codexTokenCounts{Input: 210, Output: 140, CachedInput: 10}
	usage2, err := computeCodexUsageDelta(env.S, env.Cfg, "T-001", "codex-mini-latest", sessionID, second)
	if err != nil {
		t.Fatalf("second delta: %v", err)
	}
	// Delta = second - first: input=90, output=60, cached=0.
	if usage2.RegOutputTokens != 60 {
		t.Errorf("second delta RegOutputTokens = %d, want 60", usage2.RegOutputTokens)
	}

	// Verify cursors are persisted for the next call.
	env.Reload(t)
	item, _ := env.S.Get("T-001")
	cursorKey := codexCursorKey(sessionID, "output")
	raw, _ := getNestedField(item, "delivery", cursorKey)
	if n, _ := strconv.Atoi(strings.TrimSpace(raw)); n != 140 {
		t.Errorf("persisted cursor output = %d, want 140", n)
	}
}

func TestSessionLog_CodexEstimatedCostRecorded(t *testing.T) {
	env := testutil.NewEnv(t)
	SaveStack(env.Cfg, []StackEntry{{ID: "T-001"}})

	p := SessionLogPayload{
		Provider:        AIProviderOpenAI,
		SessionID:       "codex-sess-1",
		Model:           "codex-mini-latest",
		ProcessMs:       30_000,
		AIMs:            28_000,
		RegInputTokens:  1000,
		RegOutputTokens: 500,
		CostSource:      CostSourceEstimated,
	}
	if code := SessionLog(env.S, env.Cfg, p); code != 0 {
		t.Fatalf("SessionLog exit=%d", code)
	}

	env.Reload(t)
	item, _ := env.S.Get("T-001")

	cost := readFloatField(item, "time_tracking", "ai_cost_usd")
	if cost <= 0 {
		t.Errorf("ai_cost_usd = %f, want > 0 (expected OpenAI estimated cost)", cost)
	}

	assertInt(t, item, "time_tracking", "reg_input_tokens", 1000)
	assertInt(t, item, "time_tracking", "reg_output_tokens", 500)
}
