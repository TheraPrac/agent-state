package command

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
	"github.com/theraprac/agent-state/internal/validate"
)

var slugPattern = regexp.MustCompile(`^([A-Z]-\d{3,})-\S+`)

// Fix applies auto-repairs for deterministic issues. Returns the number of
// fixes applied and the post-Move paths of every item fixDirectoryMismatch
// relocated (I-1718) — callers must pass those paths explicitly into
// autoSync, since a Move is invisible to git add -u (I-1715/I-442).
func Fix(s *store.Store, cfg *config.Config) (int, []string) {
	var fixed int

	// Legacy-alias rewrite must run first: downstream fixes (and the validator
	// itself) reject items whose status is not in the current enum, so an
	// unrewritten alias would block every other auto-fix on that item.
	fixed += fixLegacyAliases(s, cfg)
	fixed += fixRequiredFields(s, cfg)
	fixed += fixStaleDeps(s, cfg)
	fixed += fixReciprocalDeps(s, cfg)
	fixed += fixDanglingDeps(s, cfg)
	fixed += fixDuplicateBlocks(s, cfg)
	fixed += fixDeliveryGate(s, cfg)
	n, movedPaths := fixDirectoryMismatch(s, cfg)
	fixed += n
	fixed += fixOrphanDeliveryStage(s, cfg)
	fixed += fixIndex(s, cfg)

	return fixed, movedPaths
}

