// migrate-priority is a one-shot data backfill that rewrites every
// agent-state item file to use the unified `priority:` (int 0-4) field
// instead of the legacy `severity:` (critical/high/medium/low/...)
// field. Per I-406's migration table:
//
//	blocking | critical | p0       → priority: 0
//	high     | important            → priority: 1
//	medium   | normal                → priority: 2
//	tech-debt                       → priority: 3 + tag tech-debt
//	low      | minor                → priority: 4
//
// Behaviors:
//   - When an item has BOTH severity and priority, priority wins —
//     severity is just dropped (no value override).
//   - When severity is the only signal, priority is added per the table
//     (or defaults to 2 if the value is unrecognized; flagged in report).
//   - tech-debt items pick up a `tech-debt` tag if not already present.
//   - Operates on agent-state/issues/**, agent-state/tasks/**, and
//     agent-state/archive/**.
//   - --dry-run mode prints the planned changes and the summary without
//     touching any files.
//
// Output: a per-file changelog entry plus a final report. The report is
// written to stdout AND to ./migrate-priority-report.md so it can be
// committed under I-406 as evidence per the AC.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// severityToPriority maps known severity values to the unified int.
// Values not in this table fall back to defaultPriority and are flagged
// in the report so the operator can review.
var severityToPriority = map[string]int{
	"blocking":  0,
	"critical":  0,
	"p0":        0,
	"high":      1,
	"important": 1,
	"medium":    2,
	"normal":    2,
	"tech-debt": 3,
	"low":       4,
	"minor":     4,
}

// severityToTag maps severities that imply a categorical tag. tech-debt
// is a category, not a priority — it gets priority 3 (deferred backlog)
// AND a tech-debt tag added if absent.
var severityToTag = map[string]string{
	"tech-debt": "tech-debt",
}

const defaultPriority = 2 // medium — applied when severity is unknown

// fileResult captures what the tool would do (or did) with one file.
type fileResult struct {
	Path           string
	Severity       string
	OldPriority    string // "" if absent
	NewPriority    int
	TagAdded       string // "" if no tag implied
	Action         string // "skip", "drop_severity_keep_priority", "add_priority", "flag_unknown"
	Note           string // extra detail for the report
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	root := flag.String("root", ".", "agent-state root directory (e.g. theraprac-workspace/agent-state)")
	report := flag.String("report", "migrate-priority-report.md", "where to write the migration report")
	flag.Parse()

	dirs := []string{
		filepath.Join(*root, "issues"),
		filepath.Join(*root, "tasks"),
		filepath.Join(*root, "archive"),
	}

	var results []fileResult
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "skip missing dir: %s\n", d)
			continue
		}
		err := filepath.Walk(d, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			r, err := processFile(path, *dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
				return nil
			}
			if r != nil {
				results = append(results, *r)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk %s: %v\n", d, err)
		}
	}

	writeReport(*report, results, *dryRun)
	printSummary(results, *dryRun)
}

// processFile reads one item file, decides what to do, and (unless
// dryRun) writes the result back. Returns nil when the file has no
// severity field at all (no-op, not interesting for the report).
func processFile(path string, dryRun bool) (*fileResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")

	severity, sevLine := findField(lines, "severity")
	priority, _ := findField(lines, "priority")
	if severity == "" {
		// Nothing to migrate. Skip silently — the file is already
		// conforming or simply doesn't carry a severity.
		return nil, nil
	}

	r := &fileResult{
		Path:        path,
		Severity:    severity,
		OldPriority: priority,
	}

	// Case 1: both present → keep priority, drop severity (no value
	// change). Priority wins per I-406.
	if priority != "" {
		r.Action = "drop_severity_keep_priority"
		r.NewPriority = parsePriorityInt(priority)
		newLines := removeLine(lines, sevLine)
		if !dryRun {
			if err := writeLines(path, newLines); err != nil {
				return nil, err
			}
			appendChangelog(path, "migrate", "severity", severity, "(removed; priority kept)")
		}
		return r, nil
	}

	// Case 2: severity-only → translate to priority via table.
	mapped, ok := severityToPriority[strings.ToLower(severity)]
	if !ok {
		// Unknown value — flag for manual review, default to medium.
		r.Action = "flag_unknown"
		r.NewPriority = defaultPriority
		r.Note = fmt.Sprintf("unrecognized severity %q — defaulted to %d (medium); please review", severity, defaultPriority)
	} else {
		r.Action = "add_priority"
		r.NewPriority = mapped
	}

	// Build new file: replace severity line with priority line; if
	// the severity also implies a tag, ensure it's present.
	priorityLine := fmt.Sprintf("priority: %d", r.NewPriority)
	newLines := replaceLine(lines, sevLine, priorityLine)

	if tag, ok := severityToTag[strings.ToLower(severity)]; ok {
		newLines = ensureTag(newLines, tag)
		r.TagAdded = tag
	}

	if !dryRun {
		if err := writeLines(path, newLines); err != nil {
			return nil, err
		}
		appendChangelog(path, "migrate", "severity_to_priority",
			fmt.Sprintf("severity=%s", severity),
			fmt.Sprintf("priority=%d%s", r.NewPriority, tagSuffix(r.TagAdded)))
	}
	return r, nil
}

