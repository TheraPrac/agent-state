// Package migrate normalizes agent-state files to a canonical schema.
// It handles: legacy testing_evidence conversion, field drops (blocks,
// promotion_required, etc.), field additions (delivery, doc_changes),
// and canonical field ordering.
package migrate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// priorityMap converts legacy string priority values to numeric 0-4.
var priorityMap = map[string]int{
	"alpha-critical":      0,
	"blocking":            0,
	"production-critical": 1,
	"high":                1,
	"normal":              2,
	"medium":              2,
	"med":                 2,
	"important":           2,
	"post-alpha":          3,
	"post-mvp":            3,
	"nice-to-have":        4,
	"low":                 4,
}

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

	// String priority needs conversion to numeric
	if s, ok := sections["priority"]; ok && len(s.lines) > 0 {
		val := extractScalarValue(s.lines[0])
		if val != "" && val != "null" && val != "~" {
			if _, err := strconv.Atoi(val); err != nil {
				if _, mapped := priorityMap[val]; mapped {
					changes = append(changes, Change{
						Type:   "convert_priority",
						Detail: fmt.Sprintf("convert priority %q to numeric", val),
					})
				} else {
					changes = append(changes, Change{
						Type:   "convert_priority",
						Detail: fmt.Sprintf("unknown priority %q (will preserve as-is)", val),
					})
				}
			}
		}
	}

	// Category value should be added to tags
	if s, ok := sections["category"]; ok && len(s.lines) > 0 {
		val := extractScalarValue(s.lines[0])
		if val != "" && val != "null" && val != "~" {
			changes = append(changes, Change{
				Type:   "category_to_tags",
				Detail: fmt.Sprintf("add category %q to tags", val),
			})
		}
	}

	// Legacy time/cost tracking keys that should be renamed or dropped
	for _, blockName := range []string{"time_tracking"} {
		s, ok := sections[blockName]
		if !ok {
			continue
		}
		sawRename := false
		for _, line := range s.lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx <= 0 {
				continue
			}
			key := trimmed[:colonIdx]
			if _, renamed := legacyMetricRenames[key]; renamed {
				if !sawRename {
					changes = append(changes, Change{
						Type:   "rename_metric_field",
						Detail: fmt.Sprintf("rename legacy %s keys to new schema", blockName),
					})
					sawRename = true
				}
			}
			if legacyMetricDrops[key] {
				changes = append(changes, Change{
					Type:   "drop_field",
					Detail: fmt.Sprintf("drop %s.%s (superseded)", blockName, key),
				})
			}
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
// Fields not in this list are emitted as "extras" after the canonical fields.
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
	"scope_class",
	"approach_decision",
	"assigned_to", "last_touched_by",
	"tags", "epic", "sprint",
	"_blank_taxonomy",
	"sessions",
	"parallel_group",
	"_blank_9",
	"depends_on",
	"_blank_10",
	"related_issues",
	"_blank_11",
	"linked_plans",
	"plan_written_at", "plan_failed_at", "plan_failure_reason",
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

	// Rewrite legacy time/cost tracking field names in place before emission.
	// Keeps existing items compatible with the SessionLog schema.
	rewriteLegacyMetrics(sections)

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
	item         *model.Item
	cfg          *config.Config
	sections     map[string]*rawSection
	lines        []string
	emitted      map[string]bool
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
	case "priority":
		b.emitPriority()
	case "scope_class":
		// I-776: scope_class is a boolean-ish opt-in field — emit only when set.
		// Unlike epic/sprint where a `null` marker may carry meaning, a stale
		// `scope_class: null` line on an item whose class was cleared is just
		// noise and would re-introduce the bypass anti-pattern (an inert
		// declaration that operators might think still activates the carve-out).
		if b.item.ScopeClass != "" {
			b.add("scope_class: " + b.item.ScopeClass)
		}
	case "tags":
		b.emitTags()
	case "epic":
		b.emitScalarIfPresent("epic", b.item.Epic)
	case "sprint":
		b.emitScalarIfPresent("sprint", b.item.Sprint)
	case "sessions":
		b.emitListIfPresent("sessions", b.item.Sessions)
	default:
		// Use raw section for fields not explicitly handled
		b.emitRaw(field)
	}

	b.emitted[field] = true
}

func (b *builder) emitPriority() {
	s, ok := b.sections["priority"]
	if !ok || len(s.lines) == 0 {
		return
	}
	rawVal := extractScalarValue(s.lines[0])
	if rawVal == "" || rawVal == "null" || rawVal == "~" {
		b.add("priority: null")
		return
	}
	// Already numeric?
	if _, err := strconv.Atoi(rawVal); err == nil {
		b.add("priority: " + rawVal)
		return
	}
	// Map string to int
	if n, ok := priorityMap[rawVal]; ok {
		b.add(fmt.Sprintf("priority: %d", n))
		return
	}
	// Unknown string — preserve as-is
	b.add("priority: " + rawVal)
}

