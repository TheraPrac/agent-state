// Package model defines the core data types for agent-state items.
package model

import (
	"strings"
	"time"
)

// Item represents a task, issue, or idea in agent-state.
// Fields are intentionally loose (string maps for nested data) because
// the parser must handle arbitrary YAML-like frontmatter without loss.
type Item struct {
	// Core fields (required)
	ID          string
	Type        string // task, issue, idea
	Status      string
	Title       string
	Created     time.Time
	LastTouched time.Time

	// Optional scalar fields
	Completed      *time.Time
	Priority       *int
	Severity       string // issues only: critical, high, medium, low
	Category       string
	Repo           string
	Summary        string
	AssignedTo     string // agent ID
	LastTouchedBy  string // agent ID
	Epic           string // epic ID (adjective-verb-noun)
	Sprint         string // sprint ID (adjective-verb-noun)
	ClaimedBy      string // session UUID that has claimed this item
	ClaimedAt      string // RFC3339 timestamp of when claimed
	PlanApproved   bool   // design/plan gate passed

	// List fields
	Tags               []string
	Sessions           []string // Claude Code session UUIDs
	DependsOn          []string
	Blocks             []string // NOTE: in current format this is stored; after migration it's computed
	RelatedIssues      []string
	AcceptanceCriteria []string
	NextActions        []string
	Resolution         []string
	Invariants         []string
	DocChanges         []string
	LinkedPlans        []string

	// Nested structures (kept as raw key-value for flexibility)
	WorkTracking    map[string]interface{}
	Delivery        map[string]interface{}
	TestingEvidence map[string]interface{}
	TimeTracking    map[string]interface{}
	Manifest        map[string]interface{}

	// Context and other multiline blocks
	Context string

	// The full parsed document (for roundtrip fidelity)
	Doc *ParsedDocument
}

// ParsedDocument retains the raw line structure of a file for lossless roundtrip.
// The parser produces this, and the writer serializes from it.
type ParsedDocument struct {
	Lines []Line
}

// Line represents a single line in the parsed document.
type Line struct {
	Raw string // original text exactly as read

	// Parsed metadata (set for lines the parser understood)
	Key      string // field key, if this is a key:value line
	Value    string // field value (trimmed)
	Indent   int    // leading whitespace count
	IsList   bool   // starts with "- "
	IsBlock  bool   // part of a | or > block
	IsEmpty  bool   // blank line
	Comment  string // inline comment (after " #")
	BlockKey string // for nested lines, the parent key they belong to
}

// NewParsedDocument creates an empty ParsedDocument.
func NewParsedDocument() *ParsedDocument {
	return &ParsedDocument{}
}

// SetField updates or inserts a scalar field value in the document.
// Returns true if the field was found and updated, false if inserted.
func (d *ParsedDocument) SetField(key, value string) bool {
	for i, line := range d.Lines {
		if line.Key == key && line.Indent == 0 {
			// Preserve inline comment if present
			newRaw := key + ": " + value
			if line.Comment != "" {
				newRaw += "  # " + line.Comment
			}
			d.Lines[i].Raw = newRaw
			d.Lines[i].Value = value
			return true
		}
	}

	// Not found — insert before body separator (---) or at end
	newLine := Line{
		Raw:   key + ": " + value,
		Key:   key,
		Value: value,
	}
	if idx := d.bodySeparatorIndex(); idx >= 0 {
		d.Lines = append(d.Lines[:idx+1], d.Lines[idx:]...)
		d.Lines[idx] = newLine
	} else {
		d.Lines = append(d.Lines, newLine)
	}
	return false
}

// ReplaceList replaces an entire list field (key + all continuation lines)
// with the new lines. Each line in values should be "- item text".
// Supports dotted paths (e.g., "testing_evidence.api_unit") for nested fields.
func (d *ParsedDocument) ReplaceList(key string, values []string) {
	// Handle dotted paths for nested fields
	if parts := strings.SplitN(key, ".", 2); len(parts) == 2 {
		d.replaceNestedList(parts[0], parts[1], values)
		return
	}

	// Find the key line
	keyIdx := -1
	for i, line := range d.Lines {
		if line.Key == key && line.Indent == 0 {
			keyIdx = i
			break
		}
	}

	if keyIdx < 0 {
		// Key not found — append
		d.Lines = append(d.Lines, Line{Raw: key + ":", Key: key})
		for _, v := range values {
			d.Lines = append(d.Lines, Line{Raw: v, Indent: 0, BlockKey: key})
		}
		return
	}

	// Find the end of the list block (next key at indent 0, or empty line followed by key)
	endIdx := keyIdx + 1
	for endIdx < len(d.Lines) {
		l := d.Lines[endIdx]
		// Stop at next top-level key (not a list continuation)
		if l.Key != "" && l.Indent == 0 && l.Key != key {
			break
		}
		// List items start with "- " or are indented continuations
		raw := strings.TrimSpace(l.Raw)
		if raw == "" || strings.HasPrefix(raw, "- ") || l.BlockKey == key {
			endIdx++
			continue
		}
		// Some other content at indent 0 — stop
		if l.Indent == 0 && !strings.HasPrefix(raw, "- ") {
			break
		}
		endIdx++
	}

	// Build new lines
	newLines := []Line{{Raw: key + ":", Key: key}}
	for _, v := range values {
		newLines = append(newLines, Line{Raw: v, BlockKey: key})
	}

	// Replace keyIdx..endIdx with newLines
	result := make([]Line, 0, len(d.Lines)-endIdx+keyIdx+len(newLines))
	result = append(result, d.Lines[:keyIdx]...)
	result = append(result, newLines...)
	result = append(result, d.Lines[endIdx:]...)
	d.Lines = result
}

