// Package migrate normalizes agent-state files to a canonical schema.
// It handles: legacy testing_evidence conversion, field drops (blocks,
// promotion_required, etc.), field additions (delivery, doc_changes),
// and canonical field ordering.
package migrate

import (
	"fmt"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// rawSection groups the raw lines belonging to a single top-level field.
type rawSection struct {
	key   string
	lines []string
}

// Change describes a single migration transformation.
type Change struct {
	Type   string // testing_format, drop_field, add_field, reorder
	Detail string
}

// FileResult holds the migration plan for one file.
type FileResult struct {
	Path    string
	Changes []Change
	Before  string
	After   string
}

// HasChanges returns true if migration would modify this file.
func (r *FileResult) HasChanges() bool {
	return strings.TrimRight(r.Before, "\n") != strings.TrimRight(r.After, "\n")
}

// PlanFile computes migration changes without writing.
func PlanFile(item *model.Item, path string, cfg *config.Config) *FileResult {
	result := &FileResult{Path: path}
	if item.Doc == nil {
		return result
	}
	result.Before = item.Doc.String()
	result.Changes = detectChanges(item, cfg)
	result.After = Canonical(item, cfg)
	return result
}

// detectChanges identifies what transformations are needed.
func detectChanges(item *model.Item, cfg *config.Config) []Change {
	var changes []Change
	sections := extractSections(item.Doc)

	// Check testing format
	if s, ok := sections["testing_evidence"]; ok && cfg.Testing != nil {
		for _, line := range s.lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "runs:" || strings.HasPrefix(trimmed, "required:") {
				changes = append(changes, Change{
					Type:   "testing_format",
					Detail: "convert legacy runs: to required_suites/scope_suites",
				})
				break
			}
		}
	}

	// Dropped fields
	if _, ok := sections["blocks"]; ok {
		changes = append(changes, Change{Type: "drop_field", Detail: "remove blocks: (computed)"})
	}
	if _, ok := sections["promotion_required"]; ok {
		changes = append(changes, Change{Type: "drop_field", Detail: "remove promotion_required: (unused)"})
	}
	if s, ok := sections["parallel_group"]; ok {
		if isNullSection(s) {
			changes = append(changes, Change{Type: "drop_field", Detail: "remove parallel_group: null"})
		}
	}
	if s, ok := sections["linked_plans"]; ok {
		if isEmptyListSection(s) {
			changes = append(changes, Change{Type: "drop_field", Detail: "remove linked_plans: (empty)"})
		}
	}

	// Missing modern fields
	if _, ok := sections["delivery"]; !ok && cfg.Delivery != nil {
		changes = append(changes, Change{Type: "add_field", Detail: "add delivery: section"})
	}
	if _, ok := sections["doc_changes"]; !ok {
		changes = append(changes, Change{Type: "add_field", Detail: "add doc_changes: field"})
	}

	// Check work_tracking missing pr field
	if s, ok := sections["work_tracking"]; ok {
		hasPR := false
		for _, line := range s.lines {
			if strings.Contains(line, "pr:") {
				hasPR = true
				break
			}
		}
		if !hasPR {
			changes = append(changes, Change{Type: "add_field", Detail: "add work_tracking.pr: []"})
		}
	}

	return changes
}

// --- Canonical builder ---

// droppedFields are always removed during migration.
var droppedFields = map[string]bool{
	"blocks":             true,
	"promotion_required": true,
}

// conditionalDrops are removed only when they have null/empty values.
var conditionalDrops = map[string]bool{
	"parallel_group": true,
	"linked_plans":   true,
}

// canonicalOrder defines the field emission order.
// Fields not in this list are emitted as "extras" before next_actions.
var canonicalOrder = []string{
	"id", "type", "status", "created", "last_touched",
	"_blank_1",
	"completed",
	"_blank_2",
	"resolution",
	"_blank_3",
	"work_tracking",
	"_blank_4",
	"delivery",
	"_blank_5",
	"testing_evidence",
	"_blank_6",
	"title",
	"_blank_7",
	"summary",
	"_blank_8",
	"context",
	"priority", "severity", "category", "repo", "source",
	"approach_decision",
	"assigned_to", "last_touched_by",
	"parallel_group",
	"_blank_9",
	"depends_on",
	"_blank_10",
	"related_issues",
	"_blank_11",
	"linked_plans",
	"_blank_12",
	"doc_changes",
	"_blank_13",
	"scope", "scope_in", "scope_out",
	"_blank_14",
	"canonical_anchors",
	"invariants",
	"_blank_15",
	"acceptance_criteria",
	"_blank_16",
	"next_actions",
	"_blank_17",
	"time_tracking",
	"manifest",
}

