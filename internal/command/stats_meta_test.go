package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/testutil"
)

// writeOrphan appends a JSON line to .as/sessions/orphan.log mimicking
// what session_log.go::writeOrphanLog would produce.
func writeOrphan(t *testing.T, dir, agent, at string, payload SessionLogPayload) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	rec := orphanLogEntry{
		At:      at,
		AgentID: agent,
		Reason:  "no_item_on_stack_or_item_missing",
		Payload: payload,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "orphan.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// --- Tests ---

func TestStatsMeta_NoOrphanLogIsNotError(t *testing.T) {
	env := testutil.NewEnv(t)
	out := captureStdout(t, func() {
		code := StatsMeta(env.Cfg, StatsMetaOpts{})
		if code != 0 {
			t.Fatalf("StatsMeta with missing log returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "no meta-work recorded") {
		t.Errorf("expected 'no meta-work recorded' in output, got: %s", out)
	}
}

func TestStatsMeta_NoOrphanLogJSON(t *testing.T) {
	env := testutil.NewEnv(t)
	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{JSON: true})
	})
	var r metaReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("missing-log JSON should parse, got err=%v out=%s", err, out)
	}
	if len(r.Rows) != 0 {
		t.Errorf("expected 0 rows on empty log, got %d", len(r.Rows))
	}
}

func TestStatsMeta_GroupsByAgent(t *testing.T) {
	env := testutil.NewEnv(t)
	now := time.Now().UTC()
	dir := env.Cfg.SessionsDir()

	writeOrphan(t, dir, "agent-a", now.Add(-2*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-opus-4-7", ProcessMs: 60_000, RegInputTokens: 1000, RegOutputTokens: 500, CostUSD: 0.50, CostSource: CostSourceProvided,
	})
	writeOrphan(t, dir, "agent-a", now.Add(-1*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-haiku-4-5", ProcessMs: 30_000, RegInputTokens: 500, RegOutputTokens: 200, CostUSD: 0.10, CostSource: CostSourceProvided,
	})
	writeOrphan(t, dir, "agent-b", now.Add(-30*time.Minute).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-haiku-4-5", ProcessMs: 45_000, RegInputTokens: 800, RegOutputTokens: 300, CostUSD: 0.20, CostSource: CostSourceProvided,
	})

	out := captureStdout(t, func() {
		code := StatsMeta(env.Cfg, StatsMetaOpts{JSON: true})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
	})
	var r metaReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v\nout: %s", err, out)
	}

	if r.GroupBy != "agent" {
		t.Errorf("group_by = %q, want agent", r.GroupBy)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (agent-a, agent-b)", len(r.Rows))
	}
	rowsByKey := map[string]metaRow{}
	for _, row := range r.Rows {
		rowsByKey[row.Key] = row
	}
	a := rowsByKey["agent-a"]
	if a.Turns != 2 || a.ProcessSeconds != 90 || a.CostUSD < 0.59 || a.CostUSD > 0.61 {
		t.Errorf("agent-a row = %+v, want turns=2 process=90 cost~0.60", a)
	}
	b := rowsByKey["agent-b"]
	if b.Turns != 1 || b.ProcessSeconds != 45 {
		t.Errorf("agent-b row = %+v, want turns=1 process=45", b)
	}

	// Total spans both agents.
	if r.Total.Turns != 3 || r.Total.ProcessSeconds != 135 {
		t.Errorf("total = %+v, want turns=3 process=135", r.Total)
	}
}

func TestStatsMeta_AgentFilter(t *testing.T) {
	env := testutil.NewEnv(t)
	now := time.Now().UTC()
	dir := env.Cfg.SessionsDir()

	writeOrphan(t, dir, "agent-a", now.Add(-1*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-opus-4-7", ProcessMs: 60_000, RegInputTokens: 100, CostUSD: 0.10, CostSource: CostSourceProvided,
	})
	writeOrphan(t, dir, "agent-b", now.Add(-1*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-opus-4-7", ProcessMs: 30_000, RegInputTokens: 200, CostUSD: 0.05, CostSource: CostSourceProvided,
	})

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{Agent: "agent-b", JSON: true})
	})
	var r metaReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Rows) != 1 || r.Rows[0].Key != "agent-b" {
		t.Errorf("expected single agent-b row, got %+v", r.Rows)
	}
	if r.AgentFilter != "agent-b" {
		t.Errorf("agent_filter = %q, want agent-b", r.AgentFilter)
	}
}

