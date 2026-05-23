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
	Completed *time.Time
	Priority  *int
	// DEPRECATED (I-406) — read-only for legacy-file round-trip during
	// the deprecation window. New items use Priority instead. Remove
	// once a sweep confirms no agent-state files still carry severity.
	Severity      string
	Category      string
	Repo          string
	Summary       string
	AssignedTo    string // agent ID
	LastTouchedBy string // agent ID
	Epic          string // epic ID (adjective-verb-noun)
	Sprint        string // sprint ID (adjective-verb-noun)
	Arc           string // T-378 (I-712): strategic work-stream grouping
	//                       — sibling of sprint/epic at a longer horizon.
	//                       Any name an operator uses is the arc; not
	//                       predefined. One per item in v1.
	ScopeClass     string // I-776: gate scope class. When set, overrides
	//                       the default required_suites with the class's
	//                       required-suite set from cfg.Testing.ScopeClasses[name].
	//                       Empty = use cfg.Testing.RequiredSuites (default).
	ClaimedBy      string // session UUID that has claimed this item
	ClaimedAt      string // RFC3339 timestamp of when claimed
	PlanApproved   bool   // design/plan gate passed
	PlanApprovedAt string // RFC3339 timestamp; set by `st plan approve` (I-178)
	PlanApprovedBy string // operator or agent ID that approved the plan (I-178)

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

	// SBAR is the I-487 composite content field. The four sub-fields
	// follow the clinical Situation/Background/Assessment/Recommendation
	// handoff convention so issues and tasks always carry symptom +
	// history + diagnosis + proposed-fix instead of a single freeform
	// blob. Legacy items (Summary/Context populated, SBAR empty) keep
	// working unchanged; the migrate-sbar tool seeds Background from
	// Summary.
	SBAR SBAR

	// The full parsed document (for roundtrip fidelity)
	Doc *ParsedDocument
}

// SBAR is the four-section composite content of an item. Each field
// is a free-form string (potentially multiline). An item is "fully
// SBAR'd" when all four are non-empty; a partial SBAR is allowed
// during the legacy-summary deprecation window and is the migration
// tool's default for items that pre-date the schema.
type SBAR struct {
	Situation      string
	Background     string
	Assessment     string
	Recommendation string
}

// IsEmpty reports whether all four SBAR fields are blank — the
// signal that an item is unmigrated (legacy Summary/Context still
// authoritative).
func (s SBAR) IsEmpty() bool {
	return s.Situation == "" && s.Background == "" && s.Assessment == "" && s.Recommendation == ""
}

// SBARPlaceholders are the literal TODO strings written for each
// unfilled SBAR sub-field. cmd/migrate-sbar (the one-shot backfill),
// st create's scaffold lines, and the I-149 substance gate all
// reference this map so a single edit (e.g., a copy-edit pass on
// the placeholder wording) updates every consumer in lockstep.
// Without this single source, the substance gate would silently
// stop matching scaffolds that diverge from the older wording.
var SBARPlaceholders = map[string]string{
	"situation":      "TODO: one-line symptom or trigger that's observable right now",
	"background":     "TODO: prior context — history, code paths, related items",
	"assessment":     "TODO: diagnosis — what's wrong, why, and how confident",
	"recommendation": "TODO: proposed fix — scoped enough to be actionable",
}

