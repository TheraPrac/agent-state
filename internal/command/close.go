package command

import (
	"fmt"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

// CloseOpts holds flags for the close command.
type CloseOpts struct {
	Reason string
	Force  bool // bypass gate enforcement
}

func Close(s *store.Store, cfg *config.Config, id, resolution string, opts CloseOpts) int {
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
		if opts.Reason == "" {
			fmt.Fprintln(os.Stderr, "--reason is required when abandoning")
			return 2
		}
	}

	// Gate enforcement (skip for abandon/wontfix — those bypass gates by design)
	if !opts.Force && resolution != "abandoned" && resolution != "wontfix" && resolution != "declined" {
		results := validate.EvaluateGates(item, "close", cfg, s.All())
		if !validate.GatesPassed(results) {
			failure := validate.FirstFailure(results)
			fmt.Fprintf(os.Stderr, "gate %q failed: %s\n", failure.Gate, failure.Message)
			fmt.Fprintln(os.Stderr, "use --force to bypass gates")
			return 1
		}
	}

	// Transition
	oldStatus := item.Status
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	item.Doc.SetField("status", resolution)
	item.Status = resolution
	item.Doc.SetField("completed", nowStr)
	item.Doc.SetField("last_touched", nowStr)

	if opts.Reason != "" {
		item.Doc.SetField("resolution", opts.Reason)
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

	changelog.Append(cfg, id, changelog.Entry{
		Op: "close", Field: "status",
		OldValue: oldStatus, NewValue: resolution,
		Reason: opts.Reason,
	})

	fmt.Printf("Closed %s — %s (%s)\n", id, item.Title, resolution)
	return 0
}