// Canonical returns the file content in canonical format.
func Canonical(item *model.Item, cfg *config.Config) string {
	sections := extractSections(item.Doc)
	body := extractBody(item.Doc)

	b := &builder{
		item:     item,
		cfg:      cfg,
		sections: sections,
		emitted:  make(map[string]bool),
	}

	// Emit fields in canonical order
	for _, field := range canonicalOrder {
		if strings.HasPrefix(field, "_blank_") {
			b.blankIfContent()
			continue
		}
		b.emitField(field)
	}

	// Emit any extra sections not in canonical order
	b.emitExtras()

	// Append body
	if len(body) > 0 {
		b.blankIfContent()
		for _, line := range body {
			b.lines = append(b.lines, line)
		}
	}

	result := strings.Join(b.lines, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// builder accumulates canonical output lines.
type builder struct {
	item     *model.Item
	cfg      *config.Config
	sections map[string]*rawSection
	lines    []string
	emitted  map[string]bool
	lastWasBlank bool
}

func (b *builder) add(line string) {
	b.lines = append(b.lines, line)
	b.lastWasBlank = line == ""
}

func (b *builder) blank() {
	if !b.lastWasBlank {
		b.add("")
	}
}

// blankIfContent adds a blank line only if we've emitted content since the last blank.
func (b *builder) blankIfContent() {
	if len(b.lines) > 0 && !b.lastWasBlank {
		b.add("")
	}
}

func (b *builder) emitField(field string) {
	// Skip dropped fields
	if droppedFields[field] {
		b.emitted[field] = true
		return
	}

	// Conditional drops
	if conditionalDrops[field] {
		if s, ok := b.sections[field]; ok {
			if field == "parallel_group" && isNullSection(s) {
				b.emitted[field] = true
				return
			}
			if field == "linked_plans" && isEmptyListSection(s) {
				b.emitted[field] = true
				return
			}
		} else {
			// Not present — skip silently
			b.emitted[field] = true
			return
		}
	}

	// Special handlers for reconstructed sections
	switch field {
	case "id":
		b.add("id: " + b.item.ID)
	case "type":
		b.add("type: " + b.item.Type)
	case "status":
		b.add("status: " + b.item.Status)
	case "created":
		b.add("created: " + formatTime(b.item.Created))
	case "last_touched":
		b.add("last_touched: " + formatTime(b.item.LastTouched))
	case "completed":
		if b.item.Completed != nil {
			b.add("completed: " + formatTime(*b.item.Completed))
		} else {
			b.add("completed: null")
		}
	case "title":
		b.emitTitle()
	case "resolution":
		b.emitList("resolution", b.item.Resolution)
	case "work_tracking":
		b.emitWorkTracking()
	case "delivery":
		b.emitDelivery()
	case "testing_evidence":
		b.emitTestingEvidence()
	case "depends_on":
		b.emitList("depends_on", b.item.DependsOn)
	case "related_issues":
		b.emitListIfPresent("related_issues", b.item.RelatedIssues)
	case "doc_changes":
		b.emitDocChanges()
	case "acceptance_criteria":
		b.emitListIfPresent("acceptance_criteria", b.item.AcceptanceCriteria)
	case "next_actions":
		b.emitListIfPresent("next_actions", b.item.NextActions)
	case "invariants":
		b.emitListIfPresent("invariants", b.item.Invariants)
	case "tags":
		b.emitListIfPresent("tags", b.item.Tags)
	default:
		// Use raw section for fields not explicitly handled
		b.emitRaw(field)
	}

	b.emitted[field] = true
}

func (b *builder) emitTitle() {
	title := b.item.Title
	if title == "" {
		if s, ok := b.sections["title"]; ok {
			title = extractScalarValue(s.lines[0])
		}
	}
	if needsQuoting(title) {
		b.add(fmt.Sprintf(`title: "%s"`, title))
	} else {
		b.add("title: " + title)
	}
}

func (b *builder) emitList(key string, items []string) {
	if len(items) == 0 {
		b.add(key + ":")
		b.add("- []")
	} else {
		b.add(key + ":")
		for _, item := range items {
			if needsQuoting(item) {
				b.add(fmt.Sprintf(`- "%s"`, item))
			} else {
				b.add("- " + item)
			}
		}
	}
}

func (b *builder) emitListIfPresent(key string, items []string) {
	s, inSource := b.sections[key]
	if !inSource && len(items) == 0 {
		return // Not in source and no data — skip
	}
	// If typed data is empty but raw section has real content (e.g., list-of-maps),
	// preserve the raw section to avoid data loss.
	if len(items) == 0 && inSource && !isEmptyListSection(s) {
		b.emitRaw(key)
		return
	}
	b.emitList(key, items)
}

func (b *builder) emitDocChanges() {
	if len(b.item.DocChanges) > 0 {
		b.emitList("doc_changes", b.item.DocChanges)
	} else {
		b.add("doc_changes:")
		b.add("- []")
	}
}

func (b *builder) emitWorkTracking() {
	wt := b.item.WorkTracking
	b.add("work_tracking:")
	b.add("  branch: " + nullOr(mapStr(wt, "branch")))
	b.add("  commits: " + emptyListOr(mapStr(wt, "commits")))
	b.add("  pr: " + emptyListOr(mapStr(wt, "pr")))
}

func (b *builder) emitDelivery() {
	if b.cfg.Delivery == nil {
		// No delivery config — emit raw section if present
		b.emitRaw("delivery")
		return
	}

	del := b.item.Delivery
	b.add("delivery:")
	b.add("  stage: " + nullOr(mapStr(del, "stage")))
	b.add("  deployed_date: " + nullOr(mapStr(del, "deployed_date")))
	b.add("  uat_approved_by: " + nullOr(mapStr(del, "uat_approved_by")))
	b.add("  uat_approved_date: " + nullOr(mapStr(del, "uat_approved_date")))
}

func (b *builder) emitTestingEvidence() {
	if b.cfg.Testing == nil {
		// No testing config — emit raw section if present
		b.emitRaw("testing_evidence")
		return
	}

	te := b.item.TestingEvidence
	b.add("testing_evidence:")

	// tests_written
	if tw, ok := te["tests_written"]; ok {
		if list, ok := tw.([]string); ok && len(list) > 0 {
			b.add("  tests_written:")
			for _, t := range list {
				b.add("  - " + t)
			}
		} else {
			b.add("  tests_written:")
			b.add("  - []")
		}
	} else {
		b.add("  tests_written:")
		b.add("  - []")
	}

	b.add("")

	// required_suites
	suiteNames := b.cfg.Testing.RequiredSuiteNames()
	if len(suiteNames) > 0 {
		b.add("  required_suites:")
		maxLen := maxKeyLen(suiteNames)
		for _, name := range suiteNames {
			val := suiteValue(te, name)
			padding := strings.Repeat(" ", maxLen-len(name))
			b.add(fmt.Sprintf("    %s:%s %s", name, padding, val))
		}
	}

	b.add("")

	// scope_suites
	scopeNames := b.cfg.Testing.ScopeSuiteNames()
	if len(scopeNames) > 0 {
		b.add("  scope_suites:")
		maxLen := maxKeyLen(scopeNames)
		for _, name := range scopeNames {
			val := suiteValue(te, name)
			padding := strings.Repeat(" ", maxLen-len(name))
			b.add(fmt.Sprintf("    %s:%s %s", name, padding, val))
		}
	}

	b.add("")

	// notes
	notesVal := "null"
	if v := mapStr(te, "notes"); v != "" {
		notesVal = v
	}
	b.add("  notes: " + notesVal)
}

func (b *builder) emitRaw(field string) {
	s, ok := b.sections[field]
	if !ok {
		return
	}
	for _, line := range s.lines {
		b.add(line)
	}
}

func (b *builder) emitExtras() {
	// Collect sections not yet emitted, in their original order
	seen := make(map[string]bool)
	for _, field := range canonicalOrder {
		if !strings.HasPrefix(field, "_blank_") {
			seen[field] = true
		}
	}
	// Also mark always-dropped fields
	for k := range droppedFields {
		seen[k] = true
	}

	var extras []string
	for _, s := range extractSectionsOrdered(b.item.Doc) {
		if !seen[s.key] && !b.emitted[s.key] {
			extras = append(extras, s.key)
		}
	}

	for _, key := range extras {
		b.blankIfContent()
		b.emitRaw(key)
		b.emitted[key] = true
	}
}

// --- Section extraction ---

// extractSections groups ParsedDocument lines by top-level key.
func extractSections(doc *model.ParsedDocument) map[string]*rawSection {
	sections := make(map[string]*rawSection)
	if doc == nil {
		return sections
	}

	var current *rawSection

	for _, line := range doc.Lines {
		trimmed := strings.TrimSpace(line.Raw)

		// Body separator — stop
		if trimmed == "---" {
			break
		}

		// New top-level key
		if line.Indent == 0 && line.Key != "" {
			if current != nil {
				current.lines = trimTrailingBlanks(current.lines)
			}
			current = &rawSection{key: line.Key}
			sections[line.Key] = current
			current.lines = append(current.lines, line.Raw)
			continue
		}

		// Continuation of current section
		if current != nil {
			current.lines = append(current.lines, line.Raw)
		}
	}

	if current != nil {
		current.lines = trimTrailingBlanks(current.lines)
	}

	return sections
}

// extractSectionsOrdered returns sections in their original file order.
func extractSectionsOrdered(doc *model.ParsedDocument) []*rawSection {
	if doc == nil {
		return nil
	}

	var result []*rawSection
	seen := make(map[string]bool)
	var current *rawSection

	for _, line := range doc.Lines {
		trimmed := strings.TrimSpace(line.Raw)
		if trimmed == "---" {
			break
		}

		if line.Indent == 0 && line.Key != "" {
			if current != nil {
				current.lines = trimTrailingBlanks(current.lines)
			}
			current = &rawSection{key: line.Key}
			if !seen[line.Key] {
				result = append(result, current)
				seen[line.Key] = true
			}
			current.lines = append(current.lines, line.Raw)
			continue
		}

		if current != nil {
			current.lines = append(current.lines, line.Raw)
		}
	}

	if current != nil {
		current.lines = trimTrailingBlanks(current.lines)
	}

	return result
}

// extractBody returns lines from --- onward.
func extractBody(doc *model.ParsedDocument) []string {
	if doc == nil {
		return nil
	}
	for i, line := range doc.Lines {
		if strings.TrimSpace(line.Raw) == "---" {
			var body []string
			for j := i; j < len(doc.Lines); j++ {
				body = append(body, doc.Lines[j].Raw)
			}
			return body
		}
	}
	return nil
}

// --- Helpers ---

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "null"
	}
	return t.Format(time.RFC3339)
}

