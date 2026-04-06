package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Update(s *store.Store, cfg *config.Config, id, field, value string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	// Block status changes on locked items (being worked on by a pipeline).
	// Use `st unlock <id>` to force-release the lock first.
	if field == "status" && store.IsLocked(cfg, id) {
		fmt.Fprintf(os.Stderr, "%s is locked (active pipeline) — use `st unlock %s` first\n", id, id)
		return 1
	}

	// List fields — replace entire block instead of appending
	listFields := map[string]bool{
		"acceptance_criteria": true, "depends_on": true, "blocks": true,
		"related_issues": true, "next_actions": true, "resolution": true,
		"invariants": true, "doc_changes": true, "linked_plans": true,
	}

	var oldValue string
	isMultiline := strings.Contains(value, "\n")
	isDotted := strings.Contains(field, ".")

	if isMultiline && (listFields[field] || isDotted) {
		// Multiline value = list/block replacement.
		// Preserve indentation — TrimSpace would destroy YAML structure
		// for continuation lines (e.g., "  command:" under "- description:").
		var lines []string
		for _, line := range strings.Split(value, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, line)
			}
		}
		item.Doc.ReplaceList(field, lines)
	} else if isDotted {
		oldValue, _ = item.Doc.GetNestedField(field)
		item.Doc.SetNestedField(field, value)
	} else {
		oldValue, _ = item.Doc.GetField(field)
		item.Doc.SetField(field, value)
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "update", Field: field,
		OldValue: oldValue, NewValue: value,
	})

	fmt.Printf("Updated %s.%s\n", id, field)

	// Commit + push the update so it can't be silently reverted by a
	// subsequent command's pre-run GitPull or lost to a multi-agent race.
	// Best-effort: a sync failure still returns 0 because the disk state
	// is correct and a later sync will carry the commit forward.
	if err := s.GitSync(fmt.Sprintf("st update: %s.%s", id, field)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after update failed: %v\n", err)
	}
	return 0
}
