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

// validateSBARSemantic runs the Layer-2+3 LLM-backed SBAR validation via a
// Claude subprocess. Returns (blocked=true, findings) on FAIL;
// (blocked=false, findings) on WARN; (blocked=false, nil) on PASS or skip.
//
// Degrades gracefully: engine not wired, subprocess error, or unparseable
// output → skip with a stderr warning and return (false, nil). A transient
// LLM failure must not block a Layer-1-clean item.
//
// I-908.
func validateSBARSemantic(cfg *config.Config, engine RunEngine, sbar model.SBAR) (blocked bool, findings []string) {
	if engine.RunClaude == nil {
		return false, nil // in-process caller without engine, skip
	}

	prompt := buildSBARValidationPrompt(sbar)

	// Respect the operator-configured permission mode (same pattern as buildClaudeArgs
	// in run.go lines 3631–3634). Hardcoding --dangerously-skip-permissions would
	// override any `run.permission_mode` setting in .as/config.yaml.
	permMode := cfg.RunPermissionMode()
	var permArgs []string
	if permMode == "dangerously-skip-permissions" || permMode == "" {
		permArgs = []string{"--dangerously-skip-permissions"}
	} else {
		permArgs = []string{"--permission-mode", permMode}
	}

	args := append([]string{"-p", prompt, "--output-format", "json"}, permArgs...)

	// 2-minute wall timeout — matches the pattern from I-985 (plan review uses
	// AS_CLAUDE_WALL_TIMEOUT too). Without this the nil env would use the
	// 2-hour maxWallTimeout in defaultRunClaude, blocking st create for up to
	// 2 hours on a degraded LLM API.
	env := []string{"AS_CLAUDE_WALL_TIMEOUT=2m"}

	out, exitCode, err := engine.RunClaude(cfg.Root(), args, env)
	if err != nil || exitCode != 0 {
		// Any subprocess failure (crash, timeout, non-zero exit) degrades to skip.
		// Previously only skipped when exitCode!=0 AND len(out)==0; partial output
		// from a crashed process could have been misinterpreted as a verdict.
		fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (subprocess exit %d: %v)\n", exitCode, err)
		return false, nil
	}

	// Parse JSON. --output-format json produces a single object so the primary
	// parse should always succeed. If it fails (unexpected wrapper or encoding),
	// degrade to skip rather than attempting a brace-scan fallback that could
	// mis-span nested JSON within findings strings.
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
