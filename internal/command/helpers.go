package command

import (
	"fmt"
	"time"

	"github.com/theraprac/agent-state/internal/model"
)

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

// readFloatField reads a nested field and parses it as float64; returns 0 if missing/unparseable.
func readFloatField(item *model.Item, parent, key string) float64 {
	if val, exists := getNestedField(item, parent, key); exists {
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	}
	return 0
}

// readIntField reads a nested field and parses it as int; returns 0 if missing/unparseable.
func readIntField(item *model.Item, parent, key string) int {
	if val, exists := getNestedField(item, parent, key); exists {
		var i int
		fmt.Sscanf(val, "%d", &i)
		return i
	}
	return 0
}

