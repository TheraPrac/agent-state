// Package model defines the core data types for agent-state items.
package model

import "time"

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

	// Not found — append before next_actions or at the end
	newLine := Line{
		Raw:   key + ": " + value,
		Key:   key,
		Value: value,
	}
	d.Lines = append(d.Lines, newLine)
	return false
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
		// Key not found — append key + list
		d.Lines = append(d.Lines, Line{Raw: key + ":", Key: key})
		for _, item := range items {
			d.Lines = append(d.Lines, Line{Raw: "- " + item, IsList: true})
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
