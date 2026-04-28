// recompute-tokens is a one-shot data backfill that walks every
// agent-state item, drops byte-identical-tuple subagent ai_turns rows
// produced by the I-448 broadcast-fan-out attribution bug, and rewrites
// the rolled-up time_tracking totals.
//
// Dedup rule (mirrors session_log.go's runtime guard): subagent turns
// (step:subagent) within a 60-second window with byte-identical
// (cache_in, reg_in, reg_out, cache_out_1h, role, model) tuples collapse
// to one. Interactive turns are never deduped — they may legitimately
// repeat the same numbers across an item's lifecycle.
//
// Usage:
//
//	recompute-tokens [--dry-run] [--item I-XXX] [--all]
//
// Default --all behavior: walks issues/, tasks/, archive/. With --item
// runs against a single file. --dry-run prints the per-item delta
// without writing.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type turn struct {
	rawIndex int    // index in the original ai_turns slice
	raw      string // line content without the leading "- "
	at       time.Time
	step     string
	role     string
	model    string
	regIn    int
	regOut   int
	cacheIn  int
	cache1h  int
	cost     float64
	process  int
}

type itemReport struct {
	path           string
	id             string
	turnsBefore    int
	turnsAfter     int
	dupsDropped    int
	costDelta      float64
	cacheInDelta   int
	totalInDelta   int
	totalOutDelta  int
	regInDelta     int
	regOutDelta    int
	cache1hDelta   int
	processDelta   int
	turnCountAfter int
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print planned changes without writing files")
	root := flag.String("root", ".", "agent-state root (e.g. theraprac-workspace/agent-state)")
	itemFilter := flag.String("item", "", "limit to a single item ID (e.g. I-432)")
	all := flag.Bool("all", false, "walk all items (default; explicit for parity with --item)")
	flag.Parse()

	_ = all // present for symmetry; the no-flag default already walks all

	dirs := []string{
		filepath.Join(*root, "issues"),
		filepath.Join(*root, "tasks"),
		filepath.Join(*root, "archive"),
	}

	var reports []itemReport
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(d, func(path string, info fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			id := itemIDFromPath(path)
			if *itemFilter != "" && id != *itemFilter {
				return nil
			}
			rep, err := processFile(path, *dryRun)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
				return nil
			}
			if rep != nil && rep.dupsDropped > 0 {
				reports = append(reports, *rep)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk %s: %v\n", d, err)
		}
	}

	printSummary(reports, *dryRun)
}

var idPrefixRE = regexp.MustCompile(`/([IT])-(\d+)-[^/]+\.md$`)

func itemIDFromPath(path string) string {
	m := idPrefixRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s-%s", m[1], m[2])
}

