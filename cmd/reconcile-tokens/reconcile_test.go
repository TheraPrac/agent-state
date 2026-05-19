package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// I-569 finding-8: the realTokens struct is duplicated between this binary
// and internal/command/session_log_schema.go to keep the binary's
// dependency surface small. There's no compile-time check tying the two
// definitions together. This test pins the local field set so a future
// addition (e.g. a sixth token class) at least fails here, prompting the
// author to update both definitions in lockstep.
func TestRealTokens_FieldSetMatchesInternalSchema(t *testing.T) {
	want := []string{"Input", "Output", "CacheRead", "CacheCreation5m", "CacheCreation1h"}
	rt := realTokens{}
	rv := reflect.TypeOf(rt)
	if rv.NumField() != len(want) {
		t.Fatalf("realTokens has %d fields, want %d (sync with internal/command/session_log_schema.go)", rv.NumField(), len(want))
	}
	for i, name := range want {
		if got := rv.Field(i).Name; got != name {
			t.Errorf("field %d = %q, want %q (out of sync with internal/command/session_log_schema.go)", i, got, name)
		}
	}
}

// TestProjectSlug moved to internal/transcript (resolve_test.go) in
// T-353 Phase 1 — the slug logic was promoted out of this binary, so its
// canonical test lives with it.

func TestParseBySessionLines(t *testing.T) {
	lines := []string{
		"- sid=abc-123 project_dir=/proj/a started_at=2026-05-10T10:00:00-06:00 ended_at=2026-05-10T10:30:00-06:00 turns=5 input=100 output=20 cache_read=5000 cache_creation_5m=10 cache_creation_1h=2",
		"- sid=def-456 project_dir=/proj/b started_at=2026-05-10T11:00:00-06:00 ended_at=2026-05-10T11:05:00-06:00 turns=1 input=10 output=5 cache_read=0 cache_creation_5m=0 cache_creation_1h=0",
	}
	out := parseBySessionLines(lines)
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if out[0].SID != "abc-123" {
		t.Errorf("sid = %q, want abc-123", out[0].SID)
	}
	if out[0].ProjectDir != "/proj/a" {
		t.Errorf("project_dir = %q", out[0].ProjectDir)
	}
	if out[0].Tokens.Input != 100 || out[0].Tokens.CacheRead != 5000 {
		t.Errorf("tokens parsed wrong: %+v", out[0].Tokens)
	}
	if out[0].StartedAt.IsZero() {
		t.Error("started_at not parsed")
	}
}

// Reconciles a fixture transcript and asserts the truth tokens match the
// per-line `usage` blocks. Sidechain assistants must be included.
func TestJSONLUsage(t *testing.T) {
	tmp := t.TempDir()
	tp := filepath.Join(tmp, "session.jsonl")
	content := `{"type":"user","message":{"content":"hi"},"timestamp":"2026-05-10T10:00:00Z"}
{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}},"timestamp":"2026-05-10T10:00:05Z","isSidechain":false}
{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":50,"ephemeral_1h_input_tokens":20}}},"timestamp":"2026-05-10T10:00:06Z","isSidechain":true}
`
	if err := os.WriteFile(tp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := jsonlUsage(tp, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("jsonlUsage: %v", err)
	}
	want := realTokens{
		Input:           110,  // 100 + 10 (sidechain included — that's the point)
		Output:          55,   // 50 + 5
		CacheRead:       6000, // 5000 + 1000
		CacheCreation5m: 250,  // 200 (top-level fallback) + 50 (split)
		CacheCreation1h: 20,
	}
	if got != want {
		t.Errorf("jsonlUsage = %+v, want %+v", got, want)
	}
}

// Span filter: lines outside [start, end] must be skipped.
func TestJSONLUsage_SpanFilter(t *testing.T) {
	tmp := t.TempDir()
	tp := filepath.Join(tmp, "session.jsonl")
	content := `{"type":"assistant","message":{"usage":{"input_tokens":100}},"timestamp":"2026-05-10T09:00:00Z"}
{"type":"assistant","message":{"usage":{"input_tokens":200}},"timestamp":"2026-05-10T10:00:00Z"}
{"type":"assistant","message":{"usage":{"input_tokens":400}},"timestamp":"2026-05-10T11:00:00Z"}
`
	os.WriteFile(tp, []byte(content), 0644)

	start, _ := time.Parse(time.RFC3339, "2026-05-10T09:30:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-05-10T10:30:00Z")
	got, err := jsonlUsage(tp, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if got.Input != 200 {
		t.Errorf("span-filtered input = %d, want 200 (only the middle line)", got.Input)
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"", 0},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDriftRow_InflationFactor(t *testing.T) {
	r := driftRow("I-001",
		realTokens{Input: 100, CacheRead: 900},   // recorded sum = 1000
		realTokens{Input: 100, CacheRead: 100},   // truth sum = 200
		1, 1)
	want := 1000.0 / 200.0
	if r.InflationFactor != want {
		t.Errorf("inflation_factor = %.2f, want %.2f", r.InflationFactor, want)
	}
	if r.DriftPct != 400 { // |1000-200|/200 = 400%
		t.Errorf("drift = %.1f, want 400", r.DriftPct)
	}
}