// replaceNestedList replaces a nested field's content (child key + continuation
// lines) under a parent block. Used by ReplaceList for dotted paths.
func (d *ParsedDocument) replaceNestedList(parent, child string, values []string) {
	// Find the parent key line
	parentIdx := -1
	for i, line := range d.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
			break
		}
	}

	if parentIdx < 0 {
		// Parent not found — append parent + child + values
		newLines := []Line{
			{Raw: parent + ":", Key: parent},
			{Raw: "  " + child + ":", Key: child, Indent: 2, BlockKey: parent},
		}
		for _, v := range values {
			newLines = append(newLines, Line{Raw: "  " + v, Indent: 2, BlockKey: parent})
		}
		d.Lines = append(d.Lines, newLines...)
		return
	}

	// Find the child key under parent
	childIdx := -1
	for i := parentIdx + 1; i < len(d.Lines); i++ {
		line := d.Lines[i]
		if line.Indent == 0 && !line.IsEmpty {
			break // left parent block
		}
		if line.Key == child && line.Indent > 0 {
			childIdx = i
			break
		}
	}

	if childIdx < 0 {
		// Child not found — insert after last nested line of parent
		insertIdx := parentIdx + 1
		for insertIdx < len(d.Lines) {
			if d.Lines[insertIdx].Indent == 0 && !d.Lines[insertIdx].IsEmpty {
				break
			}
			insertIdx++
		}
		newLines := []Line{
			{Raw: "  " + child + ":", Key: child, Indent: 2, BlockKey: parent},
		}
		for _, v := range values {
			newLines = append(newLines, Line{Raw: "  " + v, Indent: 2, BlockKey: parent})
		}
		tail := make([]Line, len(d.Lines[insertIdx:]))
		copy(tail, d.Lines[insertIdx:])
		d.Lines = append(d.Lines[:insertIdx], append(newLines, tail...)...)
		return
	}

	// Find end of child's block (next sibling key at same indent, or end of parent)
	childIndent := d.Lines[childIdx].Indent
	endIdx := childIdx + 1
	for endIdx < len(d.Lines) {
		l := d.Lines[endIdx]
		// Left the parent block entirely
		if l.Indent == 0 && !l.IsEmpty {
			break
		}
		// Sibling key at same or lesser indent (but not empty)
		if l.Key != "" && l.Indent <= childIndent && !l.IsEmpty {
			break
		}
		// Continuation: deeper indent, list items, or empty lines
		endIdx++
	}

	// Build replacement lines preserving the child's indent
	indent := strings.Repeat(" ", childIndent)
	newLines := []Line{
		{Raw: indent + child + ":", Key: child, Indent: childIndent, BlockKey: parent},
	}
	for _, v := range values {
		newLines = append(newLines, Line{Raw: indent + v, Indent: childIndent, BlockKey: parent})
	}

	// Replace childIdx..endIdx with newLines
	result := make([]Line, 0, len(d.Lines)-endIdx+childIdx+len(newLines))
	result = append(result, d.Lines[:childIdx]...)
	result = append(result, newLines...)
	result = append(result, d.Lines[endIdx:]...)
	d.Lines = result
}

// GetField returns the value for a top-level scalar field.
func (d *ParsedDocument) GetField(key string) (string, bool) {
	for _, line := range d.Lines {
		if line.Key == key && line.Indent == 0 {
			return line.Value, true
		}
	}
	return "", false
}

