package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// dedupVerdict is the JSON shape returned by the semantic dedup LLM.
type dedupVerdict struct {
	MatchID string `json:"match_id"` // existing item ID, or empty/none
	Reason  string `json:"reason"`
}

// runSemanticDedup checks whether a proposed new issue/task is substantively
// similar to an existing open item. If it is, an observation is recorded on
// the existing item (instead of creating a new one), the existing item's
// priority is bumped, and the existing ID is returned.
//
// Returns ("", nil) when no match is found — caller proceeds with normal creation.
// Returns (existingID, nil) when merged — caller should exit 0 without creating.
// Returns ("", err) on hard failures — caller proceeds with normal creation
// (degrade gracefully; a transient LLM hiccup must not block creates).
func runSemanticDedup(s *store.Store, cfg *config.Config, itemType, title, situation string, engine RunEngine) (matchedID string, err error) {
	if engine.RunClaude == nil {
		return "", nil
	}
	if os.Getenv("AS_INTERNAL_NO_DEDUP") == "1" {
		return "", nil
	}
	if situation == "" {
		return "", nil
	}
	// Only deduplicate issues and tasks; ideas/goals don't need this.
	if itemType != "issue" && itemType != "task" {
		return "", nil
	}

	candidates := dedupCandidates(s, cfg, itemType, title, situation)
	if len(candidates) == 0 {
		return "", nil
	}

	prompt := buildDedupPrompt(title, situation, candidates)
	permMode := cfg.RunPermissionMode()
	var permArgs []string
	if permMode == "dangerously-skip-permissions" || permMode == "" {
		permArgs = []string{"--dangerously-skip-permissions"}
	} else {
		permArgs = []string{"--permission-mode", permMode}
	}
	args := append([]string{"-p", prompt, "--output-format", "json"}, permArgs...)
	env := []string{"AS_CLAUDE_WALL_TIMEOUT=90s"}

	out, exitCode, runErr := engine.RunClaude(cfg.Root(), args, env)
	if runErr != nil || exitCode != 0 {
		fmt.Fprintf(os.Stderr, "warning: semantic dedup skipped (subprocess exit %d: %v)\n", exitCode, runErr)
		return "", nil
	}

	var v dedupVerdict
	if parseErr := json.Unmarshal(out, &v); parseErr != nil {
		fmt.Fprintf(os.Stderr, "warning: semantic dedup skipped (could not parse response: %v)\n", parseErr)
		return "", nil
	}

	mid := strings.TrimSpace(v.MatchID)
	if mid == "" || strings.ToLower(mid) == "none" {
		return "", nil
	}

	// Validate the matched ID is a real, open item.
	existing, ok := s.Get(mid)
	if !ok {
		fmt.Fprintf(os.Stderr, "warning: semantic dedup returned unknown ID %q — proceeding with create\n", mid)
		return "", nil
	}
	if isTerminal(existing, cfg) {
		// Matched a closed item — still valid to create a new one.
		return "", nil
	}

	// Record the observation and bump priority.
	if mergeErr := recordObservation(s, cfg, mid, situation); mergeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record observation on %s: %v — proceeding with create\n", mid, mergeErr)
		return "", nil
	}

	return mid, nil
}