// SetNested updates a nested string field on the item, keeping the
// in-memory typed map and the parsed document in sync. It is the single
// canonical write entry point for nested scalars used by command
// handlers (st start, st run, st pr, etc.).
//
// The parent must be one of: work_tracking, delivery, testing_evidence,
// time_tracking, manifest. Unknown parents are written to the document
// only.
func (it *Item) SetNested(parent, key, value string) {
	switch parent {
	case "work_tracking":
		if it.WorkTracking == nil {
			it.WorkTracking = make(map[string]interface{})
		}
		it.WorkTracking[key] = value
	case "delivery":
		if it.Delivery == nil {
			it.Delivery = make(map[string]interface{})
		}
		it.Delivery[key] = value
	case "testing_evidence":
		if it.TestingEvidence == nil {
			it.TestingEvidence = make(map[string]interface{})
		}
		it.TestingEvidence[key] = value
	case "time_tracking":
		if it.TimeTracking == nil {
			it.TimeTracking = make(map[string]interface{})
		}
		it.TimeTracking[key] = value
	case "manifest":
		if it.Manifest == nil {
			it.Manifest = make(map[string]interface{})
		}
		it.Manifest[key] = value
	}
	if it.Doc != nil {
		it.Doc.SetNestedField(parent+"."+key, value)
	}
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

// CanonicalTopLevelKeys is the set of top-level item-schema field
// names the parser recognizes in internal/parse: storeScalar,
// storeList, storeListOfMaps, storeMultiline's top-level cases
// (summary/context), and the storeNestedScalar parent cases that are
// genuine top-level containers (work_tracking, delivery,
// testing_evidence, time_tracking, manifest, sbar). It deliberately
// EXCLUDES storeNestedScalar's `required_suites`/`scope_suites` cases:
// those are second-level keys nested under `testing_evidence`, never
// top-level fields, so they are not block terminators.
//
// It is intentionally NOT a generous "every key seen in the corpus"
// list — the corpus carries ~150 distinct legacy/freeform top-level
// keys, so a generous whitelist is unmaintainable and an omission
// would silently swallow a real field.
//
// This set is used ONLY as the garbage-mode terminator in
// SetSBARBlock: once I-487 dedented col-0 prose has been observed
// inside the block, the rebuild must consume the (possibly
// colon-bearing) prose until it reaches the real next field, and the
// only reliable "real next field" signal there is a canonical key. In
// the absence of dedented prose (a structurally clean block), the
// boundary is simply the first Indent==0 line — see SetSBARBlock — so
// a clean item carrying a legacy non-canonical field right after the
// sbar block is never mis-consumed regardless of this set.
//
// TestCanonicalTopLevelKeys_MatchesParser asserts this stays in sync
// with internal/parse; update both together.
var CanonicalTopLevelKeys = map[string]bool{
	// storeScalar
	"id": true, "type": true, "status": true, "title": true,
	"created": true, "last_touched": true, "completed": true,
	"priority": true, "severity": true, "category": true, "repo": true,
	"assigned_to": true, "last_touched_by": true, "epic": true,
	"sprint": true, "arc": true, "claimed_by": true, "claimed_at": true,
	"plan_approved": true, "plan_approved_at": true,
	"plan_approved_by": true, "parallel_group": true,
	// storeList
	"tags": true, "depends_on": true, "blocks": true,
	"related_issues": true, "acceptance_criteria": true,
	"next_actions": true, "resolution": true, "invariants": true,
	"doc_changes": true, "sessions": true, "linked_plans": true,
	"tests_written": true,
	// storeListOfMaps / storeNestedScalar top-level parents
	"testing_evidence": true, "work_tracking": true, "delivery": true,
	"time_tracking": true, "manifest": true, "sbar": true,
	// storeMultiline top-level
	"summary": true, "context": true,
}

// SetField updates or inserts a scalar field value in the document.
// For multi-line values, the field is rendered as a YAML block scalar
// (`key: |-`) and previously-attached block continuation lines are
// replaced. For single-line values where the existing field had block
// continuation lines, those continuation lines are removed.
// Returns true if the field was found and updated, false if inserted.
func (d *ParsedDocument) SetField(key, value string) bool {
	for i, line := range d.Lines {
		if line.Key == key && line.Indent == 0 {
			// Drop any existing block continuation lines under this key.
			end := i + 1
			for end < len(d.Lines) && d.Lines[end].IsBlock && d.Lines[end].BlockKey == key {
				end++
			}
			newLines := buildScalarOrBlock(key, value, line.Comment)
			tail := append([]Line{}, d.Lines[end:]...)
			d.Lines = append(d.Lines[:i], append(newLines, tail...)...)
			return true
		}
	}

	// Not found — insert before body separator (---) or at end
	newLines := buildScalarOrBlock(key, value, "")
	if idx := d.BodySeparatorIndex(); idx >= 0 {
		tail := append([]Line{}, d.Lines[idx:]...)
		d.Lines = append(d.Lines[:idx], append(newLines, tail...)...)
	} else {
		d.Lines = append(d.Lines, newLines...)
	}
	return false
}

// buildScalarOrBlock produces the line(s) for a top-level field. A value
// containing a newline is emitted as a block scalar; otherwise a single
// `key: value` line. Inline comments are preserved on the key line.
func buildScalarOrBlock(key, value, comment string) []Line {
	if strings.Contains(value, "\n") {
		header := Line{Raw: key + ": |-", Key: key}
		if comment != "" {
			header.Raw += "  # " + comment
			header.Comment = comment
		}
		out := []Line{header}
		for _, ln := range strings.Split(value, "\n") {
			raw := "  " + ln
			out = append(out, Line{Raw: raw, IsBlock: true, BlockKey: key, Indent: 2})
		}
		return out
	}
	raw := key + ": " + value
	if comment != "" {
		raw += "  # " + comment
	}
	return []Line{{Raw: raw, Key: key, Value: value, Comment: comment}}
}

// buildNestedScalarOrBlock produces the line(s) for a nested (indented)
// field under `parent` at the given indent. Nested analogue of
// buildScalarOrBlock: a value containing a newline is emitted as a
// nested block scalar (`<indent>key: |-` followed by body indented two
// spaces further), otherwise a single `<indent>key: value` line.
// Inline comments are preserved on the key line. Used by SetNestedField
// so a multi-line value no longer collapses onto one line and so the
// old block body is rebuilt rather than stranded (I-593).
//
// The key line carries BlockKey=parent and block-body lines carry
// BlockKey=key, matching exactly what internal/parse assigns on
// re-parse (parse.go sets a nested header's BlockKey to its parent and
// a nested block body's BlockKey to the child key). Without this,
// in-session metadata diverges from a re-parse and BlockKey-keyed
// lookups such as RemoveNestedField silently fail. Trailing newlines
// are trimmed before splitting, mirroring buildSBARLines (I-493), so
// no spurious empty body line is baked in.
func buildNestedScalarOrBlock(parent, key, value string, indent int, comment string) []Line {
	pad := strings.Repeat(" ", indent)
	if strings.Contains(value, "\n") {
		header := Line{Raw: pad + key + ": |-", Key: key, Indent: indent, BlockKey: parent}
		if comment != "" {
			header.Raw += "  # " + comment
			header.Comment = comment
		}
		out := []Line{header}
		for _, ln := range strings.Split(strings.TrimRight(value, "\n"), "\n") {
			out = append(out, Line{
				Raw:      pad + "  " + ln,
				IsBlock:  true,
				BlockKey: key,
				Indent:   indent + 2,
			})
		}
		return out
	}
	raw := pad + key + ": " + value
	if comment != "" {
		raw += "  # " + comment
	}
	return []Line{{Raw: raw, Key: key, Value: value, Indent: indent, Comment: comment, BlockKey: parent}}
}

// SetSBARBlock replaces the entire `sbar:` block — header line plus
// all indented continuation lines — with a freshly-rendered version of
// `s`. Each of the four sub-fields renders as a YAML block scalar
// (`  key: |-` followed by indented body lines). Empty sub-fields are
// emitted as `  key: |-` with no body lines (see buildSBARLines),
// preserving the schema invariant from I-487 that all four keys are
// present.
//
// I-493 needed this because SetNestedField only handles single-line
// nested values; SBAR sub-fields are routinely multi-paragraph, so the
// generic path produced malformed YAML when the editor returned a
// multi-line section.
func (d *ParsedDocument) SetSBARBlock(s SBAR) {
	// Find the existing sbar: block (or the insertion point).
	startIdx := -1
	for i, line := range d.Lines {
		if line.Key == "sbar" && line.Indent == 0 {
			startIdx = i
			break
		}
	}

	newLines := buildSBARLines(s)

	if startIdx < 0 {
		// No sbar: block yet — insert before body separator (or append).
		if idx := d.BodySeparatorIndex(); idx >= 0 {
			tail := append([]Line{}, d.Lines[idx:]...)
			d.Lines = append(d.Lines[:idx], append(newLines, tail...)...)
		} else {
			d.Lines = append(d.Lines, newLines...)
		}
		return
	}

	// Find the end of the sbar block. Two regimes, distinguished by
	// whether I-487 dedented col-0 prose is present:
	//
	//   Clean block: the `sbar:` header, its indented sub-fields and
	//   block bodies, then the next field at Indent==0. Here the
	//   boundary is simply the first Indent==0 non-empty line (the
	//   pre-I-593 behavior). This is correct for EVERY structurally
	//   clean item — including legacy items whose next field is a
	//   non-canonical freeform key — so the rebuild never mis-consumes
	//   a real trailing field. No whitelist is consulted in this case.
	//
	//   Corrupt block: I-487 wrote multi-line content un-indented, so
	//   prose sits at Indent==0 *inside* the block, and orphaned/
	//   duplicate sub-field headers follow. Some prose lines contain a
	//   stray colon and are mis-parsed with a spurious Key, so they are
	//   indistinguishable from a real field by syntax alone. Once such
	//   dedented prose (Indent==0, non-empty, Key=="") has been seen,
	//   the only reliable "real next field" signal is a canonical
	//   schema key, so from then on the scan consumes everything until
	//   a CanonicalTopLevelKeys key (or `---`).
	//
	// Net effect: clean items are untouched-safe without any whitelist;
	// only genuinely corrupt blocks invoke the canonical-key terminator.
	endIdx := startIdx + 1
	sawDedentedProse := false
	for endIdx < len(d.Lines) {
		l := d.Lines[endIdx]
		if strings.TrimSpace(l.Raw) == "---" {
			break
		}
		if l.Indent == 0 && !l.IsEmpty {
			if l.Key == "" {
				// Dedented prose — the I-487 corruption signature.
				sawDedentedProse = true
				endIdx++
				continue
			}
			// Indent==0 keyed line.
			if !sawDedentedProse {
				break // clean block: this is the real next field.
			}
			if CanonicalTopLevelKeys[l.Key] {
				break // corrupt block: reached the real next field.
			}
			// corrupt block, keyed garbage line — keep consuming.
		}
		endIdx++
	}

	tail := append([]Line{}, d.Lines[endIdx:]...)
	d.Lines = append(d.Lines[:startIdx], append(newLines, tail...)...)
}

// buildSBARLines renders an SBAR struct as the line slice of a
// `sbar:` block. Format mirrors cmd/migrate-sbar/renderSBARBlock so
// freshly-edited blocks are byte-identical to migrated blocks.
//
// Empty sub-fields are emitted as `  key: |-` with no body lines —
// `key: |-` followed immediately by the next header is valid YAML
// for an empty block scalar, and avoids a "    " trailing-whitespace
// line that some editors silently strip.
//
// A trailing blank Line is appended so the next top-level field has
// a visual separator after the block. Without it, every SBAR edit
// would produce a spurious one-line whitespace diff.
func buildSBARLines(s SBAR) []Line {
	out := []Line{{Raw: "sbar:", Key: "sbar"}}
	for _, sec := range []struct {
		key, val string
	}{
		{"situation", s.Situation},
		{"background", s.Background},
		{"assessment", s.Assessment},
		{"recommendation", s.Recommendation},
	} {
		out = append(out, Line{
			Raw:      "  " + sec.key + ": |-",
			Key:      sec.key,
			Indent:   2,
			BlockKey: "sbar",
		})
		if sec.val == "" {
			continue
		}
		for _, ln := range strings.Split(strings.TrimRight(sec.val, "\n"), "\n") {
			out = append(out, Line{
				Raw:      "    " + ln,
				IsBlock:  true,
				BlockKey: sec.key,
				Indent:   4,
			})
		}
	}
	out = append(out, Line{Raw: "", IsEmpty: true})
	return out
}

// SBARIsScalarCorrupted reports whether the document's `sbar` field has
// been flattened from its canonical 4-section mapping into a YAML string
// scalar. This is the I-670 corruption signature: a pre-fix
// `st update <id> sbar [--stdin|<value>]` routed through SetField, which
// renders multi-line input as `sbar: |-` (block scalar) and single-line
// input as `sbar: <value>` (inline scalar). A structurally valid sbar —
// whether freshly scaffolded by `st create` or written by SetSBARBlock —
// always has a bare `sbar:` mapping header with the four sub-keys nested
// beneath it. Detection keys off the header form alone, so prose body
// lines that happen to contain a `key:` pattern cannot cause a false
// negative. Returns false when there is no `sbar:` line at all (an
// absent block is created fresh by the composite writer, not "corrupt").
func (d *ParsedDocument) SBARIsScalarCorrupted() bool {
	for _, l := range d.Lines {
		if l.Key == "sbar" && l.Indent == 0 {
			// Canonical mapping header is exactly `sbar:` (optionally
			// followed by a comment). Anything else after the colon —
			// a block-scalar indicator (`|-`, `|`, `>`, `>-`) or an
			// inline value — means the mapping was flattened to a string.
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l.Raw), "sbar:"))
			if rest == "" || strings.HasPrefix(rest, "#") {
				return false
			}
			return true
		}
	}
	return false
}

