package command

import (
	"fmt"
	"os"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// isGoalID reports whether s looks like a goal identifier (G- followed by digits).
func isGoalID(s string) bool {
	if len(s) < 3 || s[0] != 'G' || s[1] != '-' {
		return false
	}
	for _, c := range s[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Tag adds or removes a tag on a single item. Goal-shaped args (G-NNN) are
// routed to the goals: field rather than tags:, with store validation.
// Thin wrapper over TagMany for backward compatibility.
func Tag(s *store.Store, cfg *config.Config, id, action, tag string) int {
	return TagMany(s, cfg, []string{id}, action, tag)
}

// TagMany adds or removes a tag on multiple items in a single atomic
// store.MutateMany call and a single autoSync. All IDs are validated before
// any mutation — a missing or invalid ID causes an early return without
// touching any item. Goal-shaped tags (G-NNN) route to goals:; all other
// tags route to tags:; routing is uniform across the batch since the tag is
// shared.
func TagMany(s *store.Store, cfg *config.Config, ids []string, action, tag string) int {
	if len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "at least one item ID required\n")
		return 2
	}
	if action != "add" && action != "rm" {
		fmt.Fprintf(os.Stderr, "unknown action %q — expected 'add' or 'rm' (syntax: st tag <id> [<id2>...] add|rm <tag>)\n", action)
		return 2
	}

	// Dedup ids to prevent duplicate changelog entries from repeated IDs.
	seen := make(map[string]struct{}, len(ids))
	var deduped []string
	for _, id := range ids {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			deduped = append(deduped, id)
		}
	}
	ids = deduped

	// Validate all IDs and snapshot their current state upfront.
	items := make(map[string]*model.Item, len(ids))
	for _, id := range ids {
		item, ok := s.Get(id)
		if !ok {
			fmt.Fprintf(os.Stderr, "not found: %s\n", id)
			return 1
		}
		if item.Doc == nil {
			fmt.Fprintf(os.Stderr, "%s has no document\n", id)
			return 1
		}
		items[id] = item
	}

	if isGoalID(tag) {
		return tagManyGoal(s, cfg, ids, action, tag, items)
	}
	return tagManyLabel(s, cfg, ids, action, tag, items)
}

// tagManyGoal handles batch 'st tag <ids...> add/rm G-NNN'.
func tagManyGoal(s *store.Store, cfg *config.Config, ids []string, action, goalID string, items map[string]*model.Item) int {
	// Validate goal exists.
	g, exists := s.Get(goalID)
	if !exists {
		fmt.Fprintf(os.Stderr, "tag goals: goal not found: %s\n", goalID)
		return 2
	}
	if g.Type != "goal" {
		fmt.Fprintf(os.Stderr, "tag goals: %s is not a goal (type=%s)\n", goalID, g.Type)
		return 2
	}

	// Preflight against snapshot (fast path — concurrent writes handled under lock).
	for _, id := range ids {
		item := items[id]
		switch action {
		case "add":
			if sliceContains(item.Goals, goalID) {
				fmt.Fprintf(os.Stderr, "%s already has goal %q\n", id, goalID)
				return 1
			}
		case "rm":
			if !sliceContains(item.Goals, goalID) && !sliceContains(item.Tags, goalID) {
				fmt.Fprintf(os.Stderr, "%s does not have goal %q\n", id, goalID)
				return 1
			}
		}
	}

	// Track which field held the goal per item (snapshot-based; used for accurate changelog).
	goalInGoals := make(map[string]bool, len(ids))
	if action == "rm" {
		for _, id := range ids {
			goalInGoals[id] = sliceContains(items[id].Goals, goalID)
		}
	}

	if err := s.MutateMany(ids, func(batch map[string]*model.Item) error {
		for id, it := range batch {
			switch action {
			case "add":
				// Re-check under lock against fresh state to prevent TOCTOU duplicates.
				if sliceContains(it.Goals, goalID) {
					return fmt.Errorf("%s already has goal %q (concurrent write)", id, goalID)
				}
				it.Goals = append(it.Goals, goalID)
				it.Doc.SetList("goals", it.Goals)
			case "rm":
				var keptGoals []string
				for _, gid := range it.Goals {
					if gid != goalID {
						keptGoals = append(keptGoals, gid)
					}
				}
				it.Goals = keptGoals
				it.Doc.SetList("goals", it.Goals)
				// Legacy fallback: goal may be in tags: if added before goal-routing.
				var keptTags []string
				for _, t := range it.Tags {
					if t != goalID {
						keptTags = append(keptTags, t)
					}
				}
				it.Tags = keptTags
				updateTagsInDoc(it)
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing items: %v\n", err)
		return 1
	}

	for _, id := range ids {
		switch action {
		case "add":
			changelog.Append(cfg, id, changelog.Entry{Op: "tag_add", Field: "goals", NewValue: goalID})
			fmt.Printf("Goal %s added to goals: on %s (not tags:)\n", goalID, id)
		case "rm":
			field := "goals"
			if !goalInGoals[id] {
				field = "tags"
			}
			changelog.Append(cfg, id, changelog.Entry{Op: "tag_rm", Field: field, OldValue: goalID})
			fmt.Printf("Goal %s removed from %s: on %s\n", goalID, field, id)
		}
	}

	syncMsg := fmt.Sprintf("st tag %s: %d item(s) goals %s", action, len(ids), goalID)
	if err := autoSync(s, syncMsg); err != nil {
		return 1
	}
	return 0
}

// tagManyLabel handles batch 'st tag <ids...> add/rm <label>' (non-goal tags).
func tagManyLabel(s *store.Store, cfg *config.Config, ids []string, action, tag string, items map[string]*model.Item) int {
	// Preflight against snapshot (fast path — concurrent writes handled under lock).
	for _, id := range ids {
		item := items[id]
		switch action {
		case "add":
			if sliceContains(item.Tags, tag) {
				fmt.Fprintf(os.Stderr, "%s already has tag %q\n", id, tag)
				return 1
			}
		case "rm":
			if !sliceContains(item.Tags, tag) {
				fmt.Fprintf(os.Stderr, "%s does not have tag %q\n", id, tag)
				return 1
			}
		}
	}

	if err := s.MutateMany(ids, func(batch map[string]*model.Item) error {
		for id, it := range batch {
			switch action {
			case "add":
				// Re-check under lock against fresh state to prevent TOCTOU duplicates.
				if sliceContains(it.Tags, tag) {
					return fmt.Errorf("%s already has tag %q (concurrent write)", id, tag)
				}
				it.Tags = append(it.Tags, tag)
			case "rm":
				var kept []string
				for _, t := range it.Tags {
					if t != tag {
						kept = append(kept, t)
					}
				}
				it.Tags = kept
			}
			updateTagsInDoc(it)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing items: %v\n", err)
		return 1
	}

	for _, id := range ids {
		switch action {
		case "add":
			changelog.Append(cfg, id, changelog.Entry{Op: "tag_add", Field: "tags", NewValue: tag})
		case "rm":
			changelog.Append(cfg, id, changelog.Entry{Op: "tag_rm", Field: "tags", OldValue: tag})
		}
		fmt.Printf("Tag %s %s on %s\n", action, tag, id)
	}

	syncMsg := fmt.Sprintf("st tag %s: %d item(s) %s", action, len(ids), tag)
	if err := autoSync(s, syncMsg); err != nil {
		return 1
	}
	return 0
}

// updateTagsInDoc rewrites the tags list in the document using multi-line format.
func updateTagsInDoc(item *model.Item) {
	item.Doc.SetList("tags", item.Tags)
}
