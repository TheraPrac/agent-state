package migrate

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/parse"
)

// TestCanonical_RewritesLegacyTimeTrackingFields verifies that canonical
// re-emission renames the old st run field names to the new SessionLog
// schema and drops the ambiguous total_tokens field.
func TestCanonical_RewritesLegacyTimeTrackingFields(t *testing.T) {
	src := `id: T-100
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Legacy metrics item

depends_on:
- []

next_actions:
- []

time_tracking:
  run_wall_seconds: 450
  ai_duration_seconds: 300
  run_count: 5
  input_tokens: 12000
  output_tokens: 3000
  total_tokens: 15000
  ai_cost_usd: 1.2345

work_tracking:
  ai_sessions:
    - cost:$0.1234 duration:42s in:100 out:50 step:implement at:2026-03-25T10:00:00-06:00
    - cost:$0.2345 duration:90s in:200 out:80 step:code_review at:2026-03-25T11:00:00-06:00
`

	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-100.md", src)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	out := Canonical(item, testConfig())

	// New time_tracking field names present with preserved values
	for _, want := range []string{
		"process_time_seconds: 450",
		"ai_time_seconds: 300",
		"turn_count: 5",
		"reg_input_tokens: 12000",
		"reg_output_tokens: 3000",
		"ai_cost_usd: 1.2345",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}

	// Legacy fields absent
	for _, stale := range []string{
		"run_wall_seconds:",
		"ai_duration_seconds:",
		"run_count:",
		"total_tokens:",
		"ai_sessions:",
	} {
		if strings.Contains(out, stale) {
			t.Errorf("legacy key %q should have been removed:\n%s", stale, out)
		}
	}

	// input_tokens: appears as a substring of reg_input_tokens — assert the
	// standalone legacy form is gone by looking for a line that starts
	// exactly with two spaces + input_tokens:
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  input_tokens:") {
			t.Errorf("legacy 'input_tokens:' line should be renamed, saw:\n%s", line)
		}
		if strings.HasPrefix(line, "  output_tokens:") {
			t.Errorf("legacy 'output_tokens:' line should be renamed, saw:\n%s", line)
		}
	}

	// NOTE: Legacy ai_sessions in work_tracking is NOT preserved — the
	// canonical work_tracking emitter strips unknown nested keys (pre-existing
	// behavior, not a regression). New items write ai_turns under time_tracking
	// where emitRaw preserves all nested content.
}

// TestCanonical_MetricsMigrationIsIdempotent verifies that running migration
// twice on the same file produces no further changes.
func TestCanonical_MetricsMigrationIsIdempotent(t *testing.T) {
	src := `id: T-100
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Already migrated

depends_on:
- []

next_actions:
- []

time_tracking:
  process_time_seconds: 120
  ai_time_seconds: 100
  turn_count: 3
  reg_input_tokens: 5000
  reg_output_tokens: 1500
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-100.md", src)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	first := Canonical(item, testConfig())
	// Re-parse and re-emit — write first output to a new file.
	path2 := writeTestFile(t, dir, "T-100-pass2.md", first)
	item2, err := parse.File(path2)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	second := Canonical(item2, testConfig())

	if first != second {
		t.Errorf("idempotent migration failed — first run != second run\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestDetectChanges_FlagsLegacyMetricRename verifies that detectChanges
// surfaces the rename for operator visibility.
func TestDetectChanges_FlagsLegacyMetricRename(t *testing.T) {
	src := `id: T-100
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Legacy

depends_on:
- []

next_actions:
- []

time_tracking:
  run_wall_seconds: 100
  total_tokens: 500
`
	dir := t.TempDir()
	path := writeTestFile(t, dir, "T-100.md", src)
	item, err := parse.File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	changes := detectChanges(item, testConfig())
	var sawRename, sawDrop bool
	for _, c := range changes {
		if c.Type == "rename_metric_field" {
			sawRename = true
		}
		if c.Type == "drop_field" && strings.Contains(c.Detail, "total_tokens") {
			sawDrop = true
		}
	}
	if !sawRename {
		t.Errorf("expected rename_metric_field change; got %+v", changes)
	}
	if !sawDrop {
		t.Errorf("expected drop_field change for total_tokens; got %+v", changes)
	}
}
