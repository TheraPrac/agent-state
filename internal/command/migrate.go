package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/migrate"
	"github.com/jfinlinson/agent-state/internal/store"
)

// MigrateOpts configures the migrate command.
type MigrateOpts struct {
	DryRun bool
	Scope  string // "archive", "active", or "" (all)
}

// Migrate normalizes all item files to canonical schema.
func Migrate(s *store.Store, cfg *config.Config, opts MigrateOpts) int {
	var totalFiles, changedFiles, errorFiles int

	for id, item := range s.All() {
		path, ok := s.Path(id)
		if !ok {
			continue
		}

		// Scope filter
		if opts.Scope == "archive" && !strings.Contains(path, "/archive/") {
			continue
		}
		if opts.Scope == "active" && strings.Contains(path, "/archive/") {
			continue
		}

		totalFiles++
		result := migrate.PlanFile(item, path, cfg)
		if !result.HasChanges() {
			continue
		}

		changedFiles++

		if opts.DryRun {
			fmt.Printf("  %s (%s)\n", id, path)
			for _, c := range result.Changes {
				fmt.Printf("    [%s] %s\n", c.Type, c.Detail)
			}
			continue
		}

		// Apply: write canonical content
		if err := os.WriteFile(path, []byte(result.After), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
			errorFiles++
			continue
		}
	}

	if opts.DryRun {
		fmt.Printf("\ndry run: %d/%d files would change\n", changedFiles, totalFiles)
	} else {
		fmt.Printf("migrated: %d/%d files changed", changedFiles, totalFiles)
		if errorFiles > 0 {
			fmt.Printf(" (%d errors)", errorFiles)
		}
		fmt.Println()
	}

	if errorFiles > 0 {
		return 1
	}
	return 0
}