func TestStatsMeta_SinceWindow(t *testing.T) {
	env := testutil.NewEnv(t)
	now := time.Now().UTC()
	dir := env.Cfg.SessionsDir()

	// One in-window, one out-of-window (3 days ago).
	writeOrphan(t, dir, "agent-a", now.Add(-1*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-opus-4-7", ProcessMs: 60_000, RegInputTokens: 100, CostUSD: 0.50, CostSource: CostSourceProvided,
	})
	writeOrphan(t, dir, "agent-a", now.Add(-72*time.Hour).Format(time.RFC3339), SessionLogPayload{
		Model: "claude-opus-4-7", ProcessMs: 99_000, RegInputTokens: 999, CostUSD: 99.99, CostSource: CostSourceProvided,
	})

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{Since: "24h", JSON: true})
	})
	var r metaReport
	json.Unmarshal([]byte(out), &r)
	if r.Total.Turns != 1 {
		t.Errorf("--since 24h should drop 72h-old entry; got turns=%d", r.Total.Turns)
	}
	// Days form: 7d should include both.
	out = captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{Since: "7d", JSON: true})
	})
	json.Unmarshal([]byte(out), &r)
	if r.Total.Turns != 2 {
		t.Errorf("--since 7d should include both; got turns=%d", r.Total.Turns)
	}
}

func TestStatsMeta_GroupByReason(t *testing.T) {
	env := testutil.NewEnv(t)
	dir := env.Cfg.SessionsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Hand-craft two records with different reasons (writeOrphan uses one).
	now := time.Now().UTC().Format(time.RFC3339)
	for _, rec := range []orphanLogEntry{
		{At: now, AgentID: "agent-a", Reason: "no_item_on_stack_or_item_missing", Payload: SessionLogPayload{Model: "x", ProcessMs: 10_000, CostUSD: 0.05, CostSource: CostSourceProvided}},
		{At: now, AgentID: "agent-a", Reason: "between_items_deliberation", Payload: SessionLogPayload{Model: "x", ProcessMs: 20_000, CostUSD: 0.10, CostSource: CostSourceProvided}},
	} {
		b, _ := json.Marshal(rec)
		f, _ := os.OpenFile(filepath.Join(dir, "orphan.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		f.Write(append(b, '\n'))
		f.Close()
	}

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{By: "reason", JSON: true})
	})
	var r metaReport
	json.Unmarshal([]byte(out), &r)
	if r.GroupBy != "reason" {
		t.Errorf("group_by = %q, want reason", r.GroupBy)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("expected 2 reason buckets, got %d", len(r.Rows))
	}
}

func TestStatsMeta_InvalidByRejected(t *testing.T) {
	env := testutil.NewEnv(t)
	if code := StatsMeta(env.Cfg, StatsMetaOpts{By: "weather"}); code != 2 {
		t.Errorf("--by weather should exit 2, got %d", code)
	}
}

func TestStatsMeta_InvalidSinceRejected(t *testing.T) {
	env := testutil.NewEnv(t)
	if code := StatsMeta(env.Cfg, StatsMetaOpts{Since: "huh?"}); code != 2 {
		t.Errorf("--since huh? should exit 2, got %d", code)
	}
}

func TestStatsMeta_SelfFilterResolvesAgentID(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-test")
	env := testutil.NewEnv(t)
	dir := env.Cfg.SessionsDir()
	now := time.Now().UTC().Format(time.RFC3339)
	writeOrphan(t, dir, "agent-test", now, SessionLogPayload{Model: "x", ProcessMs: 5_000, CostUSD: 0.01, CostSource: CostSourceProvided})
	writeOrphan(t, dir, "agent-other", now, SessionLogPayload{Model: "x", ProcessMs: 10_000, CostUSD: 0.02, CostSource: CostSourceProvided})

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{Agent: "self", JSON: true})
	})
	var r metaReport
	json.Unmarshal([]byte(out), &r)
	if r.AgentFilter != "agent-test" {
		t.Errorf("self should resolve to agent-test, got %q", r.AgentFilter)
	}
	if len(r.Rows) != 1 || r.Rows[0].Key != "agent-test" {
		t.Errorf("expected only agent-test row, got %+v", r.Rows)
	}
}

