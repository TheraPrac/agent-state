package command

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

func Check(s *store.Store, cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "exit code only")
	fs.Parse(args)

	var issues int

	// Validate each item
	for id, item := range s.All() {
		// Schema validation
		r := validate.Item(item, cfg)
		for _, e := range r.Errors {
			issues++
			if !*quiet {
				fmt.Printf("  \033[31m✗\033[0m %s\n", e)
			}
		}

		// Directory consistency
		path, ok := s.Path(id)
		if ok {
			dir := filepath.Dir(path)
			dr := validate.DirectoryConsistency(item, dir, cfg)
			for _, e := range dr.Errors {
				issues++
				if !*quiet {
					fmt.Printf("  \033[31m✗\033[0m %s\n", e)
				}
			}
		}
	}

	// Reciprocal dependency check
	depErrors := validate.ReciprocalDeps(s.All())
	for _, e := range depErrors {
		issues++
		if !*quiet {
			fmt.Printf("  \033[31m✗\033[0m %s\n", e)
		}
	}

	// Cycle detection
	g := deps.Build(s.All(), cfg)
	cycles := g.DetectCycles()
	for _, cycle := range cycles {
		issues++
		if !*quiet {
			fmt.Printf("  \033[31m✗\033[0m dependency cycle: %v\n", cycle)
		}
	}

	if !*quiet {
		if issues == 0 {
			fmt.Println("\033[32m✓\033[0m All checks passed")
		} else {
			fmt.Printf("\n\033[31m%d issue(s) found\033[0m\n", issues)
		}
	}

	if issues > 0 {
		return 1
	}
	return 0
}
