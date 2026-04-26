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

	if action != "add" && action != "rm" {
		fmt.Fprintf(os.Stderr, "unknown action %q — use 'add' or 'rm'\n", action)
		return 2
	}

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
	if err := s.GitSync(fmt.Sprintf("st tag %s: %s %s", action, id, tag)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after tag failed: %v\n", err)
	}
	return 0
}

// updateTagsInDoc rewrites the tags list in the document using multi-line format.
func updateTagsInDoc(item *model.Item) {
	item.Doc.SetList("tags", item.Tags)
}
