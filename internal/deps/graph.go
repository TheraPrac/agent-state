// Package deps provides dependency graph computation for agent-state items.
// It builds a DAG from depends_on fields, computes inverse blocks,
// detects cycles, and provides ready-queue queries.
package deps

import (
	"fmt"
	"sort"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// Graph represents the dependency DAG for all items.
type Graph struct {
	// Forward edges: ID -> IDs it depends on
	DependsOn map[string][]string
	// Inverse edges: ID -> IDs it blocks (computed)
	Blocks map[string][]string
	// All items
	Items map[string]*model.Item
	// Config for terminal status checks
	Cfg *config.Config
}

// Build constructs a dependency graph from all items.
func Build(items map[string]*model.Item, cfg *config.Config) *Graph {
	g := &Graph{
		DependsOn: make(map[string][]string),
		Blocks:    make(map[string][]string),
		Items:     items,
		Cfg:       cfg,
	}

	for id, item := range items {
		g.DependsOn[id] = item.DependsOn
		// Build inverse
		for _, depID := range item.DependsOn {
			g.Blocks[depID] = append(g.Blocks[depID], id)
		}
	}

	return g
}

// BlockedBy returns the IDs that block the given item (its depends_on list).
func (g *Graph) BlockedBy(id string) []string {
	return g.DependsOn[id]
}

// BlocksItems returns the IDs that the given item blocks.
func (g *Graph) BlocksItems(id string) []string {
	return g.Blocks[id]
}

// IsResolved returns true if the given dependency is resolved
// (terminal status or delivery stage >= merged).
func (g *Graph) IsResolved(id string) bool {
	item, ok := g.Items[id]
	if !ok {
		return false // missing items are not resolved
	}

	// Check terminal status
	tc, ok := g.Cfg.Types[item.Type]
	if ok {
		for _, ts := range tc.TerminalStatuses {
			if item.Status == ts {
				return true
			}
		}
	}

	// Check delivery stage >= merged (if delivery info exists)
	if stage, ok := item.Delivery["stage"]; ok {
		if s, ok := stage.(string); ok && s != "" {
			if isStageAtOrPast(s, "merged") {
				return true
			}
		}
	}

	return false
}

// IsBlocked returns true if the item has unresolved dependencies.
func (g *Graph) IsBlocked(id string) bool {
	for _, depID := range g.DependsOn[id] {
		if !g.IsResolved(depID) {
			return true
		}
	}
	return false
}

// UnresolvedDeps returns the unresolved dependencies for an item.
func (g *Graph) UnresolvedDeps(id string) []string {
	var unresolved []string
	for _, depID := range g.DependsOn[id] {
		if !g.IsResolved(depID) {
			unresolved = append(unresolved, depID)
		}
	}
	return unresolved
}

// Ready returns items that are unblocked and in a start status (e.g., queued),
// sorted by priority (lower number = higher priority).
func (g *Graph) Ready() []*model.Item {
	var ready []*model.Item

	for id, item := range g.Items {
		tc, ok := g.Cfg.Types[item.Type]
		if !ok {
			continue
		}
		// Must be in start status
		if item.Status != tc.StartStatus {
			continue
		}
		// Must not be blocked
		if g.IsBlocked(id) {
			continue
		}
		// Must not be assigned to anyone
		if item.AssignedTo != "" {
			continue
		}
		ready = append(ready, item)
	}

	// Sort by priority (nil/missing = 2 default), then by ID
	sort.Slice(ready, func(i, j int) bool {
		pi := priorityOf(ready[i])
		pj := priorityOf(ready[j])
		if pi != pj {
			return pi < pj
		}
		return ready[i].ID < ready[j].ID
	})

	return ready
}

// DetectCycles returns any cycles found in the dependency graph.
// Each cycle is a slice of IDs forming the cycle.
func (g *Graph) DetectCycles() [][]string {
	var cycles [][]string
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	path := make([]string, 0)

	var visit func(id string) bool
	visit = func(id string) bool {
		if inStack[id] {
			// Found a cycle — extract it
			cycleStart := -1
			for i, pid := range path {
				if pid == id {
					cycleStart = i
					break
				}
			}
			if cycleStart >= 0 {
				cycle := make([]string, len(path)-cycleStart)
				copy(cycle, path[cycleStart:])
				cycles = append(cycles, cycle)
			}
			return true
		}
		if visited[id] {
			return false
		}

		visited[id] = true
		inStack[id] = true
		path = append(path, id)

		for _, depID := range g.DependsOn[id] {
			visit(depID)
		}

		path = path[:len(path)-1]
		inStack[id] = false
		return false
	}

	for id := range g.Items {
		if !visited[id] {
			visit(id)
		}
	}

	return cycles
}

// Tree returns a text representation of the dependency tree for an item.
func (g *Graph) Tree(id string, maxDepth int) string {
	seen := make(map[string]bool)
	return g.treeHelper(id, 0, maxDepth, seen)
}

func (g *Graph) treeHelper(id string, depth, maxDepth int, seen map[string]bool) string {
	if depth > maxDepth {
		return ""
	}

	item, exists := g.Items[id]
	prefix := ""
	for i := 0; i < depth; i++ {
		prefix += "  "
	}

	line := prefix
	if depth > 0 {
		line += "└─ "
	}

	if !exists {
		return line + id + " (not found)\n"
	}

	status := item.Status
	if seen[id] {
		return line + fmt.Sprintf("%s  %s  [circular]\n", id, status)
	}
	seen[id] = true

	result := line + fmt.Sprintf("%s  %s  %s\n", id, status, item.Title)

	// Show what this item depends on
	for _, depID := range g.DependsOn[id] {
		result += g.treeHelper(depID, depth+1, maxDepth, seen)
	}

	return result
}

func priorityOf(item *model.Item) int {
	if item.Priority != nil {
		return *item.Priority
	}
	return 2 // default priority
}

// isStageAtOrPast checks if stage is at or past target in the default pipeline.
// This is a simplified check — full implementation will use config.Delivery.Stages.
func isStageAtOrPast(stage, target string) bool {
	stages := []string{
		"coding", "committed", "pushed", "pr_open", "reviewed",
		"merged", "deployed_dev", "smoke_passed", "closed",
	}
	stageIdx := -1
	targetIdx := -1
	for i, s := range stages {
		if s == stage {
			stageIdx = i
		}
		if s == target {
			targetIdx = i
		}
	}
	if stageIdx < 0 || targetIdx < 0 {
		return false
	}
	return stageIdx >= targetIdx
}