func (b *builder) emitTags() {
	tags := b.item.Tags

	// Category-to-tags: if category has a value, ensure it's in tags
	if s, ok := b.sections["category"]; ok && len(s.lines) > 0 {
		catVal := extractScalarValue(s.lines[0])
		if catVal != "" && catVal != "null" && catVal != "~" {
			if !stringSliceContains(tags, catVal) {
				tags = append([]string{catVal}, tags...)
			}
		}
	}

	if len(tags) == 0 {
		// Check if raw section has tags content
		if s, inSource := b.sections["tags"]; inSource && !isEmptyListSection(s) {
			b.emitRaw("tags")
			return
		}
		return // No tags — skip entirely
	}
	b.emitList("tags", tags)
}

func (b *builder) emitScalarIfPresent(key, val string) {
	_, inSource := b.sections[key]
	if !inSource && val == "" {
		return // Not in source and no data — skip
	}
	if val == "" {
		b.add(key + ": null")
	} else {
		b.add(key + ": " + val)
	}
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

	// required_suites — I-776: emit the item's class-scoped set so reconcile
	// doesn't write empty api/web placeholders onto a workspace-config item.
	// Unknown class falls back to no emission (the gate failure will surface
	// the unknown-class error; nothing useful to canonicalize here).
	suiteNames := classScopedRequiredSuiteNames(b.cfg, b.item)
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

	// scope_suites — I-776: class items have a closed required-set
	// definition and don't observe scope-suite policy at the gate, so
	// reconcile must not bake default scope-suite placeholders into
	// their files. Without this guard, a workspace-config item would
	// always carry stale `api_integration: null`, `web_e2e: null` rows
	// that could later be flipped to `required` and re-create the
	// diverged-evidence shape the class carve-out retires.
	var scopeNames []string
	if b.item.ScopeClass == "" {
		scopeNames = b.cfg.Testing.ScopeSuiteNames()
	}
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

// legacyMetricRenames maps old time_tracking field names to their new names.
// Applied in-place during Canonical emission so items re-save under the new
// SessionLog schema on first touch. All renames target the time_tracking
// block; work_tracking's canonical emitter strips unknown nested keys, so
// any legacy work_tracking.ai_sessions data is lost on migration (this was
// already the pre-existing behavior).
var legacyMetricRenames = map[string]string{
	"run_wall_seconds":    "process_time_seconds",
	"ai_duration_seconds": "ai_time_seconds",
	"run_count":           "turn_count",
	"input_tokens":        "reg_input_tokens",
	"output_tokens":       "reg_output_tokens",
}

// legacyMetricDrops are fields that were written by the old st run path and
// are no longer meaningful under the new schema. `total_tokens` was an
// ambiguous sum of regular + cache tokens at different pricing tiers — the
// new schema tracks them separately.
var legacyMetricDrops = map[string]bool{
	"total_tokens": true,
}

// rewriteLegacyMetrics mutates time_tracking and work_tracking sections in
// place, renaming legacy keys and dropping superseded ones. Idempotent:
// running twice on an already-migrated item is a no-op.
func rewriteLegacyMetrics(sections map[string]*rawSection) {
	for _, name := range []string{"time_tracking"} {
		s, ok := sections[name]
		if !ok {
			continue
		}
		s.lines = rewriteLegacyMetricLines(s.lines)
	}
}

// rewriteLegacyMetricLines rewrites legacy keys in a block's raw lines.
// Expects lines in the form "  key: value" (2-space indent) or "  key:"
// (list header) or "  - ..." (list item).
func rewriteLegacyMetricLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]
		// Skip comments and empties
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		// Extract key
		key := ""
		if colonIdx := strings.Index(trimmed, ":"); colonIdx > 0 {
			key = trimmed[:colonIdx]
		}
		if legacyMetricDrops[key] {
			continue // drop the line entirely
		}
		if newKey, renamed := legacyMetricRenames[key]; renamed {
			// Rebuild line with new key, preserving rest (value + whitespace)
			rest := trimmed[len(key):]
			out = append(out, indent+newKey+rest)
			continue
		}
		out = append(out, line)
	}
	return out
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

func stringSliceContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
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

// classScopedRequiredSuiteNames returns the required-suite names that apply
// to a given item — its scope_class's set when declared, else the default.
// Sorted; empty when the class is unknown (the gate surfaces that as a
// targeted failure; canonical-emit just skips the block).
func classScopedRequiredSuiteNames(cfg *config.Config, item *model.Item) []string {
	if cfg == nil || cfg.Testing == nil {
		return nil
	}
	required, ok := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
