package command

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/store"
)

// DepTreeOpts holds flags for the dep tree command.
type DepTreeOpts struct {
	Depth int
}

func DepTree(s *store.Store, cfg *config.Config, id string, opts DepTreeOpts) int {
	_, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	g := deps.Build(s.All(), cfg)

	maxDepth := opts.Depth
	if maxDepth <= 0 {
		maxDepth = 10
	}

	fmt.Print(g.Tree(id, maxDepth))
	return 0
}

// DepGraphOpts holds flags for the dep graph command.
type DepGraphOpts struct {
	JSON bool
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func DepGraph(s *store.Store, cfg *config.Config, opts DepGraphOpts) int {
	g := deps.Build(s.All(), cfg)

	if opts.JSON {
		var edges []graphEdge
		for id, depIDs := range g.DependsOn {
			for _, depID := range depIDs {
				edges = append(edges, graphEdge{From: id, To: depID})
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(edges)
		return 0
	}

	// Text output: show items with dependencies
	for id, depIDs := range g.DependsOn {
		if len(depIDs) == 0 {
			continue
		}
		item, ok := g.Items[id]
		if !ok {
			continue
		}
		status := item.Status
		resolved := "blocked"
		if !g.IsBlocked(id) {
			resolved = "clear"
		}
		fmt.Printf("%-8s %-10s [%s] depends on:", id, status, resolved)
		for _, depID := range depIDs {
			marker := " "
			if g.IsResolved(depID) {
				marker = "✓"
			} else {
				marker = "✗"
			}
			fmt.Printf(" %s %s", marker, depID)
		}
		fmt.Println()
	}

	return 0
}
