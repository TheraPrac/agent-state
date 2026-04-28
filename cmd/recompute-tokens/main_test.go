package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a fake item file containing a time_tracking block
// with N subagent ai_turns lines and rolled-up totals derived from
// summing those turns. Returns the path so tests can run processFile.
func fixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "issues")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProcessFile_DedupsByteIdenticalSubagents — the I-432 pattern:
// five subagent rows within a 4-minute span with byte-identical token
// tuples. The 60s dedup window collapses turns 1-4 (all within 56s of
// turn 1) to one; turn 5 (at +3m46s) is OUTSIDE the window from any
// surviving turn so it's kept separately. Result: 3 drops, 2 turns
// remain, totals reduced by 3× per-turn values.
func TestProcessFile_DedupsByteIdenticalSubagents(t *testing.T) {
	body := `id: I-432
type: issue
status: active

title: test

priority: 2

time_tracking:
  reg_input_tokens: 228380
  reg_output_tokens: 5254810
  cache_in_tokens: 3005583610
  cache_out_1h_tokens: 29245845
  total_input_tokens: 3035059840
  total_output_tokens: 5254810
  ai_cost_usd: 1907.760000
  turn_count: 5
  process_time_seconds: 730
  ai_turns:
  - session:agent-aaa model:claude-opus-4-7 cost:$381.552000 process:146s ai:0s reg_in:45676 reg_out:1050962 cache_in:601116722 cache_out:0 cache_out_1h:5849169 role:general-purpose step:subagent at:2026-04-27T20:08:25-06:00
  - session:agent-bbb model:claude-opus-4-7 cost:$381.552000 process:146s ai:0s reg_in:45676 reg_out:1050962 cache_in:601116722 cache_out:0 cache_out_1h:5849169 role:general-purpose step:subagent at:2026-04-27T20:08:50-06:00
  - session:agent-ccc model:claude-opus-4-7 cost:$381.552000 process:146s ai:0s reg_in:45676 reg_out:1050962 cache_in:601116722 cache_out:0 cache_out_1h:5849169 role:general-purpose step:subagent at:2026-04-27T20:09:08-06:00
  - session:agent-ddd model:claude-opus-4-7 cost:$381.552000 process:146s ai:0s reg_in:45676 reg_out:1050962 cache_in:601116722 cache_out:0 cache_out_1h:5849169 role:general-purpose step:subagent at:2026-04-27T20:09:21-06:00
  - session:agent-eee model:claude-opus-4-7 cost:$381.552000 process:146s ai:0s reg_in:45676 reg_out:1050962 cache_in:601116722 cache_out:0 cache_out_1h:5849169 role:general-purpose step:subagent at:2026-04-27T20:12:11-06:00

next_actions:
- []
`
	path := fixture(t, "I-432-test.md", body)
	rep, err := processFile(path, false /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if rep == nil {
		t.Fatal("expected dedup report, got nil")
	}
	if rep.dupsDropped != 3 {
		t.Errorf("dupsDropped = %d, want 3 (60s window: turns 2-4 dedup against turn 1; turn 5 at +226s kept separately)", rep.dupsDropped)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	gs := string(got)

	// 3× dropped → cache_in_tokens drops by 3*601116722 = 1803350166
	// new value: 3005583610 - 1803350166 = 1202233444
	if !strings.Contains(gs, "cache_in_tokens: 1202233444") {
		t.Errorf("cache_in_tokens not collapsed correctly. got:\n%s", gs)
	}
	if !strings.Contains(gs, "turn_count: 2") {
		t.Errorf("turn_count not reduced to 2. got:\n%s", gs)
	}
	// 3× $381.552000 = $1144.656000; before $1907.76 → after $763.104
	if !strings.Contains(gs, "ai_cost_usd: 763.104000") {
		t.Errorf("ai_cost_usd not adjusted. got:\n%s", gs)
	}
}

// TestProcessFile_LegitimateParallelSubagentsKept — I-441 pattern:
// 5 parallel subagents but with growing token counts (cache prefix
// grew slightly between spawns). NOT byte-identical; recompute leaves
// them alone.
func TestProcessFile_LegitimateParallelSubagentsKept(t *testing.T) {
	body := `id: I-441
type: issue
status: active

title: test

priority: 1

time_tracking:
  reg_input_tokens: 270125
  reg_output_tokens: 7113864
  cache_in_tokens: 4196698633
  cache_out_1h_tokens: 37428524
  total_input_tokens: 4204082622
  total_output_tokens: 7113864
  ai_cost_usd: 2651.831782
  turn_count: 5
  process_time_seconds: 2380
  ai_turns:
  - session:agent-aaa model:claude-opus-4-7 cost:$529.306534 process:476s ai:0s reg_in:54019 reg_out:1421105 cache_in:837347087 cache_out:0 cache_out_1h:7483527 role:general-purpose step:subagent at:2026-04-28T06:46:44-06:00
  - session:agent-bbb model:claude-opus-4-7 cost:$529.828468 process:476s ai:0s reg_in:54022 reg_out:1421780 cache_in:838340497 cache_out:0 cache_out_1h:7484361 role:general-purpose step:subagent at:2026-04-28T06:47:22-06:00
  - session:agent-ccc model:claude-opus-4-7 cost:$530.350095 process:476s ai:0s reg_in:54025 reg_out:1422254 cache_in:839337039 cache_out:0 cache_out_1h:7485510 role:general-purpose step:subagent at:2026-04-28T06:47:55-06:00
  - session:agent-ddd model:claude-opus-4-7 cost:$531.070758 process:476s ai:0s reg_in:54029 reg_out:1423986 cache_in:840670185 cache_out:0 cache_out_1h:7486587 role:general-purpose step:subagent at:2026-04-28T06:48:57-06:00
  - session:agent-eee model:claude-opus-4-7 cost:$531.275927 process:476s ai:0s reg_in:54030 reg_out:1424739 cache_in:841003825 cache_out:0 cache_out_1h:7488539 role:general-purpose step:subagent at:2026-04-28T06:52:02-06:00
`
	path := fixture(t, "I-441-test.md", body)
	rep, err := processFile(path, false /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if rep != nil {
		t.Errorf("expected no dups for non-identical tuples, got rep=%+v", rep)
	}

	// File should be unchanged.
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("file was rewritten despite no dups detected")
	}
}

// TestProcessFile_InteractiveTurnsKept — same-tuple interactive turns
// must NEVER be deduped (legitimate accumulation in unit tests, etc.).
func TestProcessFile_InteractiveTurnsKept(t *testing.T) {
	body := `id: T-001
type: task
status: active

title: test

priority: 2

time_tracking:
  ai_turns:
  - session:s1 model:claude-haiku-4-5 cost:$0.001 process:5s reg_in:100 reg_out:50 cache_in:0 cache_out:0 step:interactive at:2026-04-28T07:00:00-06:00
  - session:s1 model:claude-haiku-4-5 cost:$0.001 process:5s reg_in:100 reg_out:50 cache_in:0 cache_out:0 step:interactive at:2026-04-28T07:00:30-06:00
  - session:s1 model:claude-haiku-4-5 cost:$0.001 process:5s reg_in:100 reg_out:50 cache_in:0 cache_out:0 step:interactive at:2026-04-28T07:00:45-06:00
`
	path := fixture(t, "T-001-test.md", body)
	rep, err := processFile(path, false /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if rep != nil {
		t.Errorf("interactive turns must not be deduped, got rep=%+v", rep)
	}
}

// TestProcessFile_DryRunDoesNotWriteFile — pin the dry-run contract.
func TestProcessFile_DryRunDoesNotWriteFile(t *testing.T) {
	body := `id: I-100
type: issue
status: active

title: test

priority: 2

time_tracking:
  ai_turns:
  - session:s1 model:m cost:$1.0 reg_in:1 reg_out:2 cache_in:3 cache_out:0 cache_out_1h:0 role:r step:subagent at:2026-04-28T07:00:00-06:00
  - session:s2 model:m cost:$1.0 reg_in:1 reg_out:2 cache_in:3 cache_out:0 cache_out_1h:0 role:r step:subagent at:2026-04-28T07:00:30-06:00
`
	path := fixture(t, "I-100-test.md", body)
	rep, err := processFile(path, true /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if rep == nil || rep.dupsDropped != 1 {
		t.Errorf("expected 1 dup, got rep=%+v", rep)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("dry-run mutated file")
	}
}
