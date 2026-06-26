package command

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/pricing"
	"github.com/theraprac/agent-state/internal/store"
	"github.com/theraprac/agent-state/internal/transcript"
)

// MetricsBackfillOpts holds flags for the metrics backfill subcommand.
type MetricsBackfillOpts struct {
	DryRun bool
}

// backfillResult holds extracted data from one or more JSONL session files.
type backfillResult struct {
	Input     int
	Output    int
	CacheRead int
	CacheOut  int // 5-minute cache creation bucket
	Turns     int
	Model     string // last model seen across all turns
	FirstTS   time.Time
	LastTS    time.Time
}

func (r backfillResult) hasData() bool {
	return r.Input > 0 || r.Output > 0 || r.CacheRead > 0 || r.Turns > 0
}

// MetricsBackfill walks closed items that lack cost/token data, resolves their
// linked session transcripts via the top-level sessions: field, and writes
// token/cost/duration data back into time_tracking. For items with no sessions
// or missing transcripts, a sentinel is recorded so the absence is visible.
func MetricsBackfill(s *store.Store, _ *config.Config, opts MetricsBackfillOpts) int {
	var tried, costed, noSession, noTranscript, skipped int

	for _, item := range s.All() {
		// Only closed items are candidates.
		if item.Status != "done" && item.Status != "archived" && item.Completed == nil {
			continue
		}

		// Skip if cost/token data already present (idempotent).
		if readFloatField(item, "time_tracking", "ai_cost_usd") > 0 ||
			readIntField(item, "time_tracking", "reg_input_tokens") > 0 ||
			readIntField(item, "time_tracking", "cache_in_tokens") > 0 {
			skipped++
			continue
		}

		// Skip if a backfill_status sentinel already set (idempotent re-runs).
		if readStringNestedField(item, "time_tracking", "backfill_status") != "" {
			skipped++
			continue
		}

		tried++

		if len(item.Sessions) == 0 {
			noSession++
			writeBackfillSentinel(s, item.ID, "no_session", opts.DryRun)
			fmt.Printf("  [no_session] %s %q\n", item.ID, truncateTitle(item.Title, 40))
			continue
		}

		// Collect JSONL paths from top-level session UUIDs (no project_dir).
		var paths []string
		for _, sid := range item.Sessions {
			paths = append(paths, transcript.ResolveSessionByID(sid)...)
		}
		if len(paths) == 0 {
			noTranscript++
			writeBackfillSentinel(s, item.ID, "no_transcript", opts.DryRun)
			fmt.Printf("  [no_transcript] %s %q — sessions listed but transcripts not on disk\n",
				item.ID, truncateTitle(item.Title, 40))
			continue
		}

		res := parseBackfillJSONL(paths)
		if !res.hasData() {
			noTranscript++
			writeBackfillSentinel(s, item.ID, "no_transcript", opts.DryRun)
			fmt.Printf("  [no_transcript] %s %q — transcripts present but contain no usage data\n",
				item.ID, truncateTitle(item.Title, 40))
			continue
		}

		var costUSD float64
		if res.Model != "" {
			if c, err := pricing.EstimateSyntheticCostUSD(res.Model, res.Input, res.Output, res.CacheRead, res.CacheOut, 0); err == nil {
				costUSD = c
			}
		}

		var durSec int64
		if !res.FirstTS.IsZero() && !res.LastTS.IsZero() && res.LastTS.After(res.FirstTS) {
			durSec = int64(res.LastTS.Sub(res.FirstTS).Seconds())
		}

		if opts.DryRun {
			fmt.Printf("  [dry-run] %s %q — in=%d out=%d cache_r=%d cache_w=%d turns=%d cost=$%.4f process=%ds model=%s\n",
				item.ID, truncateTitle(item.Title, 40),
				res.Input, res.Output, res.CacheRead, res.CacheOut,
				res.Turns, costUSD, durSec, res.Model)
			costed++
			continue
		}

		capturedRes := res
		capturedCost := costUSD
		capturedDur := durSec
		capturedPaths := len(paths)

		if err := s.Mutate(item.ID, func(it *model.Item) error {
			it.SetNested("time_tracking", "reg_input_tokens", fmt.Sprintf("%d", capturedRes.Input))
			it.SetNested("time_tracking", "reg_output_tokens", fmt.Sprintf("%d", capturedRes.Output))
			it.SetNested("time_tracking", "cache_in_tokens", fmt.Sprintf("%d", capturedRes.CacheRead))
			it.SetNested("time_tracking", "cache_out_tokens", fmt.Sprintf("%d", capturedRes.CacheOut))
			if capturedRes.Turns > 0 {
				it.SetNested("time_tracking", "turn_count", fmt.Sprintf("%d", capturedRes.Turns))
			}
			if capturedCost > 0 {
				it.SetNested("time_tracking", "ai_cost_usd", fmt.Sprintf("%.6f", capturedCost))
			}
			if capturedDur > 0 {
				// Write as process_time_seconds — the field ExtractItemMetrics
				// reads for ProcessTime. "total_duration_seconds" is not read
				// by any consumer; this field is.
				it.SetNested("time_tracking", "process_time_seconds", fmt.Sprintf("%d", capturedDur))
			}
			if capturedRes.Model != "" {
				it.SetNested("time_tracking", "last_model", capturedRes.Model)
			}
			it.SetNested("time_tracking", "backfill_sessions_scanned", fmt.Sprintf("%d", capturedPaths))
			it.SetNested("time_tracking", "backfill_status", "done")
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "metrics backfill: %s: %v\n", item.ID, err)
			continue
		}

		fmt.Printf("  [done] %s %q — $%.4f %d turns process=%ds\n",
			item.ID, truncateTitle(item.Title, 40), costUSD, res.Turns, durSec)
		costed++
	}

	fmt.Printf("\nmetrics backfill: %d tried — %d costed, %d no_session, %d no_transcript, %d skipped\n",
		tried, costed, noSession, noTranscript, skipped)
	return 0
}