// SetNestedField updates a nested field using dotted-path syntax (e.g. "work_tracking.branch").
// Returns true if the field was found and updated.
func (d *ParsedDocument) SetNestedField(path, value string) bool {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) != 2 {
		return d.SetField(path, value)
	}
	parent, child := parts[0], parts[1]

	// Find the parent key line
	parentIdx := -1
	for i, line := range d.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
			break
		}
	}

	if parentIdx < 0 {
		// Parent not found — insert parent + child before body separator
		parentLine := Line{Raw: parent + ":", Key: parent}
		childLine := Line{Raw: "  " + child + ": " + value, Key: child, Value: value, Indent: 2, BlockKey: parent}
		if idx := d.bodySeparatorIndex(); idx >= 0 {
			tail := make([]Line, len(d.Lines[idx:]))
			copy(tail, d.Lines[idx:])
			d.Lines = append(d.Lines[:idx], append([]Line{parentLine, childLine}, tail...)...)
		} else {
			d.Lines = append(d.Lines, parentLine, childLine)
		}
		return false
	}

	// Search nested lines under parent
	for i := parentIdx + 1; i < len(d.Lines); i++ {
		line := d.Lines[i]
		if line.Indent == 0 && !line.IsEmpty {
			break // left the parent block
		}
		if line.Key == child && line.Indent > 0 {
			newRaw := strings.Repeat(" ", line.Indent) + child + ": " + value
			if line.Comment != "" {
				newRaw += "  # " + line.Comment
			}
			d.Lines[i].Raw = newRaw
			d.Lines[i].Value = value
			return true
		}
	}

	// Child not found under parent — insert after last nested line of parent
	insertIdx := parentIdx + 1
	for insertIdx < len(d.Lines) {
		if d.Lines[insertIdx].Indent == 0 && !d.Lines[insertIdx].IsEmpty {
			break
		}
		insertIdx++
	}
	childLine := Line{Raw: "  " + child + ": " + value, Key: child, Value: value, Indent: 2, BlockKey: parent}
	tail := make([]Line, len(d.Lines[insertIdx:]))
	copy(tail, d.Lines[insertIdx:])
	d.Lines = append(d.Lines[:insertIdx], append([]Line{childLine}, tail...)...)
	return false
}

// GetNestedField returns the value for a dotted-path field (e.g. "work_tracking.branch").
func (d *ParsedDocument) GetNestedField(path string) (string, bool) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) != 2 {
		return d.GetField(path)
	}
	parent, child := parts[0], parts[1]

	inParent := false
	for _, line := range d.Lines {
		if line.Key == parent && line.Indent == 0 {
			inParent = true
			continue
		}
		if inParent {
			if line.Indent == 0 && !line.IsEmpty {
				return "", false // left parent block
			}
			if line.Key == child && line.Indent > 0 {
				return line.Value, true
			}
		}
	}
	return "", false
}

// SetList replaces a top-level list field's items in the document.
// It finds the key line, clears any inline value, removes old list items,
// and inserts new "- item" lines. Returns true if the key was found.
func (d *ParsedDocument) SetList(key string, items []string) bool {
	// Find the key line
	keyIdx := -1
	for i, line := range d.Lines {
		if line.Key == key && line.Indent == 0 {
			keyIdx = i
			break
		}
	}
	if keyIdx < 0 {
		// Key not found — insert before body separator (---) or at end
		insertIdx := d.bodySeparatorIndex()
		var newLines []Line
		newLines = append(newLines, Line{Raw: key + ":", Key: key})
		if len(items) == 0 {
			newLines = append(newLines, Line{Raw: "- []", IsList: true})
		} else {
			for _, item := range items {
				newLines = append(newLines, Line{Raw: "- " + item, IsList: true})
			}
		}

		if insertIdx >= 0 {
			tail := make([]Line, len(d.Lines[insertIdx:]))
			copy(tail, d.Lines[insertIdx:])
			d.Lines = append(d.Lines[:insertIdx], append(newLines, tail...)...)
		} else {
			d.Lines = append(d.Lines, newLines...)
		}
		return false
	}

	// Update key line to have no inline value
	d.Lines[keyIdx].Raw = key + ":"
	d.Lines[keyIdx].Value = ""

	// Find the range of old list items following the key
	end := keyIdx + 1
	for end < len(d.Lines) {
		l := d.Lines[end]
		if l.IsList && l.Indent == 0 {
			end++
			continue
		}
		// Skip "- []" empty markers that weren't flagged as IsList
		if l.Indent == 0 && !l.IsEmpty && l.Key == "" && !l.IsList {
			// Could be an orphaned "- []" line — check raw
			trimmed := l.Raw
			for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
				trimmed = trimmed[1:]
			}
			if len(trimmed) >= 2 && trimmed[0] == '-' && trimmed[1] == ' ' {
				end++
				continue
			}
			break
		}
		break
	}

	// Build new list lines
	var newLines []Line
	if len(items) == 0 {
		newLines = append(newLines, Line{Raw: "- []", IsList: true})
	} else {
		for _, item := range items {
			newLines = append(newLines, Line{Raw: "- " + item, IsList: true})
		}
	}

	// Replace old list items with new ones
	tail := make([]Line, len(d.Lines[end:]))
	copy(tail, d.Lines[end:])
	d.Lines = append(d.Lines[:keyIdx+1], append(newLines, tail...)...)
	return true
}

// bodySeparatorIndex returns the index of the first "---" line (body separator),
// or -1 if no separator exists. Used to insert new fields in the frontmatter
// section rather than after the markdown body.
func (d *ParsedDocument) bodySeparatorIndex() int {
	for i, line := range d.Lines {
		if strings.TrimSpace(line.Raw) == "---" {
			return i
		}
	}
	return -1
}

// String serializes the document back to its original text.
func (d *ParsedDocument) String() string {
	if len(d.Lines) == 0 {
		return ""
	}
	var b []byte
	for i, line := range d.Lines {
		b = append(b, []byte(line.Raw)...)
		if i < len(d.Lines)-1 {
			b = append(b, '\n')
		}
	}
	return string(b)
}
