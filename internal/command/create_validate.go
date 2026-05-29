package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
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
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}

	out, exitCode, err := engine.RunClaude(cfg.Root(), args, nil)
	if err != nil || (exitCode != 0 && len(out) == 0) {
		fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (subprocess exit %d: %v)\n", exitCode, err)
		return false, nil
	}

	// Parse JSON — look for the sbarVerdict shape; tolerate extra text.
	var verdict sbarVerdict
	if parseErr := json.Unmarshal(out, &verdict); parseErr != nil {
		// Try to find JSON object in stream output
		raw := string(out)
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start >= 0 && end > start {
			if parseErr2 := json.Unmarshal([]byte(raw[start:end+1]), &verdict); parseErr2 != nil {
				fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (could not parse response)\n")
				return false, nil
			}
		} else {
			fmt.Fprintf(os.Stderr, "warning: SBAR semantic validation skipped (no JSON in response)\n")
			return false, nil
		}
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
