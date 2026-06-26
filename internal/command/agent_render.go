package command

import (
	"github.com/theraprac/agent-state/internal/model"
)

// formatAssignment returns a human-readable assignment label that includes
// sub-agent heritage when present. Examples:
//
//	"agent-b"                              — no heritage
//	"agent-b ← agent-a"                    — child of agent-a, where root is
//	                                         either unrecorded or equal to parent
//	"agent-b ← agent-a (root: agent-x)"    — deeper chain (root differs from parent)
//
// Returns empty string when the item has no assignment.
func formatAssignment(item *model.Item) string {
	if item == nil || item.AssignedTo == "" {
		return ""
	}
	if item.Doc == nil {
		return item.AssignedTo
	}
	parent, _ := item.Doc.GetNestedField("assigned_to_meta.parent_id")
	if parent == "" {
		return item.AssignedTo
	}
	root, _ := item.Doc.GetNestedField("assigned_to_meta.root_id")
	if root == "" || root == parent {
		return item.AssignedTo + " ← " + parent
	}
	return item.AssignedTo + " ← " + parent + " (root: " + root + ")"
}
