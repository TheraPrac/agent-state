// migrate-sprint-queue is a one-shot data backfill that materializes
// I-488's "sprint = queue filter" model: every non-terminal sprint
// member must have a queue entry. Operators then run
// `st queue approve --sprint <slug>` to flip newly-added pending
// entries into "ready to pick" state.
//
// Policy:
//
//	item has sprint=""          → skip (not a sprint member)
//	item is terminal            → skip (done/abandoned/archived/legacy)
//	queue entry already exists  with any source     → skip (idempotent;
//	                                                  do NOT rewrite the
//	                                                  origin of legacy
//	                                                  entries — they may
//	                                                  be operator-curated
//	                                                  and a future sprint
//	                                                  rm shouldn't cascade)
//	queue entry absent          → append with Approved=false, Source=sprint
//
// New entries land at the end of the queue (preserving operator-curated
// order) and start as Approved=false so a 30-item sprint doesn't flood
// the operator's "next" view. The migration is text-based (no import of
// internal/command) so an old migration binary keeps working as the
// runtime evolves.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// terminalStatuses match cfg.IsTerminalStatus across the unified vocab
// (I-433) plus legacy values that may still exist in archive/ pending
// migrate-status-vocab. Mirrors internal/config but inlined to keep
// the migration text-only.
var terminalStatuses = map[string]bool{
	"done":      true,
	"abandoned": true,
	"archived":  true,
	// legacy / pre-I-433 values kept for archive/ files that haven't
	// been status-vocab-migrated yet:
	"resolved":  true,
	"completed": true,
	"wontfix":   true,
}

type itemRow struct {
	ID     string
	Path   string
	Status string
	Sprint string
}

type queueEntry struct {
	ID       string
	AddedAt  string
	AddedBy  string
	Reason   string
	Approved bool
	Source   string
}

type result struct {
	ID     string
	Action string // "added" | "skipped_terminal" | "skipped_already_queued"
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing")
	// `--root` points at the directory holding issues/, tasks/, archive/.
	// In the standalone test layout (.as/ next to issues/) the queue lives
	// under <root>/.as/queue.yaml. In the real TheraPrac layout the queue
	// lives one directory up at <workspace>/.as/queue.yaml while items
	// live under <workspace>/agent-state/. Use --queue to override.
	root := flag.String("root", ".", "items root (directory holding issues/ tasks/ archive/)")
	queue := flag.String("queue", "", "queue.yaml path (default: <root>/.as/queue.yaml)")
	flag.Parse()

	queuePath := *queue
	if queuePath == "" {
		queuePath = filepath.Join(*root, ".as", "queue.yaml")
	}

	itemDirs := []string{
		filepath.Join(*root, "issues"),
		filepath.Join(*root, "tasks"),
		filepath.Join(*root, "archive"),
	}

	items, err := scanItems(itemDirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan items: %v\n", err)
		os.Exit(1)
	}

	entries, err := loadQueue(queuePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load queue: %v\n", err)
		os.Exit(1)
	}

	results, updated := backfill(items, entries)

	scanned := 0
	for _, it := range items {
		if it.Sprint != "" {
			scanned++
		}
	}

	if !*dryRun {
		if err := saveQueue(queuePath, updated); err != nil {
			fmt.Fprintf(os.Stderr, "save queue: %v\n", err)
			os.Exit(1)
		}
	}

	printSummary(scanned, results, *dryRun)
}

