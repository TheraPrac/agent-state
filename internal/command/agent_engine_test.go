package command

import (
	"encoding/json"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

func TestRunOpts_AgentEngineDefaultsToClaude(t *testing.T) {
	var opts RunOpts
	if opts.AgentEngine != "" {
		t.Errorf("AgentEngine default = %q, want empty string (claude)", opts.AgentEngine)
	}
	engine := selectAgentEngine(opts, RunEngine{})
	if engine.Name() != "claude" {
		t.Errorf("selectAgentEngine default = %q, want \"claude\"", engine.Name())
	}
}

func TestValidateAgentEngine(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"", true},
		{"claude", true},
		{"codex", true},
		{"openai", false},
		{"gpt-4", false},
	} {
		err := ValidateAgentEngine(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ValidateAgentEngine(%q) = %v, want nil", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateAgentEngine(%q) = nil, want error", tc.in)
		}
	}
}

// TestExecuteStep_RoutesToSelectedAgentEngine verifies execAgentRaw dispatches
// to the right runner based on the agent engine name.
func TestExecuteStep_RoutesToSelectedAgentEngine(t *testing.T) {
	var claudeCalled, codexCalled bool
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			claudeCalled = true
			result := ClaudeResult{Type: "result", Subtype: "success", Result: "done"}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			codexCalled = true
			return []byte("done"), 0, nil
		},
	}

	t.Run("codex engine calls RunCodex", func(t *testing.T) {
		codexCalled = false
		_, _, _ = execAgentRaw(engine, "codex", t.TempDir(), []string{"exec", "-p", "hi"}, nil)
		if !codexCalled {
			t.Error("execAgentRaw(codex) did not call RunCodex")
		}
	})

	t.Run("claude engine calls RunClaude", func(t *testing.T) {
		claudeCalled = false
		_, _, _ = execAgentRaw(engine, "claude", t.TempDir(), []string{"--print", "hi"}, nil)
		if !claudeCalled {
			t.Error("execAgentRaw(claude) did not call RunClaude")
		}
	})

	t.Run("empty engine calls RunClaude", func(t *testing.T) {
		claudeCalled = false
		_, _, _ = execAgentRaw(engine, "", t.TempDir(), []string{"--print", "hi"}, nil)
		if !claudeCalled {
			t.Error("execAgentRaw('') did not call RunClaude")
		}
	})
}

// TestClaudeEngine_BehaviorUnchanged verifies that ClaudeEngine.Run produces a
// passing StepResult when the underlying RunClaude returns success JSON.
func TestClaudeEngine_BehaviorUnchanged(t *testing.T) {
	var gotArgs []string
	engine := RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			gotArgs = args
			result := ClaudeResult{
				Type:    "result",
				Subtype: "success",
				Result:  "done",
				Usage: ClaudeUsage{
					InputTokens:  100,
					OutputTokens: 50,
				},
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
	}

	env := testutil.NewEnv(t)
	step := config.RunStepDef{Type: "claude", Prompt: "do the work"}
	step.SetName("implement")

	ae := ClaudeEngine{}
	sr := ae.Run(env.S, env.Cfg, "T-001", "", step, RunOpts{NoCoordination: true}, engine, t.TempDir(), "", false)

	if !sr.Passed {
		t.Errorf("ClaudeEngine.Run failed: %s", sr.Error)
	}
	found := false
	for _, a := range gotArgs {
		if a == "--print" || a == "-p" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ClaudeEngine args missing --print/-p flag: %v", gotArgs)
	}
}

// TestAgentEngineFlagWired is a compile-time assertion: RunOpts and PrepOpts
// both expose AgentEngine. Removing either field breaks this test.
func TestAgentEngineFlagWired(t *testing.T) {
	_ = RunOpts{AgentEngine: "claude"}
	_ = PrepOpts{AgentEngine: "codex"}
}

// TestPrep_RoutesToSelectedAgentEngine verifies that PrepOpts.AgentEngine is
// passed through so the prep path calls the right subprocess runner.
func TestPrep_RoutesToSelectedAgentEngine(t *testing.T) {
	var codexCalled bool
	engine := RunEngine{
		RunCodex: func(cwd string, args []string, env []string) ([]byte, int, error) {
			codexCalled = true
			// Return a minimal plan-looking text so prepItem doesn't crash.
			return []byte("## Plan\nDo the work.\n\n## Acceptance Criteria\n- cmd: echo ok\n"), 0, nil
		},
		PromptUser: func(prompt string) (string, error) {
			return "a\n", nil // accept
		},
		SelectMenu: func(prompt string, options []menuOption, defaultIdx int) string {
			if len(options) > 0 {
				return options[0].Key
			}
			return ""
		},
		ConfirmPrompt: func(prompt string) bool { return true },
	}

	env := testutil.NewEnv(t)
	opts := PrepOpts{AgentEngine: "codex", WriteOnly: true, ItemFilter: "T-001"}

	code := PrepStandalone(env.S, env.Cfg, "T-001", opts, engine)
	_ = code // may not be 0 without a real codex binary; we only care that RunCodex was called
	if !codexCalled {
		t.Error("PrepStandalone with AgentEngine=codex did not call RunCodex")
	}
}
