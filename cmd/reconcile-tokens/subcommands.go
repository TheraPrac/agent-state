package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// bySessionLine mirrors the format produced by
// internal/command/session_log_schema.go's formatBySessionLine. We re-parse
// here rather than importing internal/command to keep this binary's
// dependency surface small.
type bySessionLine struct {
	SID        string
	ProjectDir string
	StartedAt  time.Time
	EndedAt    time.Time
	Turns      int
	Tokens     realTokens
}

func parseBySessionLines(lines []string) []bySessionLine {
	var out []bySessionLine
	for _, raw := range lines {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		raw = strings.TrimPrefix(raw, "- ")
		var s bySessionLine
		for _, tok := range strings.Fields(raw) {
			eq := strings.IndexByte(tok, '=')
			if eq < 0 {
				continue
			}
			k, v := tok[:eq], tok[eq+1:]
			switch k {
			case "sid":
				s.SID = v
			case "project_dir":
				s.ProjectDir = decodeFieldValue(v)
			case "started_at":
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					s.StartedAt = t
				}
			case "ended_at":
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					s.EndedAt = t
				}
			case "turns":
				n, _ := strconv.Atoi(v)
				s.Turns = n
			case "input":
				n, _ := strconv.Atoi(v)
				s.Tokens.Input = n
			case "output":
				n, _ := strconv.Atoi(v)
				s.Tokens.Output = n
			case "cache_read":
				n, _ := strconv.Atoi(v)
				s.Tokens.CacheRead = n
			case "cache_creation_5m":
				n, _ := strconv.Atoi(v)
				s.Tokens.CacheCreation5m = n
			case "cache_creation_1h":
				n, _ := strconv.Atoi(v)
				s.Tokens.CacheCreation1h = n
			}
		}
		if s.SID != "" {
			out = append(out, s)
		}
	}
	return out
}

// extractTimeTrackingListLines walks an item's Doc and returns the list-entry
// raw lines under time_tracking.<key>. Used to feed parseBySessionLines and
// (for migration) to identify what's there.
func extractTimeTrackingListLines(item *model.Item, key string) []string {
	if item == nil || item.Doc == nil {
		return nil
	}
	var out []string
	inTT := false
	inBlock := false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inTT = line.Key == "time_tracking"
			inBlock = false
			continue
		}
		if !inTT {
			continue
		}
		if line.Indent == 2 && line.Key == key {
			inBlock = true
			continue
		}
		if line.Indent <= 2 && line.Key != "" && line.Key != key {
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		if t := strings.TrimSpace(line.Raw); strings.HasPrefix(t, "- ") {
			out = append(out, t)
		}
	}
	return out
}

// readRecordedRealTokens pulls the cumulative real_tokens blob off an item.
// Returns zero value if the field is absent.
func readRecordedRealTokens(item *model.Item) realTokens {
	val, ok := item.Doc.GetNestedField("time_tracking.real_tokens")
	if !ok {
		return realTokens{}
	}
	var t realTokens
	for _, tok := range strings.Fields(val) {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			continue
		}
		k, v := tok[:eq], tok[eq+1:]
		n, _ := strconv.Atoi(v)
		switch k {
		case "input":
			t.Input = n
		case "output":
			t.Output = n
		case "cache_read":
			t.CacheRead = n
		case "cache_creation_5m":
			t.CacheCreation5m = n
		case "cache_creation_1h":
			t.CacheCreation1h = n
		}
	}
	return t
}

// decodeFieldValue mirrors session_log_schema.go's encoder. project_dir
// fields are URL-encoded for spaces / tabs so they survive the
// strings.Fields tokenization on parse. Bare values pass through unchanged.
func decodeFieldValue(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	s = strings.ReplaceAll(s, "%20", " ")
	s = strings.ReplaceAll(s, "%09", "\t")
	return s
}

// formatRealTokensLine matches the writer in session_log_schema.go.
func formatRealTokensLine(t realTokens) string {
	return fmt.Sprintf(
		"input=%d output=%d cache_read=%d cache_creation_5m=%d cache_creation_1h=%d",
		t.Input, t.Output, t.CacheRead, t.CacheCreation5m, t.CacheCreation1h,
	)
}