// processFile reads one item file, computes the dedupe delta, and
// (unless dryRun) rewrites the file. Returns nil if no time_tracking /
// no ai_turns / no dups detected.
func processFile(path string, dryRun bool) (*itemReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")

	turnsStart, turnsEnd := findAITurnsRange(lines)
	if turnsStart < 0 || turnsEnd <= turnsStart {
		return nil, nil
	}

	var turns []turn
	for i := turnsStart; i < turnsEnd; i++ {
		ln := strings.TrimRight(lines[i], "\r")
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		raw := strings.TrimPrefix(t, "- ")
		entry := parseTurn(raw)
		entry.rawIndex = i
		turns = append(turns, entry)
	}

	// Identify duplicates: subagent turns within 60s of each other
	// sharing (cache_in, reg_in, reg_out, cache_out_1h, role, model).
	keep := dedupTurns(turns)
	if len(keep) == len(turns) {
		return nil, nil
	}

	rep := &itemReport{
		path:        path,
		id:          itemIDFromPath(path),
		turnsBefore: len(turns),
		turnsAfter:  len(keep),
		dupsDropped: len(turns) - len(keep),
	}
	for _, t := range turns {
		rep.cacheInDelta -= t.cacheIn
		rep.cache1hDelta -= t.cache1h
		rep.regInDelta -= t.regIn
		rep.regOutDelta -= t.regOut
		rep.totalInDelta -= t.regIn + t.cacheIn + t.cache1h
		rep.totalOutDelta -= t.regOut
		rep.costDelta -= t.cost
		rep.processDelta -= t.process
	}
	for _, t := range keep {
		rep.cacheInDelta += t.cacheIn
		rep.cache1hDelta += t.cache1h
		rep.regInDelta += t.regIn
		rep.regOutDelta += t.regOut
		rep.totalInDelta += t.regIn + t.cacheIn + t.cache1h
		rep.totalOutDelta += t.regOut
		rep.costDelta += t.cost
		rep.processDelta += t.process
	}
	rep.turnCountAfter = len(keep)

	if dryRun {
		return rep, nil
	}

	// Rewrite the file: drop dropped lines, rewrite rolled-up totals.
	dropIdx := map[int]bool{}
	keepSet := map[int]bool{}
	for _, t := range keep {
		keepSet[t.rawIndex] = true
	}
	for _, t := range turns {
		if !keepSet[t.rawIndex] {
			dropIdx[t.rawIndex] = true
		}
	}
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		if dropIdx[i] {
			continue
		}
		out = append(out, ln)
	}
	out = applyTotalsDeltas(out, rep)

	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return nil, err
	}
	return rep, nil
}

// dedupTurns returns the subset of turns to keep: each subagent
// equivalence class (same tuple, within 60s of an earlier kept member)
// collapses to its first member; interactive turns and never-seen
// tuples pass through unchanged.
func dedupTurns(turns []turn) []turn {
	keep := make([]turn, 0, len(turns))
	for _, t := range turns {
		if t.step != "subagent" {
			keep = append(keep, t)
			continue
		}
		dup := false
		for _, k := range keep {
			if k.step != "subagent" {
				continue
			}
			if t.regIn != k.regIn || t.regOut != k.regOut ||
				t.cacheIn != k.cacheIn || t.cache1h != k.cache1h ||
				t.role != k.role || t.model != k.model {
				continue
			}
			delta := t.at.Sub(k.at)
			if delta < 0 {
				delta = -delta
			}
			if delta <= 60*time.Second {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, t)
		}
	}
	return keep
}

// parseTurn extracts the fields used by the dedup tuple. Missing
// fields default to zero / empty (matches session_log.go's
// extractIntField semantics).
func parseTurn(raw string) turn {
	atStr := extractField(raw, "at:")
	at, _ := time.Parse(time.RFC3339, atStr)
	cost := 0.0
	costStr := extractField(raw, "cost:")
	if strings.HasPrefix(costStr, "$") {
		v, _ := strconv.ParseFloat(strings.TrimPrefix(costStr, "$"), 64)
		cost = v
	}
	process := 0
	procStr := extractField(raw, "process:")
	if strings.HasSuffix(procStr, "s") {
		v, _ := strconv.Atoi(strings.TrimSuffix(procStr, "s"))
		process = v
	}
	return turn{
		raw:     raw,
		at:      at,
		step:    extractField(raw, "step:"),
		role:    extractField(raw, "role:"),
		model:   extractField(raw, "model:"),
		regIn:   extractInt(raw, "reg_in:"),
		regOut:  extractInt(raw, "reg_out:"),
		cacheIn: extractInt(raw, "cache_in:"),
		cache1h: extractInt(raw, "cache_out_1h:"),
		cost:    cost,
		process: process,
	}
}

func extractField(line, key string) string {
	idx := strings.Index(line, key)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return rest
}