// scanItems walks the item directories and returns one row per .md item
// file with its id/status/sprint extracted from the YAML frontmatter.
func scanItems(dirs []string) ([]itemRow, error) {
	var rows []itemRow
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
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
			row, err := readItem(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
				return nil
			}
			if row != nil {
				rows = append(rows, *row)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func readItem(path string) (*itemRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	row := itemRow{Path: path}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Top-level keys only — must start in column 0. Sprint may be
		// absent on plenty of items so we always read the full file
		// instead of short-circuiting once id+status arrive.
		switch {
		case strings.HasPrefix(line, "id:"):
			row.ID = scalarValue(line, "id:")
		case strings.HasPrefix(line, "status:"):
			row.Status = scalarValue(line, "status:")
		case strings.HasPrefix(line, "sprint:"):
			row.Sprint = scalarValue(line, "sprint:")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, nil
	}
	return &row, nil
}

func scalarValue(line, prefix string) string {
	v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	v = strings.Trim(v, `"`)
	return v
}

// backfill is the pure decision function — given items + current queue,
// return (results, updated entries). Split out so tests can drive it
// without touching the filesystem.
func backfill(items []itemRow, entries []queueEntry) ([]result, []queueEntry) {
	idx := make(map[string]int, len(entries))
	for i, e := range entries {
		idx[e.ID] = i
	}

	// Stable order: sort sprint members by ID for predictable diffs when
	// re-running on subset workspaces.
	members := make([]itemRow, 0, len(items))
	for _, it := range items {
		if it.Sprint == "" {
			continue
		}
		members = append(members, it)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	var results []result
	for _, it := range members {
		if terminalStatuses[it.Status] {
			results = append(results, result{ID: it.ID, Action: "skipped_terminal"})
			continue
		}
		if _, ok := idx[it.ID]; ok {
			// Already in the queue — leave the existing entry alone, no
			// matter what its Source is. We don't know whether the
			// operator queued it standalone or whether it was sprint-
			// sourced under an old version, and rewriting Source could
			// cause a future `st sprint rm` to cascade-remove an entry
			// the operator wanted to keep.
			results = append(results, result{ID: it.ID, Action: "skipped_already_queued"})
			continue
		}
		entries = append(entries, queueEntry{
			ID:       it.ID,
			AddedAt:  time.Now().Format(time.RFC3339),
			AddedBy:  "migration",
			Reason:   fmt.Sprintf("sprint:%s", it.Sprint),
			Approved: false,
			Source:   "sprint",
		})
		idx[it.ID] = len(entries) - 1
		results = append(results, result{ID: it.ID, Action: "added"})
	}

	return results, entries
}

func loadQueue(path string) ([]queueEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []queueEntry
	var current *queueEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "queue:" {
			continue
		}
		if strings.HasPrefix(trimmed, "- id:") {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &queueEntry{
				ID:       strings.TrimSpace(strings.TrimPrefix(trimmed, "- id:")),
				Approved: true,
			}
			continue
		}
		if current == nil {
			continue
		}
		if i := strings.Index(trimmed, ":"); i >= 0 {
			key := strings.TrimSpace(trimmed[:i])
			val := strings.TrimSpace(trimmed[i+1:])
			val = strings.Trim(val, `"`)
			switch key {
			case "added_at":
				current.AddedAt = val
			case "added_by":
				current.AddedBy = val
			case "reason":
				current.Reason = val
			case "approved":
				current.Approved = val == "true"
			case "source":
				current.Source = val
			}
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func saveQueue(path string, entries []queueEntry) error {
	if len(entries) == 0 {
		// Don't create an empty queue file if none existed.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("queue:\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("  - id: %s\n", e.ID))
		if e.AddedAt != "" {
			sb.WriteString(fmt.Sprintf("    added_at: %s\n", e.AddedAt))
		}
		if e.AddedBy != "" {
			sb.WriteString(fmt.Sprintf("    added_by: %s\n", e.AddedBy))
		}
		if e.Reason != "" {
			reason := e.Reason
			if strings.ContainsAny(reason, ":{}[]&*?|>!%@`#") {
				reason = fmt.Sprintf("%q", reason)
			}
			sb.WriteString(fmt.Sprintf("    reason: %s\n", reason))
		}
		if !e.Approved {
			sb.WriteString("    approved: false\n")
		}
		if e.Source != "" && e.Source != "manual" {
			sb.WriteString(fmt.Sprintf("    source: %s\n", e.Source))
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

func printSummary(scanned int, results []result, dryRun bool) {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Action]++
	}
	mode := "applied"
	if dryRun {
		mode = "DRY-RUN — no changes written"
	}
	fmt.Printf("migrate-sprint-queue (%s)\n\n", mode)
	fmt.Printf("Sprint members scanned: %d\n", scanned)
	fmt.Printf("  added (new queue entry):       %d\n", counts["added"])
	fmt.Printf("  skipped (terminal status):     %d\n", counts["skipped_terminal"])
	fmt.Printf("  skipped (already queued):      %d\n", counts["skipped_already_queued"])
	if dryRun && len(results) > 0 {
		fmt.Println("\nDetails:")
		for _, r := range results {
			fmt.Printf("  %-32s %s\n", r.Action, r.ID)
		}
	}
}
