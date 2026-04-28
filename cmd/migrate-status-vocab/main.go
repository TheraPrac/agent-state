// migrate-status-vocab is a one-shot data backfill that rewrites every
// agent-state item file to use the unified status vocabulary
// (queued/active/done/abandoned/archived) per the I-433 decision.
//
// Migration table:
//
//	open       (issue)  → queued
//	resolved   (issue)  → done
//	wontfix    (issue)  → abandoned
//	completed  (task)   → done
//	queued, active, abandoned, archived  → unchanged
//
// File location stays the same per item — issues remain under issues/
// or archive/, tasks under tasks/ or archive/. The migration is a pure
// status field rewrite. Following the same pattern as I-406's
// cmd/migrate-priority: text-based YAML manipulation, dry-run mode,
// per-item changelog, final Markdown report.
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

// statusRemap maps legacy status values to the unified vocabulary. Any
// status value not in this map is left unchanged (queued/active/abandoned/
// archived already conform).
var statusRemap = map[string]string{
	"open":      "queued",
	"resolved":  "done",
	"wontfix":   "abandoned",
	"completed": "done",
}

type fileResult struct {
	Path      string
	OldStatus string
	NewStatus string
	Action    string // "remap" or "skip_already_conforming"
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	root := flag.String("root", ".", "agent-state root (e.g. theraprac-workspace/agent-state)")
	report := flag.String("report", "migrate-status-vocab-report.md", "report path (stdout in dry-run)")
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

// processFile reads one item file, rewrites status if it's a legacy
// value, and (unless dryRun) writes the result. Returns nil for files
// that don't need migration.
func processFile(path string, dryRun bool) (*fileResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")

	status, statusLine := findField(lines, "status")
	if status == "" {
		return nil, nil
	}
	newStatus, needsRemap := statusRemap[status]
	if !needsRemap {
		// Already conforming — silently skip.
		return nil, nil
	}

	r := &fileResult{
		Path:      path,
		OldStatus: status,
		NewStatus: newStatus,
		Action:    "remap",
	}

	newLines := replaceLine(lines, statusLine, "status: "+newStatus)
	if !dryRun {
		if err := writeLines(path, newLines); err != nil {
			return nil, err
		}
		appendChangelog(path, "migrate", "status", status, newStatus)
	}
	return r, nil
}

// findField — same shape as cmd/migrate-priority. Tolerates CRLF.
func findField(lines []string, field string) (string, int) {
	prefix := field + ":"
	for i, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		t := strings.TrimSpace(ln)
		if ln != t {
			continue
		}
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		v := strings.TrimSpace(t[len(prefix):])
		if idx := strings.Index(v, " #"); idx >= 0 {
			v = strings.TrimSpace(v[:idx])
		}
		return v, i
	}
	return "", -1
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

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

var idPrefixRE = regexp.MustCompile(`/([IT])-(\d+)-[^/]+\.md$`)

func itemIDFromPath(path string) string {
	m := idPrefixRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s-%s", m[1], m[2])
}

func appendChangelog(path, op, field, oldVal, newVal string) {
	id := itemIDFromPath(path)
	if id == "" {
		return
	}
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
	fmt.Fprintf(w, "# I-433 status vocabulary migration report (%s)\n\n", mode)
	fmt.Fprintf(w, "Run at: %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(w, "Total files touched: %d\n\n", len(results))

	transitions := map[string]int{}
	for _, r := range results {
		key := r.OldStatus + " → " + r.NewStatus
		transitions[key]++
	}
	keys := make([]string, 0, len(transitions))
	for k := range transitions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "## Summary by transition\n\n")
	for _, k := range keys {
		fmt.Fprintf(w, "- %s: %d\n", k, transitions[k])
	}

	fmt.Fprintf(w, "\n## All changes\n\n")
	fmt.Fprintf(w, "| File | Old status | New status |\n|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(w, "| %s | %s | %s |\n", r.Path, r.OldStatus, r.NewStatus)
	}
}

func printSummary(results []fileResult, dryRun bool) {
	mode := "EXECUTE"
	if dryRun {
		mode = "DRY-RUN"
	}
	transitions := map[string]int{}
	for _, r := range results {
		key := r.OldStatus + " → " + r.NewStatus
		transitions[key]++
	}
	fmt.Printf("\n=== I-433 status migration (%s) ===\n", mode)
	fmt.Printf("Files touched: %d\n", len(results))
	keys := make([]string, 0, len(transitions))
	for k := range transitions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-32s %d\n", k, transitions[k])
	}
	if dryRun {
		fmt.Println("\n(dry-run — no files were written. Re-run without --dry-run to apply.)")
	}
}
