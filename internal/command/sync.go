package command

import (
	"fmt"
	"os"

	"github.com/theraprac/agent-state/internal/store"
)

func Sync(s *store.Store, message string, allowNonState bool) int {
	if message == "" {
		message = "as: sync agent-state"
	}

	if allowNonState {
		os.Setenv("ST_SYNC_ALLOW_NON_STATE", "1")
		defer os.Unsetenv("ST_SYNC_ALLOW_NON_STATE")
	}

	if err := s.GitSync(message); err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		return 1
	}

	fmt.Println("Synced.")
	return 0
}