// reconcileAll loops every item, runs reconcile, and returns rows. A `since`
// filter (item's last_touched within the window) trims the scan; pass zero
// duration to scan everything.
func reconcileAll(s *store.Store, since time.Duration) ([]reconcileRow, error) {
	var rows []reconcileRow
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	for _, item := range s.All() {
		if !cutoff.IsZero() {
			lt, _ := item.Doc.GetNestedField("time_tracking.last_touched")
			if lt == "" {
				continue
			}
			if ts, err := time.Parse(time.RFC3339, lt); err != nil || ts.Before(cutoff) {
				continue
			}
		}
		recorded := readRecordedRealTokens(item)
		// Skip items without real_tokens AND without by_session — nothing
		// to reconcile against.
		bs := extractTimeTrackingListLines(item, "by_session")
		if recorded.sum() == 0 && len(bs) == 0 {
			continue
		}
		truth, sessions, jsonls, err := reconcileItem(itemTTLines{lines: bs}, recorded)
		if err != nil {
			rows = append(rows, reconcileRow{ItemID: item.ID, Notes: "error: " + err.Error()})
			continue
		}
		rows = append(rows, driftRow(item.ID, recorded, truth, sessions, jsonls))
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].InflationFactor > rows[j].InflationFactor
	})
	return rows, nil
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// --- report subcommand ---

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	since := fs.String("since", "30d", "only consider items touched within this window (e.g. 7d, 24h)")
	rootDir := fs.String("root", ".", "agent-state root (parent of issues/, tasks/, archive/)")
	fs.Parse(args)

	dur, err := parseDuration(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens report: bad --since: %v\n", err)
		return 1
	}

	s, _, err := loadStore(*rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens report: %v\n", err)
		return 1
	}
	rows, err := reconcileAll(s, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens report: %v\n", err)
		return 1
	}

	fmt.Printf("%-10s %12s %12s %8s %8s %8s  %s\n",
		"item", "recorded", "truth", "drift%", "x-factor", "sessions", "notes")
	for _, r := range rows {
		xf := "—"
		if r.InflationFactor > 0 {
			xf = fmt.Sprintf("%.2fx", r.InflationFactor)
		}
		fmt.Printf("%-10s %12d %12d %7.1f%% %8s %8d  %s\n",
			r.ItemID, r.RecordedTokens.sum(), r.TruthTokens.sum(),
			r.DriftPct, xf, r.SessionsScanned, r.Notes)
	}
	if len(rows) == 0 {
		fmt.Println("(no items in window)")
	}
	return 0
}

// --- apply subcommand ---

