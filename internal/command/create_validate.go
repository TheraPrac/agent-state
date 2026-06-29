package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// sbarVerdict is the JSON shape returned by the Layer-2+3 semantic validator.
type sbarVerdict struct {
	Verdict  string   `json:"verdict"`  // "PASS", "WARN", or "FAIL"
	Findings []string `json:"findings"` // per-field explanations
}

// validateSBARSemantic runs the Layer-2+3 LLM-backed SBAR validation.
// Returns (blocked=true, findings) on FAIL; (blocked=false, findings) on WARN;
// (blocked=false, nil) on PASS or skip.
//
// Execution path (in priority order):
//  1. engine.ValidateFunc — direct Anthropic API call; no CLI cold-start or
//     hook overhead. Wired by DefaultRunEngine() in production.
//  2. engine.RunClaude — legacy Claude subprocess fallback; used by tests
//     that inject RunClaude without setting ValidateFunc.
//  3. Both nil — skip gracefully (in-process callers without an engine).
//
// Degrades gracefully: any API/subprocess error → skip with a stderr warning
// and return (false, nil). A transient LLM failure must not block a
// Layer-1-clean item.
//
// I-908, I-1612.
func validateSBARSemantic(cfg *config.Config, engine RunEngine, sbar model.SBAR) (blocked bool, findings []string) {
	if engine.ValidateFunc == nil && engine.RunClaude == nil {
		return false, nil
	}

	prompt := buildSBARValidationPrompt(sbar)

	// buildCLIArgs constructs the subprocess arguments for the RunClaude fallback path.
	buildCLIArgs := func() []string {
		// Respect the operator-configured permission mode (same pattern as
		// buildClaudeArgs in run.go lines 3631–3634).
		permMode := cfg.RunPermissionMode()
		var permArgs []string
		if permMode == "dangerously-skip-permissions" || permMode == "" {
			permArgs = []string{"--dangerously-skip-permissions"}
		} else {
			permArgs = []string{"--permission-mode", permMode}
		}
		return append([]string{"-p", prompt, "--output-format", "json"}, permArgs...)
	}
	// 2-minute wall timeout — without this the nil env would use the
	// 2-hour maxWallTimeout in defaultRunClaude (I-985).
	cliEnv := []string{"AS_CLAUDE_WALL_TIMEOUT=2m"}

	var out []byte
	if engine.ValidateFunc != nil {
		// Fast path: direct API call, no CLI subprocess.
		apiModel := cfg.ValidationModel()
		var apiErr error
		out, apiErr = engine.ValidateFunc(apiModel, prompt)
		if apiErr != nil {
			// API failed — fall back to CLI subprocess before degrading.
			if engine.RunClaude != nil {
				var exitCode int
				var clErr error
				out, exitCode, clErr = engine.RunClaude(cfg.Root(), buildCLIArgs(), cliEnv)
				if clErr != nil || exitCode != 0 {
					fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (API: %v; CLI exit %d: %v)\n", apiErr, exitCode, clErr)
					return false, nil
				}
			} else {
				fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (API call failed: %v)\n", apiErr)
				return false, nil
			}
		}
	} else {
		// CLI-only path: tests that inject RunClaude without ValidateFunc.
		var exitCode int
		var err error
		out, exitCode, err = engine.RunClaude(cfg.Root(), buildCLIArgs(), cliEnv)
		if err != nil || exitCode != 0 {
			fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (subprocess exit %d: %v)\n", exitCode, err)
			return false, nil
		}
	}

	// Parse JSON verdict. Both paths return raw JSON matching sbarVerdict.
	// Degrade to skip on parse failure rather than attempting a brace-scan
	// fallback that could mis-span nested JSON within findings strings.
	var verdict sbarVerdict
	if parseErr := json.Unmarshal(out, &verdict); parseErr != nil {
		fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (could not parse response: %v)\n", parseErr)
		return false, nil
	}

	switch strings.ToUpper(strings.TrimSpace(verdict.Verdict)) {
	case "FAIL":
		return true, verdict.Findings
	case "WARN":
		return false, verdict.Findings
	default:
		return false, nil
	}
}

// buildSBARValidationPrompt constructs the prompt for the Layer-2+3 LLM
// semantic SBAR validator.
func buildSBARValidationPrompt(sbar model.SBAR) string {
	return fmt.Sprintf(`You are a technical quality reviewer validating an SBAR (Situation-Background-Assessment-Recommendation) for a software work item.

Evaluate each field for:
1. Specificity: concrete details (file paths, function names, line numbers, error messages, metric values)
2. Evidence-grounding: claims backed by observable facts rather than assumptions
3. Actionability (recommendation only): specific enough to execute without further clarification

SBAR:
Situation: %s

Background: %s

Assessment: %s

Recommendation: %s

Return ONLY a JSON object, no other text:
{"verdict":"PASS"|"WARN"|"FAIL","findings":["description of issue 1",...]}

Verdicts:
- PASS: all fields are specific and grounded. findings may be empty.
- WARN: minor gaps (e.g. recommendation could be more specific). findings explains.
- FAIL: significant gaps — vague or unverifiable claims, no file/function refs for code changes, assessment missing root cause. findings explains.

Short but specific SBARs can PASS. Long vague SBARs should FAIL.`,
		sbar.Situation, sbar.Background, sbar.Assessment, sbar.Recommendation)
}