// dedupCandidates returns a filtered, scored list of open items to compare
// against. Pre-filters by keyword overlap so the LLM prompt stays small and
// the check stays fast.
func dedupCandidates(s *store.Store, cfg *config.Config, itemType, title, situation string) []dedupCandidate {
	keywords := extractKeywords(title + " " + situation)
	if len(keywords) == 0 {
		return nil
	}

	var scored []struct {
		item  *model.Item
		score int
	}

	openStatuses := map[string]bool{"queued": true, "active": true, "awaiting_decision": true}
	for _, item := range s.List(store.TypeFilter(itemType)) {
		if !openStatuses[item.Status] {
			continue
		}
		hay := strings.ToLower(item.Title + " " + item.SBAR.Situation)
		hits := 0
		for _, kw := range keywords {
			if strings.Contains(hay, kw) {
				hits++
			}
		}
		if hits >= 2 {
			scored = append(scored, struct {
				item  *model.Item
				score int
			}{item, hits})
		}
	}

	// Sort descending by keyword overlap score; take top 20.
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) > 20 {
		scored = scored[:20]
	}

	out := make([]dedupCandidate, 0, len(scored))
	for _, s := range scored {
		sit := s.item.SBAR.Situation
		if len(sit) > 200 {
			sit = sit[:200] + "…"
		}
		out = append(out, dedupCandidate{ID: s.item.ID, Title: s.item.Title, Situation: sit})
	}
	return out
}

type dedupCandidate struct {
	ID        string
	Title     string
	Situation string
}

// extractKeywords returns lowercase tokens longer than 3 chars that are
// likely to be meaningful identifiers (package names, CVE IDs, function
// names, etc.). Strips punctuation except `-`, `/`, `@`, `.`.
func extractKeywords(text string) []string {
	lower := strings.ToLower(text)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return unicode.IsSpace(r) || (r != '-' && r != '/' && r != '@' && r != '.' && unicode.IsPunct(r))
	})
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		if len(w) > 3 && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func buildDedupPrompt(title, situation string, candidates []dedupCandidate) string {
	var sb strings.Builder
	sb.WriteString("You are a deduplication assistant. A new issue is about to be created:\n\n")
	sb.WriteString("TITLE: ")
	sb.WriteString(title)
	sb.WriteString("\nSITUATION: ")
	sb.WriteString(situation)
	sb.WriteString("\n\nExisting open issues to compare against:\n")
	for _, c := range candidates {
		sb.WriteString(fmt.Sprintf("\n%s | %s | %s", c.ID, c.Title, c.Situation))
	}
	sb.WriteString(`

Does any existing issue describe the SAME underlying problem — same root cause, same component, same vulnerability, or same broken behavior? Minor wording differences or different triggering tasks do NOT make them different issues.

Return ONLY a JSON object, no other text:
{"match_id":"<ID or empty string>","reason":"<one sentence>"}

If there is a clear match, set match_id to the item ID (e.g. "I-1002").
If there is no clear match, set match_id to "" (empty string).
When in doubt, return "".`)
	return sb.String()
}

// recordObservation appends a timestamped observation to the existing item
// and bumps its priority if it has been hit enough times.
func recordObservation(s *store.Store, cfg *config.Config, id, situation string) error {
	now := time.Now().Format(time.RFC3339)

	// Determine trigger item from stack top (best-effort).
	triggerID := ""
	stack := LoadStack(cfg)
	if len(stack) > 0 {
		triggerID = stack[0].ID
	}

	note := situation
	if len(note) > 160 {
		note = note[:160] + "…"
	}
	// Escape pipe in note so the pipe-delimited format stays parseable.
	note = strings.ReplaceAll(note, "|", ";")

	entry := fmt.Sprintf("%s | %s | %s", now, triggerID, note)

	return s.Mutate(id, func(item *model.Item) error {
		item.Observations = append(item.Observations, entry)
		updateListInDoc(item, "observations", item.Observations)

		// Auto-bump priority: first re-hit → p1; third re-hit → p0.
		if item.Priority != nil {
			hits := len(item.Observations)
			newPrio := *item.Priority
			switch {
			case hits >= 3 && newPrio > 0:
				newPrio = 0
			case hits >= 1 && newPrio > 1:
				newPrio = 1
			}
			if newPrio != *item.Priority {
				item.Priority = &newPrio
				item.Doc.SetField("priority", fmt.Sprintf("%d", newPrio))
				fmt.Printf("[%s] priority bumped to p%d (%d observations)\n", id, newPrio, hits)
			}
		}
		return nil
	})
}

