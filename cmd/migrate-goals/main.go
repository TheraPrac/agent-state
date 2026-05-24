// migrate-goals is a one-shot backfill that reads the tags: field on every
// agent-state item and maps legacy goal-membership tags to the new
// goals:[<goal-id>...] list field.
//
// Mapping table (T-410):
//
//	alpha-1         → G-001
//	goal:st-tooling → G-004
//	goal:compliance → G-002
//	goal:payments   → G-003
//
// Any item with a `goal:*` tag not covered by the table fails the run with
// a list of unmapped tags so the operator can add them before re-running.
//
// Behaviors:
//   - Existing goals: entries are preserved; new IDs are appended (no dups).
//   - Tags are left in place for backward-compat grep tooling.
//   - --dry-run prints planned changes without writing.
//   - --root overrides the agent-state root (default ".").
//   - Writes migrate-goals-report.md summarizing what changed.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// tagToGoal is the authoritative mapping from legacy tag to goal ID.
var tagToGoal = map[string]string{
	"alpha-1":         "G-001",
	"goal:compliance": "G-002",
	"goal:payments":   "G-003",
	"goal:st-tooling": "G-004",
}

type fileResult struct {
	Path       string
	GoalsAdded []string
	Skipped    bool
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	root := flag.String("root", ".", "agent-state root directory")
	report := flag.String("report", "migrate-goals-report.md", "where to write the migration report")
	flag.Parse()

	dirs := []string{
		filepath.Join(*root, "issues"),
		filepath.Join(*root, "tasks"),
		filepath.Join(*root, "goals"),
		filepath.Join(*root, "archive"),
	}

	// First pass: collect all unmapped goal: tags so we can fail fast.
	var unmapped []string
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(d, func(path string, info os.FileInfo, _ error) error {
			if info == nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			tags := readTags(path)
			for _, tag := range tags {
				if strings.HasPrefix(tag, "goal:") {
					if _, ok := tagToGoal[tag]; !ok {
						unmapped = append(unmapped, fmt.Sprintf("%s: %s", path, tag))
					}
				}
			}
			return nil
		})
	}
	if len(unmapped) > 0 {
		fmt.Fprintln(os.Stderr, "migrate-goals: unmapped goal: tags found (add to tagToGoal and re-run):")
		for _, u := range unmapped {
			fmt.Fprintln(os.Stderr, "  "+u)
		}
		os.Exit(1)
	}

	var results []fileResult
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "skip missing dir: %s\n", d)
			continue
		}
		_ = filepath.Walk(d, func(path string, info os.FileInfo, _ error) error {
			if info == nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			r, err := processFile(path, *dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
				return nil
			}
			results = append(results, r)
			return nil
		})
	}

	writeReport(*report, results, *dryRun)
	printSummary(results, *dryRun)
}

// processFile maps the item's tags to goal IDs and (unless dry-run) rewrites
// the file. Always returns a result (Skipped=true when nothing to do).
func processFile(path string, dryRun bool) (fileResult, error) {
	r := fileResult{Path: path, Skipped: true}

	data, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	content := string(data)
	lines := strings.Split(content, "\n")

	tags := readTagsFromLines(lines)

	// Determine which goal IDs this item maps to.
	goalSet := make(map[string]bool)
	for _, tag := range tags {
		if gid, ok := tagToGoal[tag]; ok {
			goalSet[gid] = true
		}
	}
	if len(goalSet) == 0 {
		return r, nil
	}

	// Read existing goals: field so we don't duplicate.
	existing := readGoalsFromLines(lines)
	for _, g := range existing {
		goalSet[g] = true
	}

	// Compute new additions.
	var toAdd []string
	for gid := range goalSet {
		found := false
		for _, g := range existing {
			if g == gid {
				found = true
				break
			}
		}
		if !found {
			toAdd = append(toAdd, gid)
		}
	}
	sort.Strings(toAdd)

	if len(toAdd) == 0 {
		return r, nil
	}

	r.Skipped = false
	r.GoalsAdded = toAdd

	if dryRun {
		return r, nil
	}

	// Build the full goals list and rewrite the file.
	allGoals := append(existing, toAdd...)
	sort.Strings(allGoals)
	newContent := rewriteGoals(lines, allGoals)
	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		return r, err
	}
	return r, nil
}

