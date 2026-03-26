package command

import (
	"fmt"
	"os"

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

	// Capture old value for changelog
	oldValue, _ := item.Doc.GetField(field)

	item.Doc.SetField(field, value)

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "update", Field: field,
		OldValue: oldValue, NewValue: value,
	})

	fmt.Printf("Updated %s.%s\n", id, field)
	return 0
}
