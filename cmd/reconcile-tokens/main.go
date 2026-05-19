// reconcile-tokens compares per-item time_tracking.real_tokens against
// the authoritative Anthropic transcript JSONL on disk, reports drift,
// and (with apply) overwrites recorded values to match ground truth.
//
// Subcommands:
//
//	report  --since 30d           print drift table for items touched in the window
//	apply   [--all|--item I-XXX]  overwrite recorded real_tokens when drift > 5%
//	verify  [--since 7d]          exit non-zero if any item has inflation_factor > 1.5×
//	migrate-strip-cost --apply    one-shot: strip ai_cost_usd / total_*_tokens / last_cost_source
//
// Algorithm per item (I-569 plan step 6):
//  1. Read time_tracking.by_session — list of (sid, project_dir, started_at, ended_at).
//  2. For each, open ~/.claude/projects/<slug-of-project_dir>/<sid>.jsonl
//     plus any matching subagents/agent-*.jsonl files.
//  3. Sum `usage` blocks across all assistant lines whose timestamp falls in
//     the recorded span. Sidechain assistants ARE included — that's the whole
//     point of reconciliation; we under-count them in real time on purpose
//     (see I-569 step 1) and pick them back up here.
//  4. Compare to time_tracking.real_tokens. Emit row.
//
// `apply` writes back via the same model.Item / store.Store path the rest of
// the codebase uses, so file format and locking stay consistent. Every write
// is appended to .as/.changelog/I-569-reconcile.log.
//
// Exit codes:
//
//	0  success
//	1  fatal error (bad flags, IO failure)
//	2  verify-mode: at least one item exceeds the inflation threshold
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "report":
		os.Exit(cmdReport(args))
	case "apply":
		os.Exit(cmdApply(args))
	case "verify":
		os.Exit(cmdVerify(args))
	case "migrate-strip-cost":
		os.Exit(cmdMigrateStripCost(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  reconcile-tokens report  [--since 30d] [--root .]
  reconcile-tokens apply   [--item I-XXX | --all] [--threshold 0.05] [--root .]
  reconcile-tokens verify  [--since 7d] [--root .]
  reconcile-tokens migrate-strip-cost --apply [--root .]
`)
}

// reconcileRow is the per-item drift row emitted by report and verify.
type reconcileRow struct {
	ItemID          string
	RecordedTokens  realTokens
	TruthTokens     realTokens
	DriftPct        float64 // |recorded - truth| / truth, in percent
	InflationFactor float64 // recorded / truth (>1 means we over-counted)
	SessionsScanned int
	JSONLsRead      int
	Notes           string
}

// realTokens mirrors internal/command/session_log_schema.go's struct so the
// reconcile binary doesn't need to depend on internal/command (which would
// pull in the whole CLI surface). Keep field names in sync.
type realTokens struct {
	Input           int
	Output          int
	CacheRead       int
	CacheCreation5m int
	CacheCreation1h int
}

func (a realTokens) sum() int {
	return a.Input + a.Output + a.CacheRead + a.CacheCreation5m + a.CacheCreation1h
}

func (a realTokens) add(b realTokens) realTokens {
	return realTokens{
		Input:           a.Input + b.Input,
		Output:          a.Output + b.Output,
		CacheRead:       a.CacheRead + b.CacheRead,
		CacheCreation5m: a.CacheCreation5m + b.CacheCreation5m,
		CacheCreation1h: a.CacheCreation1h + b.CacheCreation1h,
	}
}

// loadStore returns a store rooted at the given dir. Pulled into a helper so
// each subcommand uses the same setup path.
func loadStore(rootDir string) (*store.Store, *config.Config, error) {
	cfgPath := filepath.Join(rootDir, ".as", "config.yaml")
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		// Fall back to a minimal config — many test/sandbox roots don't have
		// .as/config.yaml. The fields we actually need (paths.root) come from
		// the rootDir override below.
		cfg = &config.Config{}
	}
	if cfg.Paths.Root == "" {
		cfg.Paths.Root = rootDir
	}
	s, err := store.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("store init: %w", err)
	}
	return s, cfg, nil
}

// Session-path resolution (projectSlug / claudeProjectsDir) was promoted
// to internal/transcript in T-353 Phase 1 so every JSONL consumer
// resolves paths identically. This binary now delegates; behavior is
// unchanged (transcript.ProjectSlug / transcript.ClaudeProjectsDir are
// the same logic, covered by internal/transcript tests).

// jsonlUsage walks one transcript file and sums Anthropic `usage` blocks
// whose top-level timestamp falls within [start, end]. Sidechain assistants
// are included — reconcile is the place where they get counted, by design.
// Lines without a parseable timestamp or without a usage block are skipped.
func jsonlUsage(path string, start, end time.Time) (realTokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return realTokens{}, err
	}
	defer f.Close()

	type usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheCreation            *struct {
			Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}
	type record struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   *struct {
			Usage *usage `json:"usage"`
		} `json:"message"`
	}

	var total realTokens
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024) // some lines are large
	for scanner.Scan() {
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message == nil || rec.Message.Usage == nil {
			continue
		}
		if rec.Timestamp != "" && (!start.IsZero() || !end.IsZero()) {
			ts, err := time.Parse(time.RFC3339, rec.Timestamp)
			if err == nil {
				if !start.IsZero() && ts.Before(start) {
					continue
				}
				if !end.IsZero() && ts.After(end) {
					continue
				}
			}
		}
		u := rec.Message.Usage
		total.Input += u.InputTokens
		total.Output += u.OutputTokens
		total.CacheRead += u.CacheReadInputTokens
		if u.CacheCreation != nil {
			total.CacheCreation5m += u.CacheCreation.Ephemeral5mInputTokens
			total.CacheCreation1h += u.CacheCreation.Ephemeral1hInputTokens
		} else {
			total.CacheCreation5m += u.CacheCreationInputTokens
		}
	}
	return total, scanner.Err()
}

// reconcileItem is the core algorithm. Returns (truth, sessions_scanned,
// jsonls_read, error).
func reconcileItem(item interface{ TimeTrackingLines() []string }, recorded realTokens) (realTokens, int, int, error) {
	sessions := parseBySessionLines(item.TimeTrackingLines())
	if len(sessions) == 0 {
		return realTokens{}, 0, 0, nil
	}
	projectsRoot := transcript.ClaudeProjectsDir()
	var truth realTokens
	jsonlsRead := 0
	for _, s := range sessions {
		slug := transcript.ProjectSlug(s.ProjectDir)
		if slug == "" {
			continue
		}
		base := filepath.Join(projectsRoot, slug)
		// Parent transcript
		parentPath := filepath.Join(base, s.SID+".jsonl")
		if pt, err := jsonlUsage(parentPath, s.StartedAt, s.EndedAt); err == nil {
			truth = truth.add(pt)
			jsonlsRead++
		}
		// Subagent transcripts (Anthropic stores these under
		// <parent_session>/subagents/agent-<id>.jsonl). I-569 step 1 sent
		// real-time provenance markers here so reconcile can find them; in
		// practice the directory walk also catches anything that didn't
		// emit a marker.
		subagentDir := filepath.Join(base, s.SID, "subagents")
		entries, err := os.ReadDir(subagentDir)
		if err == nil {
			for _, ent := range entries {
				if !strings.HasSuffix(ent.Name(), ".jsonl") {
					continue
				}
				sp := filepath.Join(subagentDir, ent.Name())
				if st, err := jsonlUsage(sp, s.StartedAt, s.EndedAt); err == nil {
					truth = truth.add(st)
					jsonlsRead++
				}
			}
		}
	}
	return truth, len(sessions), jsonlsRead, nil
}

// driftRow computes the row for an item given recorded and truth.
func driftRow(itemID string, recorded, truth realTokens, sessions, jsonls int) reconcileRow {
	row := reconcileRow{
		ItemID:          itemID,
		RecordedTokens:  recorded,
		TruthTokens:     truth,
		SessionsScanned: sessions,
		JSONLsRead:      jsonls,
	}
	rsum := recorded.sum()
	tsum := truth.sum()
	switch {
	case tsum == 0 && rsum == 0:
		row.Notes = "no data"
	case tsum == 0:
		row.Notes = "no JSONL truth (recorded=" + strconv.Itoa(rsum) + ")"
		row.InflationFactor = -1 // sentinel: undefined
	default:
		delta := rsum - tsum
		if delta < 0 {
			delta = -delta
		}
		row.DriftPct = float64(delta) / float64(tsum) * 100
		row.InflationFactor = float64(rsum) / float64(tsum)
	}
	return row
}

// Adapter: itemMetricsLineProvider lets reconcileItem read time_tracking
// list lines from a real model.Item without importing internal/command.
type itemTTLines struct {
	lines []string
}

func (x itemTTLines) TimeTrackingLines() []string { return x.lines }
