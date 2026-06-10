package parse

import (
	"strings"
	"testing"
)

// I-1382: a stray indented `---` inside a nested block must not be treated
// as the frontmatter→body separator. I-807/T-433 carried one inside
// time_tracking, which made the parser swallow the rest of the frontmatter
// (including the duration fields) as opaque body lines — invisible to
// getNestedField and st timer scrub while still matched by raw-text regexes.
func TestParseStrayIndentedSeparatorInsideNestedBlock(t *testing.T) {
	content := `id: I-807
type: issue
status: done
created: 2026-05-23T10:00:00-06:00
title: stray separator repro

time_tracking:
  completed_at: 2026-05-23T18:41:25-06:00
    ---
  total_duration_seconds: 17091
  work_duration_seconds: 17091
  wall_time_hours: 4.7

time_tracking:
  last_session: f87fb69b-a8c8-40b9-828e-4530a55fc5e4
  last_step: plan_review_approve

tags:
- st-tooling
---
markdown body starts here
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// All nested fields after the stray separator must be visible.
	for key, want := range map[string]string{
		"completed_at":           "2026-05-23T18:41:25-06:00",
		"total_duration_seconds": "17091",
		"work_duration_seconds":  "17091",
		"last_step":              "plan_review_approve",
	} {
		got, ok := item.TimeTracking[key].(string)
		if !ok || got != want {
			t.Errorf("time_tracking.%s = %q (ok=%v), want %q", key, got, ok, want)
		}
	}

	// Fields after the REAL (indent-0) separator must still be body, and
	// the stray line must round-trip verbatim.
	out := item.Doc.String()
	if !strings.Contains(out, "    ---") {
		t.Errorf("stray indented separator should round-trip verbatim:\n%s", out)
	}
	if !strings.Contains(out, "markdown body starts here") {
		t.Errorf("markdown body lost in round-trip:\n%s", out)
	}
	bodyIdx := strings.Index(out, "markdown body starts here")
	sepIdx := strings.Index(out, "\n---\n")
	if sepIdx == -1 || bodyIdx < sepIdx {
		t.Errorf("indent-0 separator should still precede the body:\n%s", out)
	}
}

// The real indent-0 separator must still end frontmatter parsing: keys in
// the body section must NOT be parsed into the item.
func TestParseBodySeparatorStillEndsFrontmatter(t *testing.T) {
	content := `id: T-001
type: task
status: queued
title: body separator still works

time_tracking:
  completed_at: 2026-05-23T18:41:25-06:00
---
work_duration_seconds: 99999
time_tracking:
  work_duration_seconds: 99999
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, ok := item.TimeTracking["work_duration_seconds"]; ok {
		t.Errorf("body content leaked into frontmatter: work_duration_seconds=%v", v)
	}
	if got, _ := item.TimeTracking["completed_at"].(string); got != "2026-05-23T18:41:25-06:00" {
		t.Errorf("frontmatter field before separator lost: completed_at=%q", got)
	}
}
