package parse

import (
	"testing"
)

// Tests to boost parse coverage to >=85%.

func TestParseManifestAndTimeTracking(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Full manifest

manifest:
  prs: "https://github.com/org/repo/pull/42"
  sha: abc123

time_tracking:
  started_at: 2026-03-25T10:00:00-06:00
  completed_at: null
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Manifest["prs"] != "https://github.com/org/repo/pull/42" {
		t.Errorf("manifest.prs = %v", item.Manifest["prs"])
	}
	if item.Manifest["sha"] != "abc123" {
		t.Errorf("manifest.sha = %v", item.Manifest["sha"])
	}
	if item.TimeTracking["started_at"] != "2026-03-25T10:00:00-06:00" {
		t.Errorf("time_tracking.started_at = %v", item.TimeTracking["started_at"])
	}
}

func TestParseTestsWrittenList(t *testing.T) {
	// tests_written as a top-level list (not nested under testing_evidence)
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: With tests_written

tests_written:
- "handler_test.go -- 5 tests"
- "service_test.go -- 3 tests"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	tw, ok := item.TestingEvidence["tests_written"]
	if !ok {
		t.Fatal("missing tests_written")
	}
	if list, ok := tw.([]string); ok {
		if len(list) != 2 {
			t.Errorf("tests_written = %v, want 2 items", list)
		}
	} else {
		t.Errorf("tests_written wrong type: %T", tw)
	}
}

func TestParseNestedMultilineBlock(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Nested multiline

work_tracking:
  notes: |
    This is a multiline
    work tracking note.
  branch: feat/T-001
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.WorkTracking["branch"] != "feat/T-001" {
		t.Errorf("branch = %v", item.WorkTracking["branch"])
	}
}

func TestParseBlockAtEOF(t *testing.T) {
	// Test multiline block that isn't terminated by a blank line
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Block at EOF

summary: |
  This block goes to end of file
  with no trailing blank line.`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.Summary == "" {
		t.Error("summary should not be empty")
	}
}

func TestParseListAtEOF(t *testing.T) {
	// Test list that isn't terminated by a blank line
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: List at EOF

next_actions:
- First action
- Second action`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if len(item.NextActions) != 2 {
		t.Errorf("next_actions = %v, want 2 items", item.NextActions)
	}
}

func TestParseListOfMapsAtEOF(t *testing.T) {
	// Test list-of-maps that runs to EOF
	content := `id: T-001
type: task
status: completed
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Runs at EOF

testing_evidence:
  runs:
    - command: "make test"
      result: pass`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.ID != "T-001" {
		t.Errorf("ID = %q", item.ID)
	}
}

func TestParsePriorityField(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Priority test
priority: 1
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// Priority is parsed as numeric int
	if item.Priority == nil {
		t.Fatal("Priority is nil, want 1")
	}
	if *item.Priority != 1 {
		t.Errorf("Priority = %d, want 1", *item.Priority)
	}
	// Category should be unaffected
	if item.Category != "" {
		t.Errorf("Category = %q, want empty (priority should not leak)", item.Category)
	}
}

func TestParseBareListDash(t *testing.T) {
	// Test bare "-" list item (no content after dash)
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Bare dash test

next_actions:
-
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// Should parse without error
	_ = item
}

func TestParseSplitKVNoColon(t *testing.T) {
	k, v := splitKV("no-colon-here")
	if k != "no-colon-here" || v != "" {
		t.Errorf("splitKV no colon: k=%q v=%q", k, v)
	}
}

func TestParseFlushListOnNewKey(t *testing.T) {
	// List immediately followed by a key (no blank line between)
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Flush test
tags:
- alpha
- beta
priority: 1`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.Tags) != 2 {
		t.Errorf("tags = %v, want 2 items", item.Tags)
	}
}

func TestParseNullValues(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Null test
severity: ~
repo: null
category: ""
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Severity != "" {
		t.Errorf("severity = %q, want empty (tilde = null)", item.Severity)
	}
	if item.Repo != "" {
		t.Errorf("repo = %q, want empty (null)", item.Repo)
	}
}

func TestParseNestedBlockFolded(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Folded block

context: >
  This is a folded
  multiline context.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Context == "" {
		t.Error("context should not be empty")
	}
}

func TestParseDoubleBracketEmptyList(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Double bracket

depends_on:
- [[]]
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty", item.DependsOn)
	}
}

func TestParseNestedNullTilde(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Nested null

delivery:
  stage: ~
  deployed_date: null
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Delivery["stage"] != "" {
		t.Errorf("delivery.stage = %v, want empty", item.Delivery["stage"])
	}
}
