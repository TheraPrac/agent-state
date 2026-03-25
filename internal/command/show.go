package command

import (
	"flag"
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/store"
)

func Show(s *store.Store, args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	brief := fs.Bool("brief", false, "compact output")
	field := fs.String("field", "", "show single field value")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: as show <id> [--brief] [--field <name>]")
		return 2
	}

	id := fs.Arg(0)
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if *field != "" {
		if item.Doc != nil {
			val, ok := item.Doc.GetField(*field)
			if ok {
				fmt.Println(val)
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "field %q not found on %s\n", *field, id)
		return 1
	}

	if *brief {
		stage := ""
		if s, ok := item.Delivery["stage"]; ok {
			if str, ok := s.(string); ok && str != "" {
				stage = str
			}
		}
		fmt.Printf("%s  %-10s  %s", item.ID, item.Status, item.Title)
		if stage != "" {
			fmt.Printf("  (%s)", stage)
		}
		if item.AssignedTo != "" {
			fmt.Printf("  [%s]", item.AssignedTo)
		}
		fmt.Println()
		return 0
	}

	// Full output
	fmt.Printf("%s — %s\n", item.ID, item.Title)
	fmt.Printf("  Type:     %s\n", item.Type)
	fmt.Printf("  Status:   %s\n", item.Status)
	if item.AssignedTo != "" {
		fmt.Printf("  Assigned: %s\n", item.AssignedTo)
	}
	if item.Severity != "" {
		fmt.Printf("  Severity: %s\n", item.Severity)
	}
	if stage, ok := item.Delivery["stage"]; ok {
		if str, ok := stage.(string); ok && str != "" {
			fmt.Printf("  Stage:    %s\n", str)
		}
	}
	if len(item.DependsOn) > 0 {
		fmt.Printf("  Depends:  %v\n", item.DependsOn)
	}
	if len(item.Blocks) > 0 {
		fmt.Printf("  Blocks:   %v\n", item.Blocks)
	}
	if len(item.Tags) > 0 {
		fmt.Printf("  Tags:     %v\n", item.Tags)
	}
	if item.Summary != "" {
		fmt.Printf("  Summary:\n    %s\n", item.Summary)
	}
	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("  Acceptance criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}
	if len(item.NextActions) > 0 {
		fmt.Println("  Next actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	return 0
}