// rewriteGoals rewrites the goals: block in the line slice, or inserts one
// after tags: (or at the end of the header block if no tags field).
func rewriteGoals(lines []string, goals []string) string {
	// Build goals block lines.
	block := []string{"goals:"}
	for _, g := range goals {
		block = append(block, "- "+g)
	}

	// Find existing goals: block and replace it.
	goalsStart, goalsEnd := findGoalsBlock(lines)
	if goalsStart >= 0 {
		out := make([]string, 0, len(lines))
		out = append(out, lines[:goalsStart]...)
		out = append(out, block...)
		out = append(out, lines[goalsEnd:]...)
		return strings.Join(out, "\n")
	}

	// No existing goals: block — insert after tags: block (or after last top-level header field).
	insertAfter := findInsertPoint(lines)
	out := make([]string, 0, len(lines)+len(block)+1)
	out = append(out, lines[:insertAfter+1]...)
	out = append(out, "")
	out = append(out, block...)
	out = append(out, lines[insertAfter+1:]...)
	return strings.Join(out, "\n")
}

// findGoalsBlock returns the [start, end) of an existing goals: block (the
// key line plus all following `- ` list lines). Returns (-1, -1) if absent.
func findGoalsBlock(lines []string) (int, int) {
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "goals:" || strings.HasPrefix(strings.TrimSpace(ln), "goals:") {
			// Confirm top-level (no indent).
			if ln != strings.TrimLeft(ln, " \t") {
				continue
			}
			end := i + 1
			for end < len(lines) {
				next := strings.TrimRight(lines[end], "\r")
				if strings.HasPrefix(next, "- ") {
					end++
				} else {
					break
				}
			}
			return i, end
		}
	}
	return -1, -1
}

// findInsertPoint returns the index of the line after which to insert the
// goals: block. Prefers after the tags: block; falls back to after priority:.
func findInsertPoint(lines []string) int {
	tagsEnd := -1
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "tags:") || strings.HasPrefix(ln, "tags: ") {
			tagsEnd = i
			for i+1 < len(lines) {
				next := strings.TrimRight(lines[i+1], "\r")
				if strings.HasPrefix(next, "- ") {
					i++
					tagsEnd = i
				} else {
					break
				}
			}
			return tagsEnd
		}
	}
	// Fall back to after priority:
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "priority:") {
			return i
		}
	}
	return len(lines) - 1
}

// readTags parses the tags: field of a file on disk.
func readTags(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return readTagsFromLines(strings.Split(string(data), "\n"))
}

// readTagsFromLines parses tags from already-split lines.
func readTagsFromLines(lines []string) []string {
	var tags []string
	inTags := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "tags:") {
			inTags = true
			// Inline format: tags: [a, b]
			rest := strings.TrimSpace(ln[len("tags:"):])
			if rest != "" && rest != "[]" {
				rest = strings.Trim(rest, "[]")
				for _, t := range strings.Split(rest, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						tags = append(tags, t)
					}
				}
				inTags = false // inline — no block
			}
			continue
		}
		if inTags {
			if strings.HasPrefix(ln, "- ") {
				tags = append(tags, strings.TrimSpace(ln[2:]))
			} else if ln == "" || (!strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t")) {
				inTags = false
			}
		}
	}
	return tags
}

// readGoalsFromLines parses the goals: field from already-split lines.
func readGoalsFromLines(lines []string) []string {
	var goals []string
	inGoals := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(ln, "goals:") {
			inGoals = true
			rest := strings.TrimSpace(ln[len("goals:"):])
			if rest != "" && rest != "[]" {
				inGoals = false
			}
			continue
		}
		if inGoals {
			if strings.HasPrefix(ln, "- ") {
				goals = append(goals, strings.TrimSpace(ln[2:]))
			} else if ln == "" || (!strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t")) {
				inGoals = false
			}
		}
	}
	return goals
}

func writeReport(path string, results []fileResult, dryRun bool) {
	var sb strings.Builder
	verb := "Migrated"
	if dryRun {
		verb = "Would migrate"
	}
	sb.WriteString("# migrate-goals report\n\n")
	changed := 0
	for _, r := range results {
		if r.Skipped {
			continue
		}
		changed++
		sb.WriteString(fmt.Sprintf("- %s %s → goals: %s\n", verb, r.Path, strings.Join(r.GoalsAdded, ", ")))
	}
	sb.WriteString(fmt.Sprintf("\nTotal files changed: %d\n", changed))
	_ = os.WriteFile(path, []byte(sb.String()), 0644)
}

func printSummary(results []fileResult, dryRun bool) {
	changed := 0
	for _, r := range results {
		if !r.Skipped {
			changed++
		}
	}
	verb := "Migrated"
	if dryRun {
		verb = "dry run: would migrate"
	}
	fmt.Printf("%s %d file(s)\n", verb, changed)
}
