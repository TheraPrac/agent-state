package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// writeJSONLSession writes a minimal Claude Code JSONL transcript file
// with the given assistant turns under CLAUDE_PROJECTS_DIR/<slug>/<sid>.jsonl.
// Each turn includes a usage block and an optional model name.
func writeJSONLSession(t *testing.T, claudeProjectsDir, projectDir, sid string, turns []backfillTurn) string {
	t.Helper()
	slug := strings.ReplaceAll(strings.TrimPrefix(projectDir, "/"), "/", "-")
	slugDir := filepath.Join(claudeProjectsDir, "-"+slug)
	if err := os.MkdirAll(slugDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(slugDir, sid+".jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	baseTime := time.Now().Add(-time.Hour).UTC()
	enc := json.NewEncoder(f)
	for i, turn := range turns {
		// Use RFC3339Nano to match real Claude Code JSONL format (fractional seconds).
		ts := baseTime.Add(time.Duration(i)*5*time.Minute + 200*time.Millisecond).Format(time.RFC3339Nano)
		type usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}
		type message struct {
			Model string `json:"model"`
			Usage *usage `json:"usage"`
		}
		type row struct {
			Type      string   `json:"type"`
			Timestamp string   `json:"timestamp"`
			Message   *message `json:"message"`
		}
		if err := enc.Encode(row{
			Type:      "assistant",
			Timestamp: ts,
			Message: &message{
				Model: turn.Model,
				Usage: &usage{
					InputTokens:              turn.Input,
					OutputTokens:             turn.Output,
					CacheReadInputTokens:     turn.CacheRead,
					CacheCreationInputTokens: turn.CacheOut,
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

type backfillTurn struct {
	Model     string
	Input     int
	Output    int
	CacheRead int
	CacheOut  int
}

// setupBackfillEnv creates a minimal store with items for backfill tests.
// Returns store, cfg, and the temp dir used as CLAUDE_PROJECTS_DIR.
func setupBackfillEnv(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	claudeDir := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// T-010: closed, has sessions, no token data → candidate for backfill
	writeFile(t, filepath.Join(root, "tasks", "T-010-alpha.md"), `id: T-010
type: task
status: done
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-10T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: Alpha historical task

sbar:
  situation: Alpha.
  background: Alpha.
  assessment: Alpha.
  recommendation: Alpha.

sessions:
- aaaaaaaa-0000-0000-0000-000000000001
`)

	// T-011: closed, no sessions → no_session
	writeFile(t, filepath.Join(root, "tasks", "T-011-beta.md"), `id: T-011
type: task
status: done
created: 2026-01-01T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: No sessions task

sbar:
  situation: Beta.
  background: Beta.
  assessment: Beta.
  recommendation: Beta.
`)

	// T-012: closed, sessions present but JSONL won't be on disk → no_transcript
	writeFile(t, filepath.Join(root, "tasks", "T-012-gamma.md"), `id: T-012
type: task
status: done
created: 2026-01-01T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: Missing transcript task

sbar:
  situation: Gamma.
  background: Gamma.
  assessment: Gamma.
  recommendation: Gamma.

sessions:
- bbbbbbbb-0000-0000-0000-000000000002
`)

	// T-013: closed, already has ai_cost_usd → skip
	writeFile(t, filepath.Join(root, "tasks", "T-013-delta.md"), `id: T-013
type: task
status: done
created: 2026-01-01T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: Already costed task

sbar:
  situation: Delta.
  background: Delta.
  assessment: Delta.
  recommendation: Delta.

time_tracking:
  ai_cost_usd: "1.234567"
  reg_input_tokens: "5000"
`)

	// T-014: closed, already has backfill_status → skip
	writeFile(t, filepath.Join(root, "tasks", "T-014-epsilon.md"), `id: T-014
type: task
status: done
created: 2026-01-01T00:00:00Z
completed: 2026-01-10T00:00:00Z
title: Already sentineled task

sbar:
  situation: Epsilon.
  background: Epsilon.
  assessment: Epsilon.
  recommendation: Epsilon.

time_tracking:
  backfill_status: no_session
`)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg, claudeDir
}

// TestMetricsBackfillNoSessions verifies that a closed item with no sessions
// receives backfill_status = no_session and is not otherwise mutated.
func TestMetricsBackfillNoSessions(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{})
	})

	item, ok := s.Get("T-011")
	if !ok {
		t.Fatal("T-011 not found in store")
	}
	status := readStringNestedField(item, "time_tracking", "backfill_status")
	if status != "no_session" {
		t.Errorf("T-011: want backfill_status=no_session, got %q", status)
	}
}

// TestMetricsBackfillNoTranscript verifies that a closed item with sessions
// but no JSONL on disk receives backfill_status = no_transcript.
func TestMetricsBackfillNoTranscript(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{})
	})

	item, ok := s.Get("T-012")
	if !ok {
		t.Fatal("T-012 not found in store")
	}
	status := readStringNestedField(item, "time_tracking", "backfill_status")
	if status != "no_transcript" {
		t.Errorf("T-012: want backfill_status=no_transcript, got %q", status)
	}
}

// TestMetricsBackfillDryRun verifies that --dry-run does not mutate any items.
func TestMetricsBackfillDryRun(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)

	projectDir := "/Users/agent/dev/test-project"
	sid := "aaaaaaaa-0000-0000-0000-000000000001"
	writeJSONLSession(t, claudeDir, projectDir, sid, []backfillTurn{
		{Model: "claude-sonnet-4-5", Input: 1000, Output: 500, CacheRead: 200, CacheOut: 50},
	})
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{DryRun: true})
	})

	item, ok := s.Get("T-010")
	if !ok {
		t.Fatal("T-010 not found")
	}
	// Must NOT have been mutated in dry-run.
	if status := readStringNestedField(item, "time_tracking", "backfill_status"); status != "" {
		t.Errorf("dry-run: T-010 backfill_status should be empty, got %q", status)
	}
	if c := readFloatField(item, "time_tracking", "ai_cost_usd"); c != 0 {
		t.Errorf("dry-run: T-010 ai_cost_usd should be 0, got %v", c)
	}
}