func cmdApply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	itemID := fs.String("item", "", "limit to a single item id (e.g. I-432)")
	all := fs.Bool("all", false, "apply across every item with by_session data")
	threshold := fs.Float64("threshold", 0.05, "drift fraction above which apply rewrites real_tokens")
	rootDir := fs.String("root", ".", "agent-state root")
	fs.Parse(args)

	if *itemID == "" && !*all {
		fmt.Fprintln(os.Stderr, "reconcile-tokens apply: pass --item I-XXX or --all")
		return 1
	}

	s, _, err := loadStore(*rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens apply: %v\n", err)
		return 1
	}

	// Open the changelog (append-only).
	logPath := filepath.Join(*rootDir, ".as", ".changelog", "I-569-reconcile.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens apply: changelog dir: %v\n", err)
		return 1
	}
	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens apply: changelog open: %v\n", err)
		return 1
	}
	defer logF.Close()

	rewrites := 0
	scanned := 0
	process := func(item *model.Item) {
		// I-569 finding-4: read recorded tokens INSIDE the Mutate closure so
		// a concurrent st session log can't slip a write between our snapshot
		// and the rewrite (T-304's purity-under-lock rule). Truth is computed
		// from JSONL files outside the lock — that's fine because JSONL is
		// append-only and reconcile sees a consistent snapshot.
		var (
			recorded realTokens
			truth    realTokens
			sessions int
			jsonls   int
			row      reconcileRow
			err      error
			rewrite  bool
		)
		bs := extractTimeTrackingListLines(item, "by_session")
		if len(bs) == 0 {
			return
		}
		// Compute truth outside the lock — JSONL is the source of ground truth
		// and isn't mutated by SessionLog.
		// (recorded gets re-read fresh inside Mutate just below.)
		_ = recorded // populated below
		truth, sessions, jsonls, err = reconcileItem(itemTTLines{lines: bs}, realTokens{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile-tokens apply: %s: %v\n", item.ID, err)
			return
		}
		scanned++
		if err := s.Mutate(item.ID, func(it *model.Item) error {
			recorded = readRecordedRealTokens(it)
			row = driftRow(item.ID, recorded, truth, sessions, jsonls)
			if row.TruthTokens.sum() == 0 || row.DriftPct/100 < *threshold {
				return nil // nothing to do; closure exits without writing
			}
			it.SetNested("time_tracking", "real_tokens", formatRealTokensLine(truth))
			it.SetNested("time_tracking", "last_reconciled_at", time.Now().Format(time.RFC3339))
			rewrite = true
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile-tokens apply: %s: %v\n", item.ID, err)
			return
		}
		if !rewrite {
			return // below threshold or no truth — closure already returned cleanly
		}
		rewrites++
		fmt.Fprintf(logF, "%s\t%s\trecorded=%d\ttruth=%d\tdrift=%.1f%%\tx_factor=%.2f\tsessions=%d\n",
			time.Now().Format(time.RFC3339), item.ID,
			recorded.sum(), truth.sum(), row.DriftPct, row.InflationFactor, sessions)
		fmt.Printf("rewrote %s: recorded=%d -> truth=%d (%.1f%% drift, %.2fx)\n",
			item.ID, recorded.sum(), truth.sum(), row.DriftPct, row.InflationFactor)
	}

	if *itemID != "" {
		it, ok := s.Get(*itemID)
		if !ok {
			fmt.Fprintf(os.Stderr, "reconcile-tokens apply: item %s not found\n", *itemID)
			return 1
		}
		process(it)
	} else {
		for _, it := range s.All() {
			process(it)
		}
	}
	fmt.Printf("scanned=%d rewrites=%d threshold=%.0f%%\n", scanned, rewrites, *threshold*100)
	return 0
}

// --- verify subcommand ---

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	since := fs.String("since", "7d", "verify items touched within this window")
	rootDir := fs.String("root", ".", "agent-state root")
	maxFactor := fs.Float64("max-factor", 1.5, "fail if any item's inflation_factor exceeds this")
	fs.Parse(args)

	dur, err := parseDuration(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens verify: bad --since: %v\n", err)
		return 1
	}

	s, _, err := loadStore(*rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens verify: %v\n", err)
		return 1
	}
	rows, err := reconcileAll(s, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens verify: %v\n", err)
		return 1
	}

	worst := 0
	for _, r := range rows {
		if r.InflationFactor > *maxFactor {
			fmt.Printf("FAIL %s inflation_factor=%.2fx (recorded=%d truth=%d)\n",
				r.ItemID, r.InflationFactor, r.RecordedTokens.sum(), r.TruthTokens.sum())
			worst++
		}
	}
	if worst > 0 {
		fmt.Printf("\nverify failed: %d item(s) exceed --max-factor=%.2f within --since=%s\n",
			worst, *maxFactor, *since)
		return 2
	}
	fmt.Printf("verify ok: %d item(s) scanned, all within %.2fx\n", len(rows), *maxFactor)
	return 0
}

// --- migrate-strip-cost subcommand ---

func cmdMigrateStripCost(args []string) int {
	fs := flag.NewFlagSet("migrate-strip-cost", flag.ExitOnError)
	apply := fs.Bool("apply", false, "actually write changes (default: dry-run)")
	rootDir := fs.String("root", ".", "agent-state root")
	fs.Parse(args)

	s, _, err := loadStore(*rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile-tokens migrate-strip-cost: %v\n", err)
		return 1
	}

	stripFields := []string{
		"time_tracking.ai_cost_usd",
		"time_tracking.total_ai_cost_usd",
		"time_tracking.last_cost_source",
		"time_tracking.unknown_cost_turns",
		"time_tracking.total_input_tokens",
		"time_tracking.total_output_tokens",
	}

	stripped := 0
	scanned := 0
	for _, item := range s.All() {
		scanned++
		hits := 0
		for _, path := range stripFields {
			if _, ok := item.Doc.GetNestedField(path); ok {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		if *apply {
			if err := s.Mutate(item.ID, func(it *model.Item) error {
				for _, path := range stripFields {
					it.Doc.RemoveNestedField(path)
				}
				it.SetNested("time_tracking", "last_reconciled_at", time.Now().Format(time.RFC3339))
				return nil
			}); err != nil {
				fmt.Fprintf(os.Stderr, "migrate-strip-cost: %s: %v\n", item.ID, err)
				continue
			}
			fmt.Printf("stripped %s (%d field(s))\n", item.ID, hits)
		} else {
			fmt.Printf("would strip %s (%d field(s))\n", item.ID, hits)
		}
		stripped++
	}
	mode := "dry-run"
	if *apply {
		mode = "applied"
	}
	fmt.Printf("scanned=%d stripped=%d (%s)\n", scanned, stripped, mode)
	return 0
}
