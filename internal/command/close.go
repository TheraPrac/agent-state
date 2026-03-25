package command

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

func Close(s *store.Store, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason for abandonment")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: as close <id> <resolution> [--reason \"...\"]")
		return 2
	}

	id := fs.Arg(0)
	resolution := fs.Arg(1)

	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	tc, ok := cfg.Types[item.Type]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", item.Type)
		return 1
	}

	// Resolution must be a valid terminal status
	validTerminal := false
	for _, ts := range tc.TerminalStatuses {
		if resolution == ts {
			validTerminal = true
			break
		}
	}
	if !validTerminal {
		fmt.Fprintf(os.Stderr, "invalid resolution %q — valid: %v\n", resolution, tc.TerminalStatuses)
		return 2
	}

	// Must be in active status (or start status for abandoned)
	if item.Status != tc.ActiveStatus && item.Status != tc.StartStatus {
		fmt.Fprintf(os.Stderr, "%s is %s — cannot close\n", id, item.Status)
		return 1
	}

	// If abandoning, require reason
	if resolution == "abandoned" || resolution == "wontfix" || resolution == "declined" {
		if *reason == "" {
			fmt.Fprintln(os.Stderr, "--reason is required when abandoning")
			return 2
		}
	}

	// TODO: gate enforcement (testing_complete, stage_reached, etc.)
	// For now, lightweight close — full gates come with the gate system

	// Transition
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	item.Doc.SetField("status", resolution)
	item.Status = resolution
	item.Doc.SetField("completed", nowStr)
	item.Doc.SetField("last_touched", nowStr)

	if *reason != "" {
		item.Doc.SetField("resolution", *reason)
	}

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	// Move to correct directory
	if err := s.Move(id); err != nil {
		fmt.Fprintf(os.Stderr, "moving %s: %v\n", id, err)
		return 1
	}

	fmt.Printf("Closed %s — %s (%s)\n", id, item.Title, resolution)
	return 0
}