// fixLegacyAliases rewrites items whose status field matches a known
// pre-I-433 alias to the current vocabulary. The alias map lives in
// internal/validate (vocab_suggest.go) — sharing the lookup keeps fix
// and the validator's suggestion engine in sync, so a new alias landing
// there is auto-rewritten here too. Returns the number of items rewritten.
func fixLegacyAliases(s *store.Store, _ *config.Config) int {
	var fixed int
	for _, item := range s.All() {
		if item.Doc == nil {
			continue
		}
		newStatus := validate.SuggestStatus(item.Status)
		if newStatus == "" {
			continue
		}
		itemID := item.ID
		oldStatus := item.Status
		target := newStatus
		if err := s.Mutate(itemID, func(item *model.Item) error {
			item.Status = target
			item.Doc.SetField("status", target)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
			continue
		}
		fixed++
		fmt.Printf("  \033[33m⟳\033[0m %s: rewrote legacy status alias %q → %q\n", itemID, oldStatus, target)
	}
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

		// Determine which fields need inserting before taking the lock.
		var missingFields []string
		for _, field := range tc.RequiredFields {
			if !validate.HasField(item.Doc, field) {
				missingFields = append(missingFields, field)
			}
		}
		if len(missingFields) == 0 {
			continue
		}

		itemID := item.ID
		fieldsToInsert := missingFields
		if err := s.Mutate(itemID, func(item *model.Item) error {
			for _, field := range fieldsToInsert {
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
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
			continue
		}
		for _, field := range missingFields {
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: inserted missing field %q\n", itemID, field)
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

		itemID := item.ID
		capturedDeps := newDeps
		capturedBlocks := newBlocks
		capturedDepsChanged := depsChanged
		capturedBlocksChanged := blocksChanged
		if err := s.Mutate(itemID, func(item *model.Item) error {
			if capturedDepsChanged {
				item.DependsOn = capturedDeps
				item.Doc.SetList("depends_on", capturedDeps)
			}
			if capturedBlocksChanged {
				item.Blocks = capturedBlocks
				item.Doc.SetList("blocks", capturedBlocks)
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
			continue
		}
		if depsChanged {
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: normalized depends_on slugs\n", itemID)
		}
		if blocksChanged {
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: normalized blocks slugs\n", itemID)
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
			dirtyItem := items[id]
			capturedBlocks := append([]string(nil), dirtyItem.Blocks...)
			capturedDependsOn := append([]string(nil), dirtyItem.DependsOn...)
			if err := s.Mutate(id, func(item *model.Item) error {
				item.Blocks = capturedBlocks
				item.Doc.SetList("blocks", capturedBlocks)
				item.DependsOn = capturedDependsOn
				item.Doc.SetList("depends_on", capturedDependsOn)
				return nil
			}); err != nil {
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
			capturedCleanDeps := cleanDeps
			if err := s.Mutate(id, func(item *model.Item) error {
				item.DependsOn = capturedCleanDeps
				if len(capturedCleanDeps) == 0 {
					item.Doc.SetList("depends_on", []string{})
				} else {
					item.Doc.SetList("depends_on", capturedCleanDeps)
				}
				return nil
			}); err != nil {
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
		itemID := item.ID
		archiveGate := cfg.Delivery.ArchiveGate
		if err := s.Mutate(itemID, func(item *model.Item) error {
			item.SetNested("delivery", "stage", archiveGate)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error writing %s: %v\n", itemID, err)
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

// fixDuplicateBlocks removes duplicate delivery: blocks that arise when st
// close appends a new block instead of updating the existing one. The YAML
// parser reads the LAST delivery block while SetNestedField updates the FIRST,
// creating an infinite fixDeliveryGate loop. Only delivery: is targeted — other
// blocks (time_tracking, work_tracking) may have intentional multi-occurrence
// semantics and should not be touched here.
func fixDuplicateBlocks(s *store.Store, _ *config.Config) int {
	var fixed int
	for _, item := range s.All() {
		if item.Doc == nil || !item.Doc.HasDuplicateDeliveryBlock() {
			continue
		}
		itemID := item.ID
		var n int
		if err := s.Mutate(itemID, func(item *model.Item) error {
			n = item.Doc.RemoveDuplicateDeliveryBlock()
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error fixing duplicate delivery block on %s: %v\n", itemID, err)
			continue
		}
		if n > 0 {
			fixed++
			fmt.Printf("  \033[33m⟳\033[0m %s: removed duplicate delivery block (%d line(s))\n", itemID, n)
		}
	}
	return fixed
}

// fixDirectoryMismatch moves items whose file sits in the wrong directory for
// their current status. This repairs the gap left by `st update status <X>`
// which writes the status field but does not call Store.Move — so items can
// end up in archive/ with status queued, or in issues/ with status archived.
func fixDirectoryMismatch(s *store.Store, cfg *config.Config) (int, []string) {
	var fixed int
	var movedPaths []string
	for _, item := range s.All() {
		if item.Doc == nil || item.Type == "" {
			continue
		}
		path, ok := s.Path(item.ID)
		if !ok {
			continue
		}
		expectedDir := cfg.DirectoryForStatus(item.Type, item.Status)
		if expectedDir == "" {
			continue
		}
		actualDir := filepath.Base(filepath.Dir(path))
		if actualDir == expectedDir {
			continue
		}
		itemID := item.ID
		newPath, err := s.Move(itemID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  move-failed: %s — %v\n", itemID, err)
			continue
		}
		fixed++
		fmt.Printf("  \033[33m⟳\033[0m %s: moved from %q to %q (status %q)\n",
			itemID, actualDir, expectedDir, item.Status)
		movedPaths = append(movedPaths, newPath)
	}
	return fixed, movedPaths
}

// fixOrphanDeliveryStage clears delivery.stage when it is "closed" on an item
// whose status is non-terminal. This repairs items that were briefly closed
// and then re-opened via `st update status queued` — the re-open path does not
// reset delivery.stage, leaving a stale "closed" that appears in `st list`.
func fixOrphanDeliveryStage(s *store.Store, cfg *config.Config) int {
	var fixed int
	for _, item := range s.All() {
		if item.Doc == nil || item.Type == "" {
			continue
		}
		if cfg.IsTerminalStatus(item.Type, item.Status) {
			continue
		}
		stage, _ := item.Delivery["stage"].(string)
		if stage != "closed" {
			continue
		}
		itemID := item.ID
		if err := s.Mutate(itemID, func(item *model.Item) error {
			item.Doc.RemoveNestedField("delivery.stage")
			if item.Delivery != nil {
				delete(item.Delivery, "stage")
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  error clearing delivery.stage on %s: %v\n", itemID, err)
			continue
		}
		fixed++
		fmt.Printf("  \033[33m⟳\033[0m %s: cleared stale delivery.stage=closed (status is non-terminal)\n", itemID)
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