// ReplaceList replaces an entire list field (key + all continuation lines)
// with the new lines. Each line in values should be "- item text".
func (d *ParsedDocument) ReplaceList(key string, values []string) {
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
		if idx := d.BodySeparatorIndex(); idx >= 0 {
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
			childIndent := line.Indent
			// Drop the child's existing block-scalar continuation
			// lines before replacing. This mirrors SetField's
			// continuation handling — its absence here is the I-593
			// bug: a single-line overwrite of a `key: |-` block left
			// every old body line stranded beneath the new value
			// (invalid YAML; compounds on each edit). A continuation
			// is a following line the parser flagged IsBlock for this
			// child, or — when the header was already collapsed to a
			// scalar so the parser never set IsBlock — any non-empty
			// line indented deeper than the child key. Stop at the
			// first sibling/shallower line or blank separator so we
			// never consume a following nested field.
			end := i + 1
			for end < len(d.Lines) {
				l := d.Lines[end]
				if (l.IsBlock && l.BlockKey == child) ||
					(!l.IsEmpty && l.Indent > childIndent) {
					end++
					continue
				}
				break
			}
			newLines := buildNestedScalarOrBlock(parent, child, value, childIndent, line.Comment)
			tail := append([]Line{}, d.Lines[end:]...)
			d.Lines = append(d.Lines[:i], append(newLines, tail...)...)
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

// RemoveNestedField deletes a nested field by dotted-path (e.g.
// "assigned_to_meta.parent_id"). If the parent block becomes empty after
// removal, the parent is removed too so callers don't accumulate empty
// section headers. Returns true if a line was removed.
func (d *ParsedDocument) RemoveNestedField(path string) bool {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) != 2 {
		return false
	}
	parent, child := parts[0], parts[1]

	parentIdx := -1
	for i, line := range d.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
			break
		}
	}
	if parentIdx < 0 {
		return false
	}

	removed := false
	end := parentIdx + 1
	for end < len(d.Lines) {
		line := d.Lines[end]
		if line.Indent == 0 && !line.IsEmpty {
			break
		}
		if line.Key == child && line.Indent > 0 && line.BlockKey == parent {
			d.Lines = append(d.Lines[:end], d.Lines[end+1:]...)
			removed = true
			continue
		}
		end++
	}

	if !removed {
		return false
	}

	// If the parent block has no remaining nested children, drop the
	// parent header line as well.
	hasChildren := false
	for i := parentIdx + 1; i < len(d.Lines); i++ {
		l := d.Lines[i]
		if l.Indent == 0 && !l.IsEmpty {
			break
		}
		if l.Indent > 0 && l.BlockKey == parent {
			hasChildren = true
			break
		}
	}
	if !hasChildren {
		d.Lines = append(d.Lines[:parentIdx], d.Lines[parentIdx+1:]...)
	}
	return true
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

