package command

import (
	"fmt"
	"os"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// DepAdd adds a dependency: id depends_on depID, depID blocks id.
func DepAdd(s *store.Store, cfg *config.Config, id, depID string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	dep, ok := s.Get(depID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", depID)
		return 1
	}

	if item.Doc == nil || dep.Doc == nil {
		fmt.Fprintf(os.Stderr, "item(s) missing document\n")
		return 1
	}

	// Check for self-dependency
	if id == depID {
		fmt.Fprintf(os.Stderr, "cannot depend on self\n")
		return 2
	}

	// Check duplicate
	for _, d := range item.DependsOn {
		if d == depID {
			fmt.Fprintf(os.Stderr, "%s already depends on %s\n", id, depID)
			return 1
		}
	}

	if err := s.MutateMany([]string{id, depID}, func(items map[string]*model.Item) error {
		it := items[id]
		dp := items[depID]
		// Forward edge
		it.DependsOn = append(it.DependsOn, depID)
		updateListInDoc(it, "depends_on", it.DependsOn)
		// Inverse edge
		dp.Blocks = append(dp.Blocks, id)
		updateListInDoc(dp, "blocks", dp.Blocks)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing dep edges %s -> %s: %v\n", id, depID, err)
		return 1
	}

	// Changelog
	changelog.Append(cfg, id, changelog.Entry{
		Op: "dep_add", Field: "depends_on", NewValue: depID,
	})
	changelog.Append(cfg, depID, changelog.Entry{
		Op: "dep_add", Field: "blocks", NewValue: id,
	})

	fmt.Printf("Added dependency: %s depends on %s\n", id, depID)

	// Commit + push both edge updates atomically so the forward and
	// inverse edges can't be silently reverted by a subsequent command's
	// pre-run GitPull. Best-effort.
	if err := autoSync(s, fmt.Sprintf("st dep add: %s -> %s", id, depID)); err != nil {
		return 1
	}
	return 0
}

// DepRm removes a dependency: id no longer depends_on depID, depID no longer blocks id.
func DepRm(s *store.Store, cfg *config.Config, id, depID string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	dep, ok := s.Get(depID)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", depID)
		return 1
	}

	if item.Doc == nil || dep.Doc == nil {
		fmt.Fprintf(os.Stderr, "item(s) missing document\n")
		return 1
	}

	// Pre-flight on cached state for a fast user-error message.
	found := false
	for _, d := range item.DependsOn {
		if d == depID {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "%s does not depend on %s\n", id, depID)
		return 1
	}

	if err := s.MutateMany([]string{id, depID}, func(items map[string]*model.Item) error {
		it := items[id]
		dp := items[depID]

		var newDeps []string
		for _, d := range it.DependsOn {
			if d != depID {
				newDeps = append(newDeps, d)
			}
		}
		it.DependsOn = newDeps
		updateListInDoc(it, "depends_on", it.DependsOn)

		var newBlocks []string
		for _, b := range dp.Blocks {
			if b != id {
				newBlocks = append(newBlocks, b)
			}
		}
		dp.Blocks = newBlocks
		updateListInDoc(dp, "blocks", dp.Blocks)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing dep edges %s -> %s: %v\n", id, depID, err)
		return 1
	}

	// Changelog
	changelog.Append(cfg, id, changelog.Entry{
		Op: "dep_rm", Field: "depends_on", OldValue: depID,
	})
	changelog.Append(cfg, depID, changelog.Entry{
		Op: "dep_rm", Field: "blocks", OldValue: id,
	})

	fmt.Printf("Removed dependency: %s no longer depends on %s\n", id, depID)

	// Commit + push both edge updates atomically so the forward and
	// inverse edges can't be silently reverted by a subsequent command's
	// pre-run GitPull. Best-effort.
	if err := autoSync(s, fmt.Sprintf("st dep rm: %s -> %s", id, depID)); err != nil {
		return 1
	}
	return 0
}

// updateListInDoc rewrites a list field in the document.
// For depends_on and blocks, uses the multi-line "- item" format.
func updateListInDoc(item *model.Item, key string, values []string) {
	doc := item.Doc
	if doc == nil {
		return
	}

	// Find the key line and replace the list
	keyIdx := -1
	listEnd := -1
	for i, line := range doc.Lines {
		if line.Key == key && line.Indent == 0 {
			keyIdx = i
			// Find the end of the list (next non-list, non-empty line or key line)
			for j := i + 1; j < len(doc.Lines); j++ {
				if doc.Lines[j].IsList {
					listEnd = j
				} else if doc.Lines[j].IsEmpty {
					continue
				} else {
					break
				}
			}
			break
		}
	}

	if keyIdx < 0 {
		// Key not found — append it
		doc.Lines = append(doc.Lines, model.Line{Raw: "", IsEmpty: true})
		doc.Lines = append(doc.Lines, model.Line{Raw: key + ":", Key: key})
		if len(values) == 0 {
			doc.Lines = append(doc.Lines, model.Line{Raw: "- []", IsList: true})
		} else {
			for _, v := range values {
				doc.Lines = append(doc.Lines, model.Line{Raw: "- " + v, IsList: true})
			}
		}
		return
	}

	// Remove old list items
	if listEnd >= keyIdx {
		// Build new lines
		var newListLines []model.Line
		if len(values) == 0 {
			newListLines = append(newListLines, model.Line{Raw: "- []", IsList: true})
		} else {
			for _, v := range values {
				newListLines = append(newListLines, model.Line{Raw: "- " + v, IsList: true})
			}
		}

		// Replace old list lines with new ones
		before := doc.Lines[:keyIdx+1]
		after := doc.Lines[listEnd+1:]
		doc.Lines = make([]model.Line, 0, len(before)+len(newListLines)+len(after))
		doc.Lines = append(doc.Lines, before...)
		doc.Lines = append(doc.Lines, newListLines...)
		doc.Lines = append(doc.Lines, after...)
	}
}

