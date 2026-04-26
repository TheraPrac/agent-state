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
	fixed += fixReciprocalDeps(s, cfg)
	fixed += fixDanglingDeps(s, cfg)
	fixed += fixDeliveryGate(s, cfg)
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

// fixReciprocalDeps fixes "A depends on B, but B doesn't list A in blocks" by
// adding the missing blocks entry. Iterates until stable since adding one
// reciprocal entry can reveal another missing one.
func fixReciprocalDeps(s *store.Store, cfg *config.Config) int {
	var totalFixed int

	for pass := 0; pass < 5; pass++ {
		fixed := 0
		items := s.All()
		dirty := map[string]bool{}

		for id, item := range items {
			for _, depID := range item.DependsOn {
				dep, ok := items[depID]
				if !ok {
					continue
				}
				if !sliceContains(dep.Blocks, id) {
					dep.Blocks = append(dep.Blocks, id)
					if dep.Doc != nil {
						dep.Doc.SetList("blocks", dep.Blocks)
					}
					dirty[depID] = true
					fixed++
					fmt.Printf("  \033[33m⟳\033[0m %s: added %s to blocks (reciprocal of depends_on)\n", depID, id)
				}
			}

			for _, blockID := range item.Blocks {
				blocked, ok := items[blockID]
				if !ok {
					continue
				}
				if !sliceContains(blocked.DependsOn, id) {
					blocked.DependsOn = append(blocked.DependsOn, id)
					if blocked.Doc != nil {
						blocked.Doc.SetList("depends_on", blocked.DependsOn)
					}
					dirty[blockID] = true
					fixed++
					fmt.Printf("  \033[33m⟳\033[0m %s: added %s to depends_on (reciprocal of blocks)\n", blockID, id)
				}
			}
		}

		for id := range dirty {
			item := items[id]
			if err := s.Write(item); err != nil {
				fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", id, err)
			}
		}

		totalFixed += fixed
		if fixed == 0 {
			break
		}
	}

	return totalFixed
}

// fixDanglingDeps removes depends_on references to items that don't exist.
func fixDanglingDeps(s *store.Store, cfg *config.Config) int {
	var fixed int
	items := s.All()

	for id, item := range items {
		if item.Doc == nil {
			continue
		}

		var cleanDeps []string
		removed := false
		for _, depID := range item.DependsOn {
			if _, ok := items[depID]; ok {
				cleanDeps = append(cleanDeps, depID)
			} else {
				removed = true
				fixed++
				fmt.Printf("  \033[33m⟳\033[0m %s: removed dangling depends_on ref %s\n", id, depID)
			}
		}

		if removed {
			item.DependsOn = cleanDeps
			if len(cleanDeps) == 0 {
				item.Doc.SetList("depends_on", []string{})
			} else {
				item.Doc.SetList("depends_on", cleanDeps)
			}
			if err := s.Write(item); err != nil {
				fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", id, err)
			}
		}
	}

	return fixed
}

// fixDeliveryGate stamps archived items that have a delivery block but haven't
// reached uat_approved. These are legacy items that predate the gate policy.
func fixDeliveryGate(s *store.Store, cfg *config.Config) int {
	if cfg.Delivery == nil || cfg.Delivery.ArchiveGate == "" {
		return 0
	}

	var fixed int
	for _, item := range s.All() {
		if !cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		if item.Type != "task" && item.Type != "issue" {
			continue
		}
		if item.Delivery == nil || len(item.Delivery) == 0 {
			continue
		}

		stage, _ := item.Delivery["stage"].(string)
		if cfg.StageReached(stage, cfg.Delivery.ArchiveGate) {
			continue
		}

		// Auto-fix: stamp as uat_approved (legacy item, already archived)
		item.SetNested("delivery", "stage", cfg.Delivery.ArchiveGate)
		if err := s.Write(item); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", item.ID, err)
			continue
		}
		fixed++
		if stage == "" {
			stage = "null"
		}
		fmt.Printf("  \033[33m⟳\033[0m %s: delivery_stage %s → %s (legacy archived item)\n",
			item.ID, stage, cfg.Delivery.ArchiveGate)
	}

	return fixed
}

// normalizeDeps converts slug-format IDs to bare IDs.
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
		fmt.Printf("  \033[33m⟳\033[0m regenerating index.md\n")
		Index(s, cfg)
		return 1
	}

	errs := validate.IndexCoverage(s.All(), string(indexContent), cfg)
	if len(errs) > 0 {
		fmt.Printf("  \033[33m⟳\033[0m regenerating index.md (%d items missing)\n", len(errs))
		Index(s, cfg)
		return 1
	}

	return 0
}

// isSlugID returns true if the ID contains a slug suffix.
func isSlugID(id string) bool {
	return slugPattern.MatchString(id)
}

func sliceContains(ss []string, target string) bool {
	for _, v := range ss {
		if v == target {
			return true
		}
	}
	return false
}

// FixableSummary returns a count of auto-fixable issues from check results.
func FixableSummary(s *store.Store, cfg *config.Config) (fixable int, descriptions []string) {
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
