package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/migrate"
	"github.com/theraprac/agent-state/internal/store"
)

// MigrateOpts configures the migrate command.
type MigrateOpts struct {
	DryRun bool
	Scope  string   // "archive", "active", or "" (all)
	IDs    []string // I-1439: when non-empty, restrict to these item IDs
	// (targeted repair — re-serialize specific corrupt files through the
	// typed struct without rewriting the whole corpus).
}

// Migrate normalizes all item files to canonical schema.
func Migrate(s *store.Store, cfg *config.Config, opts MigrateOpts) int {
	var totalFiles, changedFiles, errorFiles int

	// I-1439: targeted-id set for surgical repair.
	var idSet map[string]bool
	if len(opts.IDs) > 0 {
		idSet = make(map[string]bool, len(opts.IDs))
		for _, id := range opts.IDs {
			idSet[id] = true
		}
	}

	for id, item := range s.All() {
		path, ok := s.Path(id)
		if !ok {
			continue
		}

		// I-1439: targeted-id filter takes precedence — when a set is
		// given, only those ids migrate and scope is ignored.
		if idSet != nil {
			if !idSet[id] {
				continue
			}
		} else {
			// Scope filter
			if opts.Scope == "archive" && !strings.Contains(path, "/archive/") {
				continue
			}
			if opts.Scope == "active" && strings.Contains(path, "/archive/") {
				continue
			}
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
