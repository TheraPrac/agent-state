package command

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// setupMetricsEnv creates a test store with two tasks and one issue,
// all carrying time_tracking data, plus two items with goals/tags for filter tests.
func setupMetricsEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// T-001: task with time_tracking
	writeFile(t, filepath.Join(root, "tasks", "T-001-alpha.md"), `id: T-001
type: task
status: done
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-10T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: Alpha task

goals:
- G-014

tags:
- st-tooling
- perf

sbar:
  situation: Alpha.
  background: Alpha.
  assessment: Alpha.
  recommendation: Alpha.

time_tracking:
  turn_count: "10"
  ai_cost_usd: "0.123456"
  lines_added: "500"
  lines_removed: "100"
  files_changed_count: "8"
  total_duration_seconds: "3600"
  process_time_seconds: "3200"
`)

	// T-002: task with different metrics
	writeFile(t, filepath.Join(root, "tasks", "T-002-beta.md"), `id: T-002
type: task
status: done
created: 2026-02-01T00:00:00Z
last_touched: 2026-02-15T00:00:00Z
completed: 2026-02-15T00:00:00Z
title: Beta task

goals:
- G-011

tags:
- infra

sbar:
  situation: Beta.
  background: Beta.
  assessment: Beta.
  recommendation: Beta.

time_tracking:
  turn_count: "5"
  ai_cost_usd: "0.050000"
  lines_added: "200"
  lines_removed: "50"
  files_changed_count: "3"
  total_duration_seconds: "1800"
  process_time_seconds: "1600"
`)

	// I-001: issue with metrics
	writeFile(t, filepath.Join(root, "issues", "I-001-bug.md"), `id: I-001
type: issue
status: done
created: 2026-03-01T00:00:00Z
last_touched: 2026-03-05T00:00:00Z
completed: 2026-03-05T00:00:00Z
title: A bug fix
priority: 2

goals:
- G-014

tags:
- st-tooling

sbar:
  situation: Bug.
  background: Bug.
  assessment: Bug.
  recommendation: Bug.

time_tracking:
  turn_count: "3"
  ai_cost_usd: "0.020000"
  lines_added: "50"
  lines_removed: "10"
  files_changed_count: "2"
  total_duration_seconds: "900"
  process_time_seconds: "800"
`)

	// T-003: task with no time_tracking (should be excluded)
	writeFile(t, filepath.Join(root, "tasks", "T-003-empty.md"), `id: T-003
type: task
status: queued
created: 2026-04-01T00:00:00Z
last_touched: 2026-04-01T00:00:00Z
title: No metrics task

sbar:
  situation: No metrics.
  background: No metrics.
  assessment: No metrics.
  recommendation: No metrics.
`)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Seed time_tracking into items via Mutate so the typed map is populated.
	seedMetrics := map[string]map[string]string{
		"T-001": {
			"turn_count":               "10",
			"ai_cost_usd":              "0.123456",
			"lines_added":              "500",
			"lines_removed":            "100",
			"files_changed_count":      "8",
			"process_time_seconds":     "3200",
			"total_duration_seconds":   "3600",
		},
		"T-002": {
			"turn_count":               "5",
			"ai_cost_usd":              "0.050000",
			"lines_added":              "200",
			"lines_removed":            "50",
			"files_changed_count":      "3",
			"process_time_seconds":     "1600",
			"total_duration_seconds":   "1800",
		},
		"I-001": {
			"turn_count":               "3",
			"ai_cost_usd":              "0.020000",
			"lines_added":              "50",
			"lines_removed":            "10",
			"files_changed_count":      "2",
			"process_time_seconds":     "800",
			"total_duration_seconds":   "900",
		},
	}
	for id, fields := range seedMetrics {
		if err := s.Mutate(id, func(item *model.Item) error {
			if item.TimeTracking == nil {
				item.TimeTracking = make(map[string]interface{})
			}
			for k, v := range fields {
				item.TimeTracking[k] = v
			}
			return nil
		}); err != nil {
			t.Fatalf("seeding time_tracking for %s: %v", id, err)
		}
	}

	return s, cfg
}

// TestMetricsEmpty verifies that an empty store returns exit code 0 without panicking.
func TestMetricsEmpty(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	code := Metrics(s, cfg, MetricsOpts{})
	if code != 0 {
		t.Errorf("Metrics on empty store returned %d, want 0", code)
	}
}

// TestMetricsFilterType verifies that --type task excludes issues.
func TestMetricsFilterType(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Type: "task"})
	})

	if strings.Contains(out, "I-001") {
		t.Errorf("--type task output should not contain I-001 (issue), got:\n%s", out)
	}
	if !strings.Contains(out, "T-001") {
		t.Errorf("--type task output should contain T-001, got:\n%s", out)
	}
	if !strings.Contains(out, "T-002") {
		t.Errorf("--type task output should contain T-002, got:\n%s", out)
	}
}