func tagSuffix(tag string) string {
	if tag == "" {
		return ""
	}
	return " +tag=" + tag
}

// findField returns the value of a top-level YAML field and its line
// index. Returns ("", -1) when not found.
//
// Tolerates CRLF line endings — files written by Windows-flavoured
// tools (or VS Code without an .editorconfig override) use "\r\n", and
// strings.Split("...", "\n") leaves the \r on each line. Stripping the
// trailing CR before the indent check prevents `severity: high\r` from
// being silently treated as indented and skipped.
func findField(lines []string, field string) (string, int) {
	prefix := field + ":"
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		t := strings.TrimSpace(ln)
		// Top-level only: indentation must be zero. Tab-indented blocks
		// (e.g. inside time_tracking) are skipped.
		if ln != t {
			continue
		}
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		v := strings.TrimSpace(t[len(prefix):])
		// Strip an inline comment if present.
		if idx := strings.Index(v, " #"); idx >= 0 {
			v = strings.TrimSpace(v[:idx])
		}
		return v, i
	}
	return "", -1
}

func parsePriorityInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultPriority
	}
	if n, err := stringToInt(s); err == nil {
		return n
	}
	return defaultPriority
}

func stringToInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func removeLine(lines []string, idx int) []string {
	if idx < 0 || idx >= len(lines) {
		return lines
	}
	out := make([]string, 0, len(lines)-1)
	out = append(out, lines[:idx]...)
	out = append(out, lines[idx+1:]...)
	return out
}

func replaceLine(lines []string, idx int, newLine string) []string {
	if idx < 0 || idx >= len(lines) {
		return lines
	}
	out := make([]string, len(lines))
	copy(out, lines)
	out[idx] = newLine
	return out
}