func extractInt(line, key string) int {
	v := extractField(line, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// findAITurnsRange returns [start, end) line indices for the
// time_tracking.ai_turns list. Treats CRLF tolerantly.
func findAITurnsRange(lines []string) (int, int) {
	inTimeTracking := false
	inAITurns := false
	start := -1
	for i, ln := range lines {
		clean := strings.TrimRight(ln, "\r")
		// top-level field — if we were inside ai_turns, the new top-level
		// key terminates the list. Return BEFORE updating state so the
		// caller sees the correct end index.
		if !strings.HasPrefix(clean, " ") && strings.HasSuffix(clean, ":") {
			if inAITurns {
				return start, i
			}
			key := strings.TrimSuffix(clean, ":")
			inTimeTracking = key == "time_tracking"
			continue
		}
		if !inTimeTracking {
			continue
		}
		if strings.HasPrefix(clean, "  ai_turns:") {
			inAITurns = true
			start = i + 1
			continue
		}
		// any other non-list, indent-2 field ends the ai_turns section
		if inAITurns && strings.HasPrefix(clean, "  ") && !strings.HasPrefix(strings.TrimLeft(clean, " "), "- ") && !strings.HasPrefix(clean, "    ") {
			return start, i
		}
	}
	if inAITurns {
		return start, len(lines)
	}
	return -1, -1
}

// applyTotalsDeltas rewrites the rolled-up time_tracking integer +
// cost fields in-place. Conservative: only fields the dedupe touched
// are updated.
func applyTotalsDeltas(lines []string, rep *itemReport) []string {
	updateInt := func(key string, delta int) {
		updateField(lines, key, func(v string) string {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			n += delta
			return strconv.Itoa(n)
		})
	}
	updateInt("reg_input_tokens", rep.regInDelta)
	updateInt("reg_output_tokens", rep.regOutDelta)
	updateInt("cache_in_tokens", rep.cacheInDelta)
	updateInt("cache_out_1h_tokens", rep.cache1hDelta)
	updateInt("total_input_tokens", rep.totalInDelta)
	updateInt("total_output_tokens", rep.totalOutDelta)
	updateInt("turn_count", rep.turnsAfter-rep.turnsBefore)
	updateInt("process_time_seconds", rep.processDelta)

	// ai_cost_usd uses a 6-decimal float; preserve that precision.
	updateField(lines, "ai_cost_usd", func(v string) string {
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		f += rep.costDelta
		return fmt.Sprintf("%.6f", f)
	})
	return lines
}

func updateField(lines []string, key string, transform func(old string) string) {
	prefix := "  " + key + ": "
	for i, ln := range lines {
		clean := strings.TrimRight(ln, "\r")
		if !strings.HasPrefix(clean, prefix) {
			continue
		}
		old := clean[len(prefix):]
		lines[i] = prefix + transform(old)
		return
	}
}

func printSummary(reports []itemReport, dryRun bool) {
	mode := "EXECUTE"
	if dryRun {
		mode = "DRY-RUN"
	}
	fmt.Printf("\n=== I-448 token recompute (%s) ===\n", mode)
	fmt.Printf("Items affected: %d\n\n", len(reports))

	sort.Slice(reports, func(i, j int) bool {
		return reports[i].costDelta < reports[j].costDelta
	})

	totalDups := 0
	totalCost := 0.0
	totalTokens := 0
	for _, r := range reports {
		fmt.Printf("  %s  dropped=%d  Δturns=%d→%d  Δcost=$%.2f  Δcache_in=%d  Δtotal_in=%d\n",
			r.id, r.dupsDropped, r.turnsBefore, r.turnsAfter,
			r.costDelta, r.cacheInDelta, r.totalInDelta)
		totalDups += r.dupsDropped
		totalCost += r.costDelta
		totalTokens += r.totalInDelta
	}
	fmt.Printf("\nTotals: %d turns dropped  $%.2f cost adjustment  %d total_input_tokens adjustment\n",
		totalDups, totalCost, totalTokens)
	if dryRun {
		fmt.Println("\n(dry-run — no files written. Re-run without --dry-run to apply.)")
	}
}