func extractScalarValue(line string) string {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return ""
	}
	val := strings.TrimSpace(line[idx+1:])
	// Strip inline comment
	if ci := strings.Index(val, " #"); ci >= 0 {
		val = strings.TrimSpace(val[:ci])
	}
	return val
}

func isNullSection(s *rawSection) bool {
	if len(s.lines) != 1 {
		return false
	}
	val := extractScalarValue(s.lines[0])
	return val == "null" || val == "~" || val == ""
}

func isEmptyListSection(s *rawSection) bool {
	if len(s.lines) < 1 {
		return true
	}
	// Check for "key:\n- []" pattern
	for _, line := range s.lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "- []" || trimmed == "- [[]]" || trimmed == "" {
			continue
		}
		return false // has actual content
	}
	return true
}

func trimTrailingBlanks(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func needsQuoting(s string) bool {
	return strings.ContainsAny(s, ":`#{}[]") || strings.HasPrefix(s, "'") || strings.HasPrefix(s, "\"")
}

func nullOr(val string) string {
	if val == "" || val == "~" {
		return "null"
	}
	return val
}

func emptyListOr(val string) string {
	if val == "" || val == "null" || val == "~" {
		return "[]"
	}
	return val
}

func mapStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func suiteValue(te map[string]interface{}, name string) string {
	// Check flat key (parser stores nested suite values as flat keys)
	if v := mapStr(te, name); v != "" {
		// Value needs quoting if it contains ':'
		if strings.Contains(v, ":") {
			return fmt.Sprintf(`"%s"`, v)
		}
		return v
	}
	return "null"
}

func maxKeyLen(keys []string) int {
	max := 0
	for _, k := range keys {
		if len(k) > max {
			max = len(k)
		}
	}
	return max
}