// ensureTag adds the tag to the top-level `tags:` list if not present.
// Handles three variants:
//
//  1. Block form, multi-line:
//     tags:
//     - foo
//     - bar
//     → insert "- <tag>" at the end of the block.
//
//  2. Inline form: `tags: [foo, bar]`
//     → if tag absent, replace with the block form preserving order.
//
//  3. No tags field at all
//     → append a fresh `tags:\n- <tag>` block before the trailing blank.
func ensureTag(lines []string, tag string) []string {
	tagsLine := -1
	inlineValue := ""
	for i, ln := range lines {
		clean := strings.TrimRight(ln, "\r")
		t := strings.TrimSpace(clean)
		if clean != t {
			continue
		}
		if t == "tags:" {
			tagsLine = i
			break
		}
		if strings.HasPrefix(t, "tags:") {
			tagsLine = i
			rest := strings.TrimSpace(strings.TrimPrefix(t, "tags:"))
			inlineValue = rest // could be `[foo, bar]` or empty
			break
		}
	}
	if tagsLine == -1 {
		// Variant 3: append at end (before trailing blank line if any).
		end := len(lines)
		for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		out := make([]string, 0, len(lines)+2)
		out = append(out, lines[:end]...)
		out = append(out, "tags:", "- "+tag)
		out = append(out, lines[end:]...)
		return out
	}
	// Variant 2: inline `tags: [foo, bar]`. Parse the existing list,
	// add tag if absent, rewrite as a multi-line block. Preserves
	// original tags so we never silently lose data.
	if strings.HasPrefix(inlineValue, "[") && strings.HasSuffix(inlineValue, "]") {
		body := strings.TrimSuffix(strings.TrimPrefix(inlineValue, "["), "]")
		existing := []string{}
		for _, raw := range strings.Split(body, ",") {
			trimmed := strings.TrimSpace(raw)
			trimmed = strings.Trim(trimmed, `"'`)
			if trimmed != "" {
				existing = append(existing, trimmed)
			}
		}
		for _, e := range existing {
			if e == tag {
				return lines
			}
		}
		existing = append(existing, tag)
		out := make([]string, 0, len(lines)+len(existing))
		out = append(out, lines[:tagsLine]...)
		out = append(out, "tags:")
		for _, e := range existing {
			out = append(out, "- "+e)
		}
		out = append(out, lines[tagsLine+1:]...)
		return out
	}
	// Variant 1: block form. Walk forward looking for "- <existing>"
	// lines; append "- <tag>" if not already present.
	already := false
	insertAt := tagsLine + 1
	for i := tagsLine + 1; i < len(lines); i++ {
		t := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if !strings.HasPrefix(t, "- ") {
			break
		}
		insertAt = i + 1
		if strings.TrimSpace(strings.TrimPrefix(t, "- ")) == tag {
			already = true
			break
		}
	}
	if already {
		return lines
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, "- "+tag)
	out = append(out, lines[insertAt:]...)
	return out
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// changelogID derives an item id from the file path: agent-state/{...}/I-406-*.md
// → "I-406". Returns "" for files that don't match the convention.
var idPrefixRE = regexp.MustCompile(`/([IT])-(\d+)-[^/]+\.md$`)

func itemIDFromPath(path string) string {
	m := idPrefixRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s-%s", m[1], m[2])
}

// appendChangelog writes an entry to agent-state/.changelog/<id>.log.
// Best-effort: failures print to stderr but don't abort the migration.
func appendChangelog(path, op, field, oldVal, newVal string) {
	id := itemIDFromPath(path)
	if id == "" {
		return
	}
	// Locate the changelog dir relative to the item file.
	// agent-state/{issues,tasks,archive}/<file> → agent-state/.changelog
	clDir := filepath.Join(filepath.Dir(filepath.Dir(path)), ".changelog")
	if err := os.MkdirAll(clDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "changelog mkdir %s: %v\n", clDir, err)
		return
	}
	entry := fmt.Sprintf("%s op=%s field=%s old=%q new=%q\n",
		time.Now().Format(time.RFC3339), op, field, oldVal, newVal)
	f, err := os.OpenFile(filepath.Join(clDir, id+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	io.WriteString(f, entry)
}

// writeReport renders a Markdown summary of the migration to the
// configured path. In dry-run mode the report is written to stdout
// (prefixed with --- markers) instead of the file path so the operator
// can inspect changes without a side-effect on disk.
func writeReport(path string, results []fileResult, dryRun bool) {
	var w *bufio.Writer
	if dryRun {
		fmt.Println("--- BEGIN dry-run report (would have been written to " + path + ") ---")
		w = bufio.NewWriter(os.Stdout)
		defer func() {
			w.Flush()
			fmt.Println("--- END dry-run report ---")
		}()
	} else {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "writing report: %v\n", err)
			return
		}
		defer f.Close()
		w = bufio.NewWriter(f)
		defer w.Flush()
	}

	mode := "EXECUTE"
	if dryRun {
		mode = "DRY-RUN"
	}
	fmt.Fprintf(w, "# I-406 priority migration report (%s)\n\n", mode)
	fmt.Fprintf(w, "Run at: %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(w, "Total files touched: %d\n\n", len(results))

	// Counts by action.
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Action]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "## Summary by action\n\n")
	for _, k := range keys {
		fmt.Fprintf(w, "- %s: %d\n", k, counts[k])
	}

	// Flagged unknown values get their own section so a human can review.
	flagged := []fileResult{}
	for _, r := range results {
		if r.Action == "flag_unknown" {
			flagged = append(flagged, r)
		}
	}
	if len(flagged) > 0 {
		fmt.Fprintf(w, "\n## Flagged for review (unrecognized severity values)\n\n")
		for _, r := range flagged {
			fmt.Fprintf(w, "- %s — severity=%q → priority=%d (default)\n", r.Path, r.Severity, r.NewPriority)
		}
	}

	fmt.Fprintf(w, "\n## All changes\n\n")
	fmt.Fprintf(w, "| File | Action | Severity | Priority | Tag |\n")
	fmt.Fprintf(w, "|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(w, "| %s | %s | %s | %d | %s |\n",
			r.Path, r.Action, r.Severity, r.NewPriority, r.TagAdded)
	}
}

func printSummary(results []fileResult, dryRun bool) {
	mode := "EXECUTE"
	if dryRun {
		mode = "DRY-RUN"
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Action]++
	}
	fmt.Printf("\n=== I-406 migration (%s) ===\n", mode)
	fmt.Printf("Files touched: %d\n", len(results))
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-32s %d\n", k, counts[k])
	}
	if dryRun {
		fmt.Println("\n(dry-run — no files were written. Re-run without --dry-run to apply.)")
	}
}
