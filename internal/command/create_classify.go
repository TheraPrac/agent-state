package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// classifyVerdict is the JSON shape returned by the goal-classifier LLM.
type classifyVerdict struct {
	GoalIDs []string `json:"goal_ids"`
	Reason  string   `json:"reason"`
}

// classifyGoals returns the IDs of active goals that best match the new item,
// and a bool indicating whether the LLM was actually invoked (true) or the
// function returned early without running (false). The bool allows the caller
// to distinguish "no match found by the LLM" from "classify did not run".
//
// When engine.RunClaude is nil (tests, migrations, in-process callers) or the
// item type is not task/issue, returns nil, false, nil — a silent no-op.
// On LLM error, returns nil, true, nil and prints a warning (graceful degradation).
func classifyGoals(s *store.Store, cfg *config.Config, itemType, title, situation string, engine RunEngine) ([]string, bool, error) {
	if engine.RunClaude == nil {
		return nil, false, nil
	}
	if os.Getenv("AS_INTERNAL_NO_CLASSIFY") == "1" {
		return nil, false, nil
	}
	if itemType != "task" && itemType != "issue" {
		return nil, false, nil
	}

	goals := s.List(store.TypeFilter("goal"), store.StatusFilter("active"))
	if len(goals) == 0 {
		return nil, false, nil
	}

	prompt := buildClassifyPrompt(title, situation, goals)
	permMode := cfg.RunPermissionMode()
	var permArgs []string
	if permMode == "dangerously-skip-permissions" || permMode == "" {
		permArgs = []string{"--dangerously-skip-permissions"}
	} else {
		permArgs = []string{"--permission-mode", permMode}
	}
	args := append([]string{"-p", prompt, "--output-format", "json"}, permArgs...)
	env := []string{"AS_CLAUDE_WALL_TIMEOUT=60s"}

	out, exitCode, runErr := engine.RunClaude(cfg.Root(), args, env)
	if runErr != nil || exitCode != 0 {
		fmt.Fprintf(os.Stderr, "warning: goal auto-classify skipped (subprocess exit %d: %v)\n", exitCode, runErr)
		return nil, true, nil
	}

	// defaultRunClaude returns the ClaudeResult envelope line from
	// claude's stream-JSON output. The model's actual text lives inside
	// ClaudeResult.Result as a string — unmarshal that, not the envelope.
	cr, crErr := parseClaudeOutput(out)
	if crErr != nil {
		fmt.Fprintf(os.Stderr, "warning: goal auto-classify skipped (could not parse result envelope: %v)\n", crErr)
		return nil, true, nil
	}

	var v classifyVerdict
	if parseErr := json.Unmarshal([]byte(cr.Result), &v); parseErr != nil {
		fmt.Fprintf(os.Stderr, "warning: goal auto-classify skipped (could not parse verdict: %v)\n", parseErr)
		return nil, true, nil
	}

	// Validate — filter out any IDs the LLM hallucinated.
	goalSet := make(map[string]struct{}, len(goals))
	for _, g := range goals {
		goalSet[g.ID] = struct{}{}
	}
	var matched []string
	for _, gid := range v.GoalIDs {
		gid = strings.TrimSpace(gid)
		if _, ok := goalSet[gid]; ok {
			matched = append(matched, gid)
		}
	}

	if len(matched) == 0 {
		fmt.Fprintf(os.Stderr, "warning: no active goal matched %q — item created without a goal link (add one: st item goals add <id> <goal-id>)\n", title)
	}

	return matched, true, nil
}

// truncateRunes truncates s to at most n runes, appending "…" when truncated.
// Operates on rune boundaries to avoid producing invalid UTF-8.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}

func buildClassifyPrompt(title, situation string, goals []*model.Item) string {
	var sb strings.Builder
	sb.WriteString("You are classifying a new work item into one or more strategic goals.\n\n")
	sb.WriteString("New item:\n")
	sb.WriteString("  title: " + title + "\n")
	if situation != "" {
		sb.WriteString("  situation: " + truncateRunes(strings.TrimSpace(situation), 300) + "\n")
	}
	sb.WriteString("\nActive goals:\n")
	for _, g := range goals {
		line := fmt.Sprintf("  %s: %s", g.ID, g.Title)
		if g.SBAR.Situation != "" {
			line += " — " + truncateRunes(strings.TrimSpace(g.SBAR.Situation), 150)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(`
Return a JSON object with exactly this shape:
{"goal_ids": ["G-XXX"], "reason": "one-sentence explanation"}

Rules:
- goal_ids must be a subset of the goal IDs listed above (empty array if none match).
- Assign only goals that clearly match — when in doubt, return [].
- Multiple goals are allowed only when the item genuinely spans both domains.
- Do NOT invent goal IDs not listed above.
`)
	return sb.String()
}