// writeBackfillSentinel records the sentinel in time_tracking.backfill_status.
// In dry-run mode it is a no-op (caller already printed the status line).
func writeBackfillSentinel(s *store.Store, id, status string, dryRun bool) {
	if dryRun {
		return
	}
	_ = s.Mutate(id, func(it *model.Item) error {
		it.SetNested("time_tracking", "backfill_status", status)
		return nil
	})
}

// parseBackfillJSONL sums token/turn data across all given JSONL file paths.
// Mirrors reconcile-tokens/main.go:jsonlUsage but also captures model and
// wall-clock span (first/last assistant message timestamp).
func parseBackfillJSONL(paths []string) backfillResult {
	type usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}
	type wireRec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   *struct {
			Model string `json:"model"`
			Usage *usage `json:"usage"`
		} `json:"message"`
	}

	var res backfillResult
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			var rec wireRec
			if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
				continue
			}
			if rec.Type != "assistant" || rec.Message == nil || rec.Message.Usage == nil {
				continue
			}
			u := rec.Message.Usage
			res.Input += u.InputTokens
			res.Output += u.OutputTokens
			res.CacheRead += u.CacheReadInputTokens
			res.CacheOut += u.CacheCreationInputTokens
			res.Turns++
			if rec.Message.Model != "" {
				res.Model = rec.Message.Model
			}
			if rec.Timestamp != "" {
				// RFC3339Nano is required: real Claude Code transcripts use
				// fractional-second timestamps (e.g. "…T17:00:02.200Z").
				// RFC3339 silently fails on those, leaving all timestamps zero.
				if ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
					if res.FirstTS.IsZero() {
						res.FirstTS = ts
					}
					res.LastTS = ts
				}
			}
		}
		f.Close()
	}
	return res
}

// readStringNestedField reads a nested YAML field as a string.
// Returns "" if absent. Uses the typed TimeTracking map for "time_tracking".
func readStringNestedField(item *model.Item, parent, key string) string {
	s, _ := getNestedField(item, parent, key)
	return s
}
