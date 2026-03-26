package command

import (
	"github.com/jfinlinson/agent-state/internal/model"
)

// setNestedField writes a value into a nested map on the item and updates the Doc.
// Example: setNestedField(item, "time_tracking", "started_at", "2026-03-26T10:00:00-06:00")
func setNestedField(item *model.Item, parent, key, val string) {
	// Update the in-memory map
	m := nestedMap(item, parent)
	if m != nil {
		m[key] = val
	}

	// Update the document for roundtrip fidelity
	if item.Doc == nil {
		return
	}

	// Find the parent key line, then find or insert the nested key
	parentIdx := -1
	for i, line := range item.Doc.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
			break
		}
	}

	if parentIdx < 0 {
		// Parent not found — append parent and nested key
		item.Doc.Lines = append(item.Doc.Lines,
			model.Line{Raw: "", IsEmpty: true},
			model.Line{Raw: parent + ":", Key: parent},
			model.Line{Raw: "  " + key + ": " + val, Key: key, Indent: 2, BlockKey: parent},
		)
		return
	}

	// Search for existing nested key within the parent block
	for i := parentIdx + 1; i < len(item.Doc.Lines); i++ {
		line := item.Doc.Lines[i]
		if line.Indent == 0 && !line.IsEmpty {
			break // left the parent block
		}
		if line.Key == key && line.Indent > 0 && line.BlockKey == parent {
			// Update existing
			item.Doc.Lines[i].Raw = "  " + key + ": " + val
			item.Doc.Lines[i].Value = val
			return
		}
	}

	// Not found in block — insert after parent line
	newLine := model.Line{Raw: "  " + key + ": " + val, Key: key, Value: val, Indent: 2, BlockKey: parent}
	after := parentIdx + 1
	item.Doc.Lines = append(item.Doc.Lines[:after], append([]model.Line{newLine}, item.Doc.Lines[after:]...)...)
}

// getNestedField reads a value from a nested map on the item.
func getNestedField(item *model.Item, parent, key string) (string, bool) {
	m := nestedMap(item, parent)
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// nestedMap returns the map for a given parent field name.
func nestedMap(item *model.Item, parent string) map[string]interface{} {
	switch parent {
	case "work_tracking":
		return item.WorkTracking
	case "delivery":
		return item.Delivery
	case "testing_evidence":
		return item.TestingEvidence
	case "time_tracking":
		if item.TimeTracking == nil {
			item.TimeTracking = make(map[string]interface{})
		}
		return item.TimeTracking
	case "manifest":
		return item.Manifest
	default:
		return nil
	}
}
