package command

import (
	"github.com/jfinlinson/agent-state/internal/model"
)

// filterByAgent returns the subset of items whose AssignedTo matches
// `agent`, plus unassigned items (claimable by anyone). This is the
// T-347 default scope rule for agent-facing renderers: from inside an
// agent workspace, the operator sees what they own and what's still
// open to claim — never what's already claimed by a peer.
//
// Unassigned items are kept so the renderer doesn't hide work that's
// awaiting an owner. Peer-assigned items are dropped.
func filterByAgent(items []*model.Item, agent string) []*model.Item {
	if agent == "" {
		return items
	}
	out := make([]*model.Item, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		if it.AssignedTo == "" || it.AssignedTo == agent {
			out = append(out, it)
		}
	}
	return out
}

// filterPrimeItemsByAgent applies the same self-or-unassigned rule
// as filterByAgent but to the primeItem export shape used by
// `st prime` (which serializes for LLM consumers). T-347.
func filterPrimeItemsByAgent(items []primeItem, agent string) []primeItem {
	if agent == "" {
		return items
	}
	out := make([]primeItem, 0, len(items))
	for _, it := range items {
		if it.Assigned == "" || it.Assigned == agent {
			out = append(out, it)
		}
	}
	return out
}

// classifyAgentOwnership splits a queue position's owner into one of
// three visual buckets used by `st queue show`:
//
//	"self":   AssignedTo == currentAgent
//	"open":   AssignedTo == "" (claimable by anyone)
//	"peer":   AssignedTo == someone else
//
// When currentAgent is empty (workspace root), every item lands in
// "self" so the legacy single-style rendering is preserved.
func classifyAgentOwnership(item *model.Item, currentAgent string) string {
	if currentAgent == "" {
		return "self"
	}
	if item == nil {
		return "open"
	}
	switch item.AssignedTo {
	case "":
		return "open"
	case currentAgent:
		return "self"
	default:
		return "peer"
	}
}
