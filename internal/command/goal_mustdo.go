package command

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// GoalMustDoAdd adds itemIDs to the given bucket of goalID's must_do set.
// bucket="" means uncategorized (flat-list form). Returns exit code.
func GoalMustDoAdd(s *store.Store, cfg *config.Config, goalID, bucket string, itemIDs []string) int {
	goal, ok := s.Get(goalID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", goalID)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "must-do add: %s is not a goal (type=%s)\n", goalID, goal.Type)
		return 1
	}

	// Validate all itemIDs exist and are not already in must_do.
	for _, id := range itemIDs {
		if _, exists := s.Get(id); !exists {
			fmt.Fprintf(os.Stderr, "must-do add: item %s not found\n", id)
			return 1
		}
		if bucketOf(goal.MustDo, id) != "" {
			// bucketOf returns "" if not found, non-empty string if found
			// (non-empty means it IS in must_do)
		}
		if _, b := findInMustDo(goal.MustDo, id); b {
			fmt.Fprintf(os.Stderr, "must-do add: %s is already in must_do of %s\n", id, goalID)
			return 1
		}
	}

	if err := s.Mutate(goalID, func(it *model.Item) error {
		if it.MustDo == nil {
			it.MustDo = make(map[string][]string)
		}
		for _, id := range itemIDs {
			it.MustDo[bucket] = append(it.MustDo[bucket], id)
		}
		if it.Doc != nil {
			it.Doc.SetMustDo(it.MustDo)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "must-do add %s: %v\n", goalID, err)
		return 1
	}

	for _, id := range itemIDs {
		bucketLabel := bucket
		if bucketLabel == "" {
			bucketLabel = "(uncategorized)"
		}
		_ = changelog.Append(cfg, goalID, changelog.Entry{
			Op:       "must_do_add",
			Field:    "must_do",
			NewValue: id + " bucket=" + bucketLabel,
			Agent:    cfg.Identity().ID,
		})
		fmt.Printf("Added %s to %s must_do[%s]\n", id, goalID, bucketLabel)
	}
	return 0
}

// GoalMustDoRemove removes itemIDs from any bucket in goalID's must_do set.
// Empty buckets are pruned. Returns exit code.
func GoalMustDoRemove(s *store.Store, cfg *config.Config, goalID string, itemIDs []string) int {
	goal, ok := s.Get(goalID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", goalID)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "must-do remove: %s is not a goal\n", goalID)
		return 1
	}

	for _, id := range itemIDs {
		if _, found := findInMustDo(goal.MustDo, id); !found {
			fmt.Fprintf(os.Stderr, "must-do remove: %s not in must_do of %s\n", id, goalID)
			return 1
		}
	}

	if err := s.Mutate(goalID, func(it *model.Item) error {
		for _, id := range itemIDs {
			removeFromMustDo(it.MustDo, id)
		}
		// Prune empty buckets.
		for bucket, ids := range it.MustDo {
			if len(ids) == 0 {
				delete(it.MustDo, bucket)
			}
		}
		if it.Doc != nil {
			it.Doc.SetMustDo(it.MustDo)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "must-do remove %s: %v\n", goalID, err)
		return 1
	}

	for _, id := range itemIDs {
		fmt.Printf("Removed %s from %s must_do\n", id, goalID)
	}
	return 0
}

// GoalMustDoList lists the must_do items for goalID with per-item status.
func GoalMustDoList(s *store.Store, cfg *config.Config, goalID string) int {
	return goalMustDoListTo(os.Stdout, s, cfg, goalID)
}

func goalMustDoListTo(w io.Writer, s *store.Store, cfg *config.Config, goalID string) int {
	goal, ok := s.Get(goalID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", goalID)
		return 1
	}
	if goal.Type != "goal" {
		fmt.Fprintf(os.Stderr, "must-do list: %s is not a goal\n", goalID)
		return 1
	}

	if len(goal.MustDo) == 0 {
		fmt.Fprintln(w, "(must_do is empty)")
		return 0
	}

	// Build sorted bucket key list.
	bucketKeys := sortedBucketKeys(goal.MustDo)

	totalDone := 0
	totalAll := 0

	for _, bucket := range bucketKeys {
		ids := goal.MustDo[bucket]
		if bucket == "" {
			fmt.Fprintln(w, "uncategorized:")
		} else {
			fmt.Fprintf(w, "%s:\n", bucket)
		}
		for _, id := range ids {
			item, exists := s.Get(id)
			statusStr := "???"
			titleStr := ""
			isDone := false
			if exists {
				statusStr = item.Status
				titleStr = item.Title
				isDone = isTerminalStatus(item.Status)
			}
			doneMarker := " "
			if isDone {
				doneMarker = "✓"
				totalDone++
			}
			totalAll++
			fmt.Fprintf(w, "  [%s] %-8s  %-6s  %s\n", doneMarker, id, statusStr, titleStr)
		}
	}
	fmt.Fprintf(w, "\n%d/%d done\n", totalDone, totalAll)
	return 0
}

// GoalMustDoListAllIDs prints all item IDs in any goal's must_do, one per line.
// Used by drain-checkpoint to check structural placement without calling st binary.
func GoalMustDoListAllIDs(s *store.Store, w io.Writer) {
	for _, it := range s.List(store.TypeFilter("goal")) {
		for _, ids := range it.MustDo {
			for _, id := range ids {
				fmt.Fprintln(w, id)
			}
		}
	}
}

// findInMustDo returns the bucket name and true if id is found in any bucket.
// Returns ("", false) if not found.
func findInMustDo(mustDo map[string][]string, id string) (string, bool) {
	for bucket, ids := range mustDo {
		for _, existing := range ids {
			if existing == id {
				return bucket, true
			}
		}
	}
	return "", false
}

// bucketOf returns the bucket name containing id, or "" if not found.
// Distinct from findInMustDo: an uncategorized item is stored under "" key.
func bucketOf(mustDo map[string][]string, id string) string {
	bucket, _ := findInMustDo(mustDo, id)
	return bucket
}

// removeFromMustDo removes all occurrences of id from every bucket.
func removeFromMustDo(mustDo map[string][]string, id string) {
	for bucket, ids := range mustDo {
		filtered := ids[:0]
		for _, existing := range ids {
			if existing != id {
				filtered = append(filtered, existing)
			}
		}
		mustDo[bucket] = filtered
	}
}

// sortedBucketKeys returns bucket keys sorted alphabetically with "" first.
func sortedBucketKeys(mustDo map[string][]string) []string {
	keys := make([]string, 0, len(mustDo))
	for k := range mustDo {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "" {
			return true
		}
		if keys[j] == "" {
			return false
		}
		return keys[i] < keys[j]
	})
	return keys
}

// isTerminalStatus returns true for done/closed lifecycle statuses.
func isTerminalStatus(status string) bool {
	switch status {
	case "completed", "resolved", "abandoned", "wontfix", "closed", "met", "dropped":
		return true
	}
	return false
}

// mustDoSummary renders a compact summary line for goal show rendering.
// e.g. "3/10 done (billing:2/4 clinical:1/3 phi_safety:0/3)"
func mustDoSummary(mustDo map[string][]string, s *store.Store) string {
	if len(mustDo) == 0 {
		return "(empty)"
	}
	bucketKeys := sortedBucketKeys(mustDo)
	totalDone, totalAll := 0, 0
	var parts []string

	for _, bucket := range bucketKeys {
		ids := mustDo[bucket]
		done := 0
		for _, id := range ids {
			if item, ok := s.Get(id); ok && isTerminalStatus(item.Status) {
				done++
			}
		}
		totalDone += done
		totalAll += len(ids)
		label := bucket
		if label == "" {
			label = "other"
		}
		parts = append(parts, fmt.Sprintf("%s:%d/%d", label, done, len(ids)))
	}

	summary := fmt.Sprintf("%d/%d done", totalDone, totalAll)
	if len(parts) > 1 || (len(parts) == 1 && !strings.HasPrefix(parts[0], "other:")) {
		summary += " (" + strings.Join(parts, " ") + ")"
	}
	return summary
}