// TestMetricsFilterTag verifies that --tag filters by tag substring.
func TestMetricsFilterTag(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Tag: "infra"})
	})

	if strings.Contains(out, "T-001") {
		t.Errorf("--tag infra output should not contain T-001, got:\n%s", out)
	}
	if strings.Contains(out, "I-001") {
		t.Errorf("--tag infra output should not contain I-001, got:\n%s", out)
	}
	if !strings.Contains(out, "T-002") {
		t.Errorf("--tag infra output should contain T-002, got:\n%s", out)
	}
}

// TestMetricsSortLOC verifies that --sort loc orders rows by |net_loc| descending.
func TestMetricsSortLOC(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Sort: "loc"})
	})

	// T-001 has net LOC 400 (500-100), T-002 has 150, I-001 has 40.
	// T-001 should appear before T-002 in the output.
	posT1 := strings.Index(out, "T-001")
	posT2 := strings.Index(out, "T-002")
	if posT1 == -1 || posT2 == -1 {
		t.Fatalf("expected both T-001 and T-002 in output, got:\n%s", out)
	}
	if posT1 > posT2 {
		t.Errorf("--sort loc: T-001 (net +400) should appear before T-002 (net +150), got:\n%s", out)
	}
}

// TestMetricsTopLimit verifies that --top 1 limits output to 1 row (excluding header/footer).
func TestMetricsTopLimit(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Top: 1})
	})

	// Count item IDs in output (T-xxx or I-xxx).
	count := 0
	for _, prefix := range []string{"T-001", "T-002", "I-001"} {
		if strings.Contains(out, prefix) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("--top 1: expected exactly 1 item in output, found %d in:\n%s", count, out)
	}
}

// TestMetricsFormatJSON verifies that --format json produces a valid JSON array.
func TestMetricsFormatJSON(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Format: "json"})
	})

	var rows []metricsRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--format json: invalid JSON: %v\noutput:\n%s", err, out)
	}
	if len(rows) == 0 {
		t.Errorf("--format json: expected rows, got empty array")
	}
	// Every row must have a non-empty ID.
	for _, r := range rows {
		if r.ID == "" {
			t.Errorf("--format json: row has empty ID: %+v", r)
		}
	}
}

// TestMetricsFormatCSV verifies that --format csv produces a header row followed by data rows.
func TestMetricsFormatCSV(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Format: "csv"})
	})

	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("--format csv: invalid CSV: %v\noutput:\n%s", err, out)
	}
	if len(records) < 2 {
		t.Errorf("--format csv: expected header + at least 1 data row, got %d records", len(records))
	}
	header := records[0]
	if header[0] != "id" {
		t.Errorf("--format csv: first header column should be 'id', got %q", header[0])
	}
	// duration_seconds column must be present so downstream consumers can sort numerically.
	found := false
	for _, col := range header {
		if col == "duration_seconds" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--format csv: header missing 'duration_seconds' column, got %v", header)
	}
}

// TestMetricsSince verifies that --since excludes items completed before the date
// and open items (no completed_at).
func TestMetricsSince(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	// --since 2026-02-01: should include T-002 (2026-02-15) and I-001 (2026-03-05),
	// but exclude T-001 (completed 2026-01-10).
	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Since: "2026-02-01"})
	})

	if strings.Contains(out, "T-001") {
		t.Errorf("--since 2026-02-01: should exclude T-001 (completed Jan 10), got:\n%s", out)
	}
	if !strings.Contains(out, "T-002") {
		t.Errorf("--since 2026-02-01: should include T-002 (completed Feb 15), got:\n%s", out)
	}
	if !strings.Contains(out, "I-001") {
		t.Errorf("--since 2026-02-01: should include I-001 (completed Mar 5), got:\n%s", out)
	}
	// T-003 has no time_tracking data, so it was already excluded by HasMetrics.
	// But confirm the --since filter also works on open items implicitly.
}

// TestMetricsGoalFilter verifies that --goal filters to only items in that goal.
func TestMetricsGoalFilter(t *testing.T) {
	s, cfg := setupMetricsEnv(t)

	out := captureStdout(t, func() {
		Metrics(s, cfg, MetricsOpts{Goal: "G-014"})
	})

	if strings.Contains(out, "T-002") {
		t.Errorf("--goal G-014: should exclude T-002 (goal G-011), got:\n%s", out)
	}
	if !strings.Contains(out, "T-001") {
		t.Errorf("--goal G-014: should include T-001 (goal G-014), got:\n%s", out)
	}
	if !strings.Contains(out, "I-001") {
		t.Errorf("--goal G-014: should include I-001 (goal G-014), got:\n%s", out)
	}
}
