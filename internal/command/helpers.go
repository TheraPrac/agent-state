package command

import (
	"fmt"
	"strconv"
	"time"

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
// It handles string, int, and float64 types (YAML may parse numeric values as int or float64).
func getNestedField(item *model.Item, parent, key string) (string, bool) {
	m := nestedMap(item, parent)
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch val := v.(type) {
	case string:
		return val, true
	case int:
		return fmt.Sprintf("%d", val), true
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), true
	default:
		return fmt.Sprintf("%v", val), true
	}
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
		if item.Manifest == nil {
			item.Manifest = make(map[string]interface{})
		}
		return item.Manifest
	default:
		return nil
	}
}

// formatDuration formats a duration as "Xd Xh Xm Xs", omitting zero components.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	total := int(d.Seconds())
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60

	// Fixed format: show the two most significant non-zero units
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	result := fmt.Sprintf("%ds", seconds)
	return result
}

// formatTokens formats a token count as human-readable: 1.2K, 3.5M, 1.1B, 2.0T.
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	if n < 1_000_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n < 1_000_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	return fmt.Sprintf("%.1fT", float64(n)/1_000_000_000_000)
}

// formatLOC formats a net LOC count as human-readable with +/- prefix: +1.2K, -350, +2.5M.
func formatLOC(n int) string {
	sign := "+"
	abs := n
	if n < 0 {
		sign = ""
		abs = -n
	}
	if abs < 1000 {
		return fmt.Sprintf("%s%d", sign, n)
	}
	if abs < 1_000_000 {
		return fmt.Sprintf("%s%.1fK", sign, float64(n)/1_000)
	}
	return fmt.Sprintf("%s%.1fM", sign, float64(n)/1_000_000)
}
