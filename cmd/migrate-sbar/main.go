// migrate-sbar is a one-shot data backfill that adds an sbar:
// composite-content block to every agent-state item file that doesn't
// already have one (I-487). The schema split rewards reviewers because
// every item then carries Situation/Background/Assessment/Recommendation
// instead of a single freeform summary blob.
//
// Migration policy:
//
//	already has sbar:               → skip
//	has legacy summary:             → seed sbar.background from summary;
//	                                  leave situation/assessment/
//	                                  recommendation as empty placeholders
//	                                  for a follow-up human/LLM pass
//	no summary, no sbar:            → write empty sbar: block with all
//	                                  four placeholders so the schema is
//	                                  uniform across the corpus
//
// The migration uses text-based YAML manipulation rather than reparse +
// reserialize so it doesn't risk reformatting unrelated whitespace in
// 800+ files. Insertion is positional: directly after the existing
// summary: block when present, otherwise at the end of the YAML
// frontmatter.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/theraprac/agent-state/internal/model"
)

// fileResult tracks one item migration outcome for the final report.
type fileResult struct {
	Path   string
	Action string // "seeded_from_summary" | "added_empty" | "skipped_has_sbar"
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	root := flag.String("root", ".", "agent-state root (e.g. theraprac-workspace/agent-state)")
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
			base := filepath.Base(path)
			if base == "index.md" || base == "_template.md" {
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

	printSummary(results, *dryRun)
}

// processFile applies the SBAR migration to a single item. Returns
// nil when the file already has a sbar: block (no-op).
func processFile(path string, dryRun bool) (*fileResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)

	if hasTopLevelKey(content, "sbar") {
		return &fileResult{Path: path, Action: "skipped_has_sbar"}, nil
	}

	summary := extractTopLevelBlock(content, "summary")
	newContent, action := insertSBAR(content, summary)
	if newContent == content {
		// Defensive: insertSBAR should always change something when
		// hasTopLevelKey(sbar)=false — but if it didn't, don't write.
		return nil, nil
	}

	if !dryRun {
		if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
			return nil, err
		}
	}
	return &fileResult{Path: path, Action: action}, nil
}

// hasTopLevelKey reports whether the document has a top-level key
// (no leading whitespace) matching name. Used so the migration is
// idempotent — re-running won't double-insert the sbar block.
func hasTopLevelKey(content, name string) bool {
	scanner := bufio.NewScanner(strings.NewReader(content))
	prefix := name + ":"
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// extractTopLevelBlock returns the multi-line content of a top-level
// `name: |-` (or `|`) block, with the block-indent stripped. Returns
// "" when the field is absent or scalar.
func extractTopLevelBlock(content, name string) string {
	lines := strings.Split(content, "\n")
	prefix := name + ":"

	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l, prefix))
		if rest != "|" && rest != "|-" && rest != "|+" && rest != ">" && rest != ">-" {
			// Scalar value — return as-is.
			return rest
		}
		// Multiline block: collect indented lines until indent drops.
		var buf strings.Builder
		for j := i + 1; j < len(lines); j++ {
			ll := lines[j]
			if ll == "" {
				buf.WriteString("\n")
				continue
			}
			if strings.HasPrefix(ll, "  ") {
				if buf.Len() > 0 {
					buf.WriteString("\n")
				}
				buf.WriteString(strings.TrimPrefix(ll, "  "))
				continue
			}
			break
		}
		return strings.TrimRight(buf.String(), "\n")
	}
	return ""
}

// insertSBAR returns content with a freshly-rendered sbar: block
// inserted, and the action label describing what happened. Insertion
// point: right after the summary: block when present, otherwise at
// the end of the YAML frontmatter (or end of file when there's no
// markdown body).
func insertSBAR(content, summary string) (string, string) {
	action := "added_empty"
	if summary != "" {
		action = "seeded_from_summary"
	}

	block := renderSBARBlock(summary)

	lines := strings.Split(content, "\n")
	insertAt := -1

	// Prefer right after the summary block.
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "summary:") {
			// Walk past the block body (lines indented ≥ 2 or empty).
			j := i + 1
			for ; j < len(lines); j++ {
				ll := lines[j]
				if ll == "" || strings.HasPrefix(ll, "  ") {
					continue
				}
				break
			}
			insertAt = j
			break
		}
	}

	// Fallback: append at end of file (works whether or not there's a
	// markdown body — sbar reads as YAML even when inlined late).
	if insertAt == -1 {
		insertAt = len(lines)
	}

	out := make([]string, 0, len(lines)+8)
	out = append(out, lines[:insertAt]...)
	out = append(out, block...)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n"), action
}

// renderSBARBlock builds the lines of a sbar: block. background is
// seeded from `summary` when non-empty; the other three fields are
// always written as empty multiline blocks so the schema is uniform.
//
// I-149: placeholder strings come from model.SBARPlaceholders so this
// migration, st create's scaffold, and the substance gate share a
// single source of truth.
func renderSBARBlock(summary string) []string {
	lines := []string{
		"sbar:",
		"  situation: |-",
		"    " + model.SBARPlaceholders["situation"],
	}
	if summary != "" {
		lines = append(lines, "  background: |-")
		for _, l := range strings.Split(strings.TrimRight(summary, "\n"), "\n") {
			lines = append(lines, "    "+l)
		}
	} else {
		lines = append(lines, "  background: |-",
			"    "+model.SBARPlaceholders["background"])
	}
	lines = append(lines,
		"  assessment: |-",
		"    "+model.SBARPlaceholders["assessment"],
		"  recommendation: |-",
		"    "+model.SBARPlaceholders["recommendation"],
		"")
	return lines
}

// printSummary writes a one-screen summary to stdout.
func printSummary(results []fileResult, dryRun bool) {
	verb := "would-migrate"
	if !dryRun {
		verb = "migrated"
	}

	counts := map[string]int{}
	for _, r := range results {
		counts[r.Action]++
	}

	fmt.Printf("%s: %d items\n", verb, len(results))

	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d\n", k, counts[k])
	}

	if dryRun {
		fmt.Println("\n(dry-run — no files changed; rerun without --dry-run to apply)")
	}
}
