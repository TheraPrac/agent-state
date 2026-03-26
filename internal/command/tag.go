package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Tag adds or removes a tag on an item.
func Tag(s *store.Store, cfg *config.Config, id, action, tag string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	switch action {
	case "add":
		// Check for duplicates
		for _, t := range item.Tags {
			if t == tag {
				fmt.Fprintf(os.Stderr, "%s already has tag %q\n", id, tag)
				return 1
			}
		}
		item.Tags = append(item.Tags, tag)
		updateTagsInDoc(item)

		changelog.Append(cfg, id, changelog.Entry{
			Op: "tag_add", Field: "tags", NewValue: tag,
		})

	case "rm":
		found := false
		var newTags []string
		for _, t := range item.Tags {
			if t == tag {
				found = true
			} else {
				newTags = append(newTags, t)
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "%s does not have tag %q\n", id, tag)
			return 1
		}
		item.Tags = newTags
		updateTagsInDoc(item)

		changelog.Append(cfg, id, changelog.Entry{
			Op: "tag_rm", Field: "tags", OldValue: tag,
		})

	default:
		fmt.Fprintf(os.Stderr, "unknown action %q — use 'add' or 'rm'\n", action)
		return 2
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("Tag %s %s on %s\n", action, tag, id)
	return 0
}

// updateTagsInDoc rewrites the tags list in the document using multi-line format.
func updateTagsInDoc(item *model.Item) {
	item.Doc.SetList("tags", item.Tags)
}