func TestStatsMeta_MalformedLineSkipped(t *testing.T) {
	env := testutil.NewEnv(t)
	dir := env.Cfg.SessionsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "orphan.log")
	// One valid, one garbage, one valid.
	now := time.Now().UTC().Format(time.RFC3339)
	rec := orphanLogEntry{At: now, AgentID: "agent-a", Reason: "x", Payload: SessionLogPayload{Model: "x", ProcessMs: 1000, CostUSD: 0.01, CostSource: CostSourceProvided}}
	b, _ := json.Marshal(rec)
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	f.Write(append(b, '\n'))
	f.WriteString("not-json-at-all\n")
	f.Write(append(b, '\n'))
	f.Close()

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{JSON: true})
	})
	var r metaReport
	json.Unmarshal([]byte(out), &r)
	if r.Total.Turns != 2 {
		t.Errorf("malformed line should be skipped, leaving 2 valid; got turns=%d", r.Total.Turns)
	}
}

// Regression for /code-review finding: parseDurationFlexible used to
// reject mixed expressions like "1d12h" because the d-suffix check was
// all-or-nothing. After the fix, "1d12h" parses as 36h and "2d3h30m"
// parses as 51h30m.
func TestParseDurationFlexible_MixedUnits(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"1d12h", 36 * time.Hour},
		{"2d3h30m", 51*time.Hour + 30*time.Minute},
		{"0d", 0},
	}
	for _, c := range cases {
		got, err := parseDurationFlexible(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %v, want %v", c.in, got, c.want)
		}
	}
	// Negative cases stay rejected.
	for _, bad := range []string{"huh?", "1week", "-3d"} {
		if _, err := parseDurationFlexible(bad); err == nil {
			t.Errorf("%q should have errored", bad)
		}
	}
}

// Regression for /code-review finding: emptyReport used to hardcode
// GroupBy:"agent" regardless of opts.By. After the fix, --by reason
// --json on a missing log returns group_by:"reason".
func TestStatsMeta_EmptyReportPreservesGroupBy(t *testing.T) {
	env := testutil.NewEnv(t)
	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{By: "reason", JSON: true})
	})
	var r metaReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.GroupBy != "reason" {
		t.Errorf("group_by on empty log with --by reason = %q, want 'reason'", r.GroupBy)
	}
}

// Regression: log exists but --since window drops every entry — should
// behave like an empty log (no error, "no meta-work recorded" text).
func TestStatsMeta_AllEntriesFilteredOut(t *testing.T) {
	env := testutil.NewEnv(t)
	dir := env.Cfg.SessionsDir()
	// One ancient entry well outside any normal window.
	writeOrphan(t, dir, "agent-a",
		time.Now().Add(-365*24*time.Hour).UTC().Format(time.RFC3339),
		SessionLogPayload{Model: "x", ProcessMs: 1000, CostUSD: 0.01, CostSource: CostSourceProvided})

	out := captureStdout(t, func() {
		code := StatsMeta(env.Cfg, StatsMetaOpts{Since: "1d"})
		if code != 0 {
			t.Fatalf("exit=%d on filtered-empty, want 0", code)
		}
	})
	if !strings.Contains(out, "no meta-work recorded") {
		t.Errorf("expected 'no meta-work recorded' when all filtered, got: %s", out)
	}
}

func TestStatsMeta_TextRendersHeaderAndRows(t *testing.T) {
	env := testutil.NewEnv(t)
	dir := env.Cfg.SessionsDir()
	now := time.Now().UTC().Format(time.RFC3339)
	writeOrphan(t, dir, "agent-a", now, SessionLogPayload{Model: "x", ProcessMs: 600_000, CostUSD: 1.50, CostSource: CostSourceProvided})

	out := captureStdout(t, func() {
		StatsMeta(env.Cfg, StatsMetaOpts{Since: "7d"})
	})
	for _, want := range []string{"Meta-work", "last 7d", "by agent", "agent-a", "$1.50", "1 turns"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q. Got:\n%s", want, out)
		}
	}
}

