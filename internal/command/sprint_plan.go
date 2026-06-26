package command

import (
	"fmt"
	"os"
	"sort"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/store"
)

// SprintPlan analyzes a sprint's items using the dependency graph,
// grouping items into parallel execution groups and flagging cross-sprint deps.
func SprintPlan(s *store.Store, cfg *config.Config, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	sp, err := r.SprintByID(sprintID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if len(sp.Items) == 0 {
		fmt.Printf("Sprint: %s — %s\n\n(no items)\n", sp.ID, sp.Title)
		return 0
	}

	// Build sets for quick lookup
	sprintItems := make(map[string]bool)
	for _, id := range sp.Items {
		sprintItems[id] = true
	}

	// Build intra-sprint dependency graph
	// For each item, find which of its depends_on are also in this sprint
	intraDeps := make(map[string][]string) // id -> [sprint deps it depends on]
	var crossDeps []string                 // items with cross-sprint dependencies

	for _, itemID := range sp.Items {
		item, ok := s.Get(itemID)
		if !ok {
			continue
		}
		for _, depID := range item.DependsOn {
			if sprintItems[depID] {
				intraDeps[itemID] = append(intraDeps[itemID], depID)
			} else {
				// Cross-sprint dep — dep is NOT in this sprint
				crossDeps = append(crossDeps, fmt.Sprintf("%s -> %s", itemID, depID))
			}
		}
	}

	// Topological sort into parallel groups
	groups := computeParallelGroups(sp.Items, intraDeps, s)

	// Display
	fmt.Printf("Sprint: %s — %s\n\n", sp.ID, sp.Title)

	for i, group := range groups {
		if len(groups) > 1 {
			fmt.Printf("Parallel group %d", i+1)
			if i == 0 {
				fmt.Printf(" (no deps)")
			}
			fmt.Println(":")
		}
		for _, itemID := range group {
			item, ok := s.Get(itemID)
			status := "ready"
			title := itemID
			if ok {
				title = item.Title
				if cfg.IsTerminalStatus(item.Type, item.Status) {
					status = "done"
				} else {
					tc, ok := cfg.Types[item.Type]
					if ok && item.Status == tc.ActiveStatus {
						status = "active"
					}
				}
			}
			fmt.Printf("  %-8s %-35s %s\n", itemID, title, status)
		}
		fmt.Println()
	}

	// Cross-sprint dependencies
	if len(crossDeps) > 0 {
		// Deduplicate
		seen := make(map[string]bool)
		var unique []string
		for _, d := range crossDeps {
			if !seen[d] {
				seen[d] = true
				unique = append(unique, d)
			}
		}
		sort.Strings(unique)
		fmt.Println("Cross-sprint deps:")
		for _, d := range unique {
			fmt.Printf("  %s\n", d)
		}
	} else {
		fmt.Println("Cross-sprint deps: none")
	}

	if len(groups) <= 1 && len(crossDeps) == 0 {
		fmt.Println("All items parallelizable")
	}

	return 0
}

// computeParallelGroups performs a topological sort of sprint items into
// parallel execution groups. Items with no unsatisfied intra-sprint deps
// are in group 0, items whose deps are all in group 0 are in group 1, etc.
func computeParallelGroups(items []string, intraDeps map[string][]string, s *store.Store) [][]string {
	itemSet := make(map[string]bool)
	for _, id := range items {
		itemSet[id] = true
	}

	assigned := make(map[string]int) // item -> group index
	maxGroup := 0

	// Keep iterating until all items are assigned
	for len(assigned) < len(items) {
		progress := false
		for _, id := range items {
			if _, done := assigned[id]; done {
				continue
			}
			deps := intraDeps[id]
			if len(deps) == 0 {
				assigned[id] = 0
				progress = true
				continue
			}
			// Check if all deps are assigned
			allAssigned := true
			maxDepGroup := 0
			for _, depID := range deps {
				if !itemSet[depID] {
					continue // external dep, ignore
				}
				g, ok := assigned[depID]
				if !ok {
					allAssigned = false
					break
				}
				if g > maxDepGroup {
					maxDepGroup = g
				}
			}
			if allAssigned {
				group := maxDepGroup + 1
				assigned[id] = group
				if group > maxGroup {
					maxGroup = group
				}
				progress = true
			}
		}
		if !progress {
			// Cycle detected or unresolvable — assign remaining to last group
			for _, id := range items {
				if _, done := assigned[id]; !done {
					assigned[id] = maxGroup + 1
				}
			}
			maxGroup++
			break
		}
	}

	// Collect groups
	groups := make([][]string, maxGroup+1)
	for _, id := range items {
		g := assigned[id]
		groups[g] = append(groups[g], id)
	}

	// Sort items within each group for deterministic output
	for i := range groups {
		sort.Strings(groups[i])
	}

	// Remove empty groups
	var result [][]string
	for _, g := range groups {
		if len(g) > 0 {
			result = append(result, g)
		}
	}
	if len(result) == 0 {
		return [][]string{{}}
	}
	return result
}
