package command

import (
	"fmt"
	"os"
	"regexp"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

var slugPattern = regexp.MustCompile(`^([A-Z]-\d{3,})-\S+`)

// Fix applies auto-repairs for deterministic issues. Returns the number of fixes applied.
func Fix(s *store.Store, cfg *config.Config) int {
	var fixed int

	fixed += fixRequiredFields(s, cfg)
	fixed += fixStaleDeps(s, cfg)
	fixed += fixIndex(s, cfg)

	return fixed
}

// fixRequiredFields inserts missing type-specific required fields into items.
func fixRequiredFields(s *store.Store, cfg *config.Config) int {
	var fixed int
	for _, item := range s.All() {
		if item.Doc == nil || item.Type == "" {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}

		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}

		modified := false
		for _, field := range tc.RequiredFields {
			if validate.HasField(item.Doc, field) {
				continue
			}

			switch field {
			case "depends_on", "blocks":
				item.Doc.SetList(field, []string{})
			case "severity":
				item.Doc.SetField(field, "medium")
			default:
				item.Doc.SetField(field, "")
			}
			modified = true
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: inserted missing field %q\n", item.ID, field)
		}

		if modified {
			if err := s.Write(item); err != nil {
				fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", item.ID, err)
			}
		}
	}
	return fixed
}

// fixStaleDeps normalizes slug-format dependency IDs to bare IDs.
// e.g., T-013-subscription-billing-implementation → T-013
func fixStaleDeps(s *store.Store, cfg *config.Config) int {
	var fixed int
	for _, item := range s.All() {
		if item.Doc == nil {
			continue
		}

		newDeps, depsChanged := normalizeDeps(item.DependsOn)
		newBlocks, blocksChanged := normalizeDeps(item.Blocks)

		if !depsChanged && !blocksChanged {
			continue
		}

		if depsChanged {
			item.DependsOn = newDeps
			item.Doc.SetList("depends_on", newDeps)
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: normalized depends_on slugs\n", item.ID)
		}
		if blocksChanged {
			item.Blocks = newBlocks
			item.Doc.SetList("blocks", newBlocks)
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: normalized blocks slugs\n", item.ID)
		}

		if err := s.Write(item); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", item.ID, err)
		}
	}
	return fixed
}

// normalizeDeps converts slug-format IDs to bare IDs.
// Returns the new list and whether any changes were made.
func normalizeDeps(deps []string) ([]string, bool) {
	if len(deps) == 0 {
		return deps, false
	}
	changed := false
	result := make([]string, len(deps))
	for i, dep := range deps {
		if m := slugPattern.FindStringSubmatch(dep); m != nil {
			result[i] = m[1]
			changed = true
		} else {
			result[i] = dep
		}
	}
	return result, changed
}

// fixIndex regenerates index.md to ensure all non-archived items are listed.
func fixIndex(s *store.Store, cfg *config.Config) int {
	indexPath := cfg.IndexPath()
	indexContent, err := os.ReadFile(indexPath)
	if err != nil {
		// If no index file, definitely regenerate
		fmt.Printf("  \033[33m⟳\033[0m regenerating index.md\n")
		Index(s, cfg)
		return 1
	}

	// Check if any non-terminal items are missing from the index
	errs := validate.IndexCoverage(s.All(), string(indexContent), cfg)
	if len(errs) > 0 {
		fmt.Printf("  \033[33m⟳\033[0m regenerating index.md (%d items missing)\n", len(errs))
		Index(s, cfg)
		return 1
	}

	return 0
}

// isSlugID returns true if the ID contains a slug suffix (e.g., T-013-some-description).
func isSlugID(id string) bool {
	return slugPattern.MatchString(id)
}

// FixableSummary returns a count of auto-fixable issues from check results.
func FixableSummary(s *store.Store, cfg *config.Config) (fixable int, descriptions []string) {
	// Count missing required fields
	for _, item := range s.All() {
		if item.Doc == nil || item.Type == "" {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		tc, ok := cfg.Types[item.Type]
		if !ok {
			continue
		}
		for _, field := range tc.RequiredFields {
			if !validate.HasField(item.Doc, field) {
				fixable++
			}
		}
	}
	if fixable > 0 {
		descriptions = append(descriptions, fmt.Sprintf("%d missing required fields", fixable))
	}

	// Count stale slug deps
	slugCount := 0
	for _, item := range s.All() {
		for _, dep := range item.DependsOn {
			if isSlugID(dep) {
				slugCount++
			}
		}
		for _, dep := range item.Blocks {
			if isSlugID(dep) {
				slugCount++
			}
		}
	}
	if slugCount > 0 {
		fixable += slugCount
		descriptions = append(descriptions, fmt.Sprintf("%d slug-format dependency refs", slugCount))
	}

	// Count index.md coverage gaps
	indexPath := cfg.IndexPath()
	if indexContent, err := os.ReadFile(indexPath); err == nil {
		errs := validate.IndexCoverage(s.All(), string(indexContent), cfg)
		if len(errs) > 0 {
			fixable += len(errs)
			descriptions = append(descriptions, fmt.Sprintf("%d items missing from index.md", len(errs)))
		}
	}

	return fixable, descriptions
}
