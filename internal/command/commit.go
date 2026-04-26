package command

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Commit records a commit message in work_tracking.commits for an item.
func Commit(s *store.Store, cfg *config.Config, id, message string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	now := time.Now().Format(time.RFC3339)

	// Try to get the HEAD SHA from the current directory
	sha := ""
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(out))
	}

	commitLine := fmt.Sprintf("%s %s: %s", now[:10], sha, message)

	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.AppendToNestedList("work_tracking", "commits", commitLine)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "commit", Field: "work_tracking.commits", NewValue: message,
	})

	fmt.Printf("Recorded commit on %s: %s\n", id, message)
	return 0
}

