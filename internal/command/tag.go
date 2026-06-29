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
		fmt.Fprintf(os.Stderr, "unknown action %q — use 'add' or 'rm'\n", action)
		return 2
	}

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

// tagGoal handles 'st tag <id> add/rm G-NNN' by routing to the goals: field.
func tagGoal(s *store.Store, cfg *config.Config, id, action, goalID string, item *model.Item) int {
	// Validate the goal exists and is actually type="goal".
	g, exists := s.Get(goalID)
	if !exists {
		fmt.Fprintf(os.Stderr, "tag %s goals: goal not found: %s\n", id, goalID)
		return 2
	}
	if g.Type != "goal" {
		fmt.Fprintf(os.Stderr, "tag %s goals: %s is not a goal (type=%s)\n", id, goalID, g.Type)
		return 2
	}

	switch action {
	case "add":
		for _, gid := range item.Goals {
			if gid == goalID {
				fmt.Fprintf(os.Stderr, "%s already has goal %q\n", id, goalID)
				return 1
			}
		}
	case "rm":
		inGoals := false
		for _, gid := range item.Goals {
			if gid == goalID {
				inGoals = true
				break
			}
		}
		if !inGoals {
			// Fall back: goal may live in tags: from before goal-routing was added.
			for _, t := range item.Tags {
				if t == goalID {
					return tagLabel(s, cfg, id, action, goalID, item)
				}
			}
			fmt.Fprintf(os.Stderr, "%s does not have goal %q\n", id, goalID)
			return 1
		}
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		switch action {
		case "add":
			it.Goals = append(it.Goals, goalID)
		case "rm":
			var kept []string
			for _, gid := range it.Goals {
				if gid != goalID {
					kept = append(kept, gid)
				}
			}
			it.Goals = kept
		}
		it.Doc.SetList("goals", it.Goals)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	switch action {
	case "add":
		changelog.Append(cfg, id, changelog.Entry{Op: "tag_add", Field: "goals", NewValue: goalID})
		fmt.Printf("Goal %s added to goals: on %s (not tags:)\n", goalID, id)
	case "rm":
		changelog.Append(cfg, id, changelog.Entry{Op: "tag_rm", Field: "goals", OldValue: goalID})
		fmt.Printf("Goal %s removed from goals: on %s\n", goalID, id)
	}

	if err := autoSync(s, fmt.Sprintf("st tag %s: %s goals %s", action, id, goalID)); err != nil {
		return 1
	}
	return 0
}

// tagLabel handles the standard 'st tag <id> add/rm <label>' path (non-goal tags).
func tagLabel(s *store.Store, cfg *config.Config, id, action, tag string, item *model.Item) int {
	preflightErr := func() int {
		switch action {
		case "add":
			for _, t := range item.Tags {
				if t == tag {
					fmt.Fprintf(os.Stderr, "%s already has tag %q\n", id, tag)
					return 1
				}
			}
		case "rm":
			for _, t := range item.Tags {
				if t == tag {
					return 0
				}
			}
			fmt.Fprintf(os.Stderr, "%s does not have tag %q\n", id, tag)
			return 1
		}
		return 0
	}()
	if preflightErr != 0 {
		return preflightErr
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		switch action {
		case "add":
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
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	switch action {
	case "add":
		changelog.Append(cfg, id, changelog.Entry{Op: "tag_add", Field: "tags", NewValue: tag})
	case "rm":
		changelog.Append(cfg, id, changelog.Entry{Op: "tag_rm", Field: "tags", OldValue: tag})
	}

	fmt.Printf("Tag %s %s on %s\n", action, tag, id)

	// Commit + push immediately so the tag change can't be silently
	// reverted by a subsequent command's pre-run GitPull. Best-effort.
	if err := autoSync(s, fmt.Sprintf("st tag %s: %s %s", action, id, tag)); err != nil {
		return 1
	}
	return 0
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

	// Preflight: check no item is already tagged / not tagged.
	for _, id := range ids {
		item := items[id]
		switch action {
		case "add":
			for _, gid := range item.Goals {
				if gid == goalID {
					fmt.Fprintf(os.Stderr, "%s already has goal %q\n", id, goalID)
					return 1
				}
			}
		case "rm":
			inGoals := false
			for _, gid := range item.Goals {
				if gid == goalID {
					inGoals = true
					break
				}
			}
			inTags := false
			for _, t := range item.Tags {
				if t == goalID {
					inTags = true
					break
				}
			}
			if !inGoals && !inTags {
				fmt.Fprintf(os.Stderr, "%s does not have goal %q\n", id, goalID)
				return 1
			}
		}
	}

	if err := s.MutateMany(ids, func(batch map[string]*model.Item) error {
		for _, it := range batch {
			switch action {
			case "add":
				it.Goals = append(it.Goals, goalID)
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
				continue
			}
			it.Doc.SetList("goals", it.Goals)
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
			changelog.Append(cfg, id, changelog.Entry{Op: "tag_rm", Field: "goals", OldValue: goalID})
			fmt.Printf("Goal %s removed from goals: on %s\n", goalID, id)
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
	// Preflight: check preconditions for all items.
	for _, id := range ids {
		item := items[id]
		switch action {
		case "add":
			for _, t := range item.Tags {
				if t == tag {
					fmt.Fprintf(os.Stderr, "%s already has tag %q\n", id, tag)
					return 1
				}
			}
		case "rm":
			found := false
			for _, t := range item.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "%s does not have tag %q\n", id, tag)
				return 1
			}
		}
	}

	if err := s.MutateMany(ids, func(batch map[string]*model.Item) error {
		for _, it := range batch {
			switch action {
			case "add":
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

