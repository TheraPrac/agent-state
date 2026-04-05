package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ShowOpts holds flags for the show command.
type ShowOpts struct {
	Brief bool
	Field string
	Raw   bool
}

func Show(s *store.Store, cfg *config.Config, id string, opts ShowOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if opts.Raw {
		path, ok := s.Path(id)
		if !ok {
			fmt.Fprintf(os.Stderr, "no file path for %s\n", id)
			return 1
		}
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", path, err)
			return 1
		}
		fmt.Print(string(content))
		return 0
	}

	if opts.Field != "" {
		if item.Doc != nil {
			val, ok := item.Doc.GetField(opts.Field)
			if ok {
				fmt.Println(val)
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "field %q not found on %s\n", opts.Field, id)
		return 1
	}

	if opts.Brief {
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

	// Full output — field keys match the YAML field names (lowercase, single
	// space) so that ACs written as `st show I-XXX | grep 'status: resolved'`
	// work against both the raw file and the pretty output.
	fmt.Printf("%s — %s\n", item.ID, item.Title)
	fmt.Printf("  type: %s\n", item.Type)
	fmt.Printf("  status: %s\n", item.Status)
	if cfg != nil && store.IsLocked(cfg, id) {
		fmt.Printf("  lock: \033[33mlocked\033[0m (protected from git pull)\n")
	}
	if item.AssignedTo != "" {
		fmt.Printf("  assigned_to: %s\n", item.AssignedTo)
	}
	if item.Severity != "" {
		fmt.Printf("  severity: %s\n", item.Severity)
	}
	if stage, ok := item.Delivery["stage"]; ok {
		if str, ok := stage.(string); ok && str != "" {
			fmt.Printf("  stage: %s\n", str)
		}
	}
	if len(item.DependsOn) > 0 {
		fmt.Printf("  depends_on: %v\n", item.DependsOn)
	}
	if len(item.Blocks) > 0 {
		fmt.Printf("  blocks: %v\n", item.Blocks)
	}
	if len(item.Tags) > 0 {
		fmt.Printf("  tags: %v\n", item.Tags)
	}
	if item.Summary != "" {
		fmt.Printf("  summary:\n    %s\n", item.Summary)
	}
	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("  acceptance_criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}
	if len(item.NextActions) > 0 {
		fmt.Println("  next_actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	return 0
}
