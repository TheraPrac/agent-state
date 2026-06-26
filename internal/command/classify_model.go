package command

import (
	"fmt"

	"github.com/theraprac/agent-state/internal/classify"
	"github.com/theraprac/agent-state/internal/config"
)

// ClaudeModel is the production classify.Model implementation: it
// builds a prompt from the inputs, dispatches a `claude -p` subprocess
// via the RunEngine, parses the JSON envelope, then parses the model's
// inner stdout text as a verdict payload. Lives in the command package
// (not internal/classify) because it depends on the run.go subprocess
// helpers — moving those to a shared package isn't justified for a
// single consumer.
type ClaudeModel struct {
	Cfg      *config.Config
	Engine   RunEngine
	Opts     RunOpts // permission mode, model, budget
	WorkDir  string  // cwd passed to claude; typically the active worktree
	Examples []classify.CorpusEntry
}

// Classify implements classify.Model. Sequence:
//
//  1. Build the classifier prompt from Inputs + corpus examples.
//  2. Build claude argv via buildClaudeArgs (reuse run.go helper).
//  3. Dispatch via engine.RunClaude(cwd, args, env). Test code
//     injects a mock engine here; production uses defaultRunClaude.
//  4. Parse the wrapping envelope via parseClaudeOutput → ClaudeResult.
//  5. Pull the model's stdout text out of ClaudeResult.Result and
//     parse it as a verdict payload via classify.ParseModelOutput.
//
// Errors at any stage propagate up; the orchestrating Classifier
// wraps them as "model classify: <err>".
func (m *ClaudeModel) Classify(in classify.Inputs) (classify.Result, error) {
	if m.Engine.RunClaude == nil {
		return classify.Result{}, fmt.Errorf("ClaudeModel: Engine.RunClaude is nil")
	}
	prompt := classify.BuildPrompt(in, m.Examples)

	cwd := m.WorkDir
	if cwd == "" && m.Cfg != nil {
		cwd = m.Cfg.Root()
	}

	args := buildClaudeArgs(m.Cfg, prompt, m.Opts, cwd)
	sessionID := generateSessionID()
	env := []string{"AS_SESSION_ID=" + sessionID}
	if m.Cfg != nil {
		if agentID := m.Cfg.AgentID(); agentID != "" {
			env = append(env, "AS_AGENT_ID="+agentID)
		}
	}

	output, exitCode, err := m.Engine.RunClaude(cwd, args, env)
	if err != nil {
		return classify.Result{}, fmt.Errorf("claude exec: %w", err)
	}
	if exitCode != 0 {
		return classify.Result{}, fmt.Errorf("claude exit %d: %s", exitCode, truncateOutput(string(output)))
	}

	parsed, err := parseClaudeOutput(output)
	if err != nil {
		return classify.Result{}, fmt.Errorf("parse envelope: %w", err)
	}
	if parsed.IsError {
		return classify.Result{}, fmt.Errorf("claude reported error: %v", parsed.Errors)
	}
	// Match proposePlan/executeClaude behavior: a non-empty subtype that
	// isn't "success" is a model-side failure even when is_error is false.
	if parsed.Subtype != "" && parsed.Subtype != "success" {
		return classify.Result{}, fmt.Errorf("claude returned subtype %q: %v", parsed.Subtype, parsed.Errors)
	}

	res, err := classify.ParseModelOutput(parsed.Result)
	if err != nil {
		return classify.Result{}, fmt.Errorf("parse verdict: %w", err)
	}
	return res, nil
}

// truncateOutput caps stderr bleed in error messages so a multi-KB
// claude failure doesn't end up on every operator's terminal.
func truncateOutput(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// newDefaultClaudeModel constructs a ClaudeModel that uses the real
// `claude -p` subprocess via DefaultRunEngine. CLI production wiring
// calls this from Classify when opts.Model is nil. Tests inject their
// own classify.Model via opts.Model and never hit this path.
//
// Forces permission mode "plan" — the classifier's only legitimate
// output is a JSON verdict; it never needs to edit files or shell out.
// Pinning the mode here defends against an operator running with
// cfg.RunPermissionMode() = "dangerously-skip-permissions" globally;
// without this override, a misbehaving model could fire tool calls
// during classification.
func newDefaultClaudeModel(cfg *config.Config) *ClaudeModel {
	return &ClaudeModel{
		Cfg:    cfg,
		Engine: DefaultRunEngine(),
		Opts:   RunOpts{PermissionMode: "plan"},
	}
}