// AppendToNestedList appends a value to a list field nested under a parent
// key (e.g. work_tracking.commits). If the parent or list doesn't exist,
// they are created. New parent blocks are spliced before the body
// separator (---) so they land in the frontmatter, not the markdown body.
// If the existing list contains an empty marker (`- []` or `- [[]]`), the
// marker is replaced with the new value rather than appended after it.
func (d *ParsedDocument) AppendToNestedList(parent, key, value string) {
	parentIdx := -1
	keyIdx := -1
	for i, line := range d.Lines {
		if line.Key == parent && line.Indent == 0 {
			parentIdx = i
		}
		if parentIdx >= 0 && line.Key == key && line.Indent > 0 {
			keyIdx = i
		}
	}

	parentLine := Line{Raw: parent + ":", Key: parent}
	keyLine := Line{Raw: "  " + key + ":", Key: key, Indent: 2, BlockKey: parent}
	itemLine := Line{Raw: "  - " + value, IsList: true, Indent: 2, BlockKey: parent}

	if parentIdx < 0 {
		newLines := []Line{parentLine, keyLine, itemLine}
		if idx := d.BodySeparatorIndex(); idx >= 0 {
			tail := append([]Line{}, d.Lines[idx:]...)
			d.Lines = append(d.Lines[:idx], append(newLines, tail...)...)
		} else {
			d.Lines = append(d.Lines, newLines...)
		}
		return
	}

	if keyIdx < 0 {
		// Parent exists but key doesn't — insert key + first list item
		// at the end of the parent's nested block.
		insertAt := parentIdx + 1
		for insertAt < len(d.Lines) {
			line := d.Lines[insertAt]
			if line.Indent > 0 || line.IsEmpty {
				insertAt++
				continue
			}
			break
		}
		newLines := []Line{keyLine, itemLine}
		tail := append([]Line{}, d.Lines[insertAt:]...)
		d.Lines = append(d.Lines[:insertAt], append(newLines, tail...)...)
		return
	}

	// Key exists — find end of its list, replacing any empty marker.
	insertAt := keyIdx + 1
	for insertAt < len(d.Lines) {
		line := d.Lines[insertAt]
		if line.IsList && line.Indent >= 2 {
			compact := strings.ReplaceAll(strings.ReplaceAll(line.Raw, " ", ""), "\t", "")
			if compact == "-[]" || compact == "-[[]]" {
				d.Lines[insertAt] = itemLine
				return
			}
			insertAt++
			continue
		}
		// Tolerate "- []" lines that the parser left without IsList
		if line.Indent >= 2 && !line.IsEmpty {
			compact := strings.ReplaceAll(strings.ReplaceAll(line.Raw, " ", ""), "\t", "")
			if compact == "-[]" || compact == "-[[]]" {
				d.Lines[insertAt] = itemLine
				return
			}
		}
		break
	}

	tail := append([]Line{}, d.Lines[insertAt:]...)
	d.Lines = append(d.Lines[:insertAt], append([]Line{itemLine}, tail...)...)
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
		insertIdx := d.BodySeparatorIndex()
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

// BodySeparatorIndex returns the index of the first "---" line (body separator),
// or -1 if no separator exists. Used to insert new fields in the frontmatter
// section rather than after the markdown body.
func (d *ParsedDocument) BodySeparatorIndex() int {
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
