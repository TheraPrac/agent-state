package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/store"
)

func Sync(s *store.Store, message string) int {
	if message == "" {
		message = "as: sync agent-state"
	}

	if err := s.GitSyncAll(message); err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		return 1
	}

	fmt.Println("Synced.")
	return 0
}