// TestMetricsBackfillWrites verifies that a closed item with a valid JSONL
// transcript gets token/cost/turn_count/duration written back correctly.
func TestMetricsBackfillWrites(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)

	projectDir := "/Users/agent/dev/test-project"
	sid := "aaaaaaaa-0000-0000-0000-000000000001"
	turns := []backfillTurn{
		{Model: "claude-sonnet-4-5", Input: 1000, Output: 500, CacheRead: 200, CacheOut: 50},
		{Model: "claude-sonnet-4-5", Input: 800, Output: 400, CacheRead: 100, CacheOut: 30},
		{Model: "claude-sonnet-4-5", Input: 600, Output: 300, CacheRead: 50, CacheOut: 10},
	}
	writeJSONLSession(t, claudeDir, projectDir, sid, turns)
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{})
	})

	item, ok := s.Get("T-010")
	if !ok {
		t.Fatal("T-010 not found")
	}

	// Verify token counts (sums of all 3 turns).
	wantInput := 1000 + 800 + 600
	gotInput := readIntField(item, "time_tracking", "reg_input_tokens")
	if gotInput != wantInput {
		t.Errorf("reg_input_tokens: want %d, got %d", wantInput, gotInput)
	}

	wantOutput := 500 + 400 + 300
	gotOutput := readIntField(item, "time_tracking", "reg_output_tokens")
	if gotOutput != wantOutput {
		t.Errorf("reg_output_tokens: want %d, got %d", wantOutput, gotOutput)
	}

	// Verify turn count.
	if got := readIntField(item, "time_tracking", "turn_count"); got != 3 {
		t.Errorf("turn_count: want 3, got %d", got)
	}

	// Cost must be positive.
	if c := readFloatField(item, "time_tracking", "ai_cost_usd"); c <= 0 {
		t.Errorf("ai_cost_usd: want >0, got %v", c)
	}

	// Duration must be positive (3 turns × 5-minute gap → ≥ 10 minutes).
	if d := readIntField(item, "time_tracking", "process_time_seconds"); d < 60 {
		t.Errorf("process_time_seconds: want >=60, got %d", d)
	}

	// backfill_status must be "done".
	if status := readStringNestedField(item, "time_tracking", "backfill_status"); status != "done" {
		t.Errorf("backfill_status: want done, got %q", status)
	}
}

// TestMetricsBackfillSkipsAlreadyCosted verifies that an item with existing
// ai_cost_usd is not touched by backfill.
func TestMetricsBackfillSkipsAlreadyCosted(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{})
	})

	item, ok := s.Get("T-013")
	if !ok {
		t.Fatal("T-013 not found")
	}
	// Cost must still be the original value (not overwritten).
	if c := readFloatField(item, "time_tracking", "ai_cost_usd"); fmt.Sprintf("%.6f", c) != "1.234567" {
		t.Errorf("T-013 ai_cost_usd should be unchanged (1.234567), got %.6f", c)
	}
	// No backfill_status should be added.
	if status := readStringNestedField(item, "time_tracking", "backfill_status"); status != "" {
		t.Errorf("T-013 should not have backfill_status set, got %q", status)
	}
}

// TestMetricsBackfillSkipsStatusSet verifies that an item already bearing a
// backfill_status sentinel is not re-processed.
func TestMetricsBackfillSkipsStatusSet(t *testing.T) {
	s, cfg, claudeDir := setupBackfillEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	captureStdout(t, func() {
		MetricsBackfill(s, cfg, MetricsBackfillOpts{})
	})

	item, ok := s.Get("T-014")
	if !ok {
		t.Fatal("T-014 not found")
	}
	// The existing sentinel must be preserved unchanged.
	if status := readStringNestedField(item, "time_tracking", "backfill_status"); status != "no_session" {
		t.Errorf("T-014 backfill_status: want no_session (original), got %q", status)
	}
}
