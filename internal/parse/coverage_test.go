package parse

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests targeting uncovered code paths to boost coverage.

func TestParseAllListTypes(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Test

tags: [alpha, beta]

resolution:
- First resolution
- Second resolution

depends_on:
- T-002
- T-003

blocks:
- T-004

related_issues:
- I-001

invariants:
- Must be true

doc_changes:
- updated foo.md

linked_plans:
- "plan-a.md"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if len(item.DependsOn) != 2 {
		t.Errorf("DependsOn = %v, want 2", item.DependsOn)
	}
	if len(item.Blocks) != 1 {
		t.Errorf("Blocks = %v, want 1", item.Blocks)
	}
	if len(item.RelatedIssues) != 1 {
		t.Errorf("RelatedIssues = %v, want 1", item.RelatedIssues)
	}
	if len(item.Resolution) != 2 {
		t.Errorf("Resolution = %v, want 2", item.Resolution)
	}
	if len(item.Invariants) != 1 {
		t.Errorf("Invariants = %v, want 1", item.Invariants)
	}
	if len(item.DocChanges) != 1 {
		t.Errorf("DocChanges = %v, want 1", item.DocChanges)
	}
	if len(item.LinkedPlans) != 1 {
		t.Errorf("LinkedPlans = %v, want 1", item.LinkedPlans)
	}
}

func TestParseNestedTestingEvidence(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Test

testing_evidence:
  tests_written:
  - "handler_test.go -- 5 tests"
  required_suites:
    api_unit: "pass | 2026-03-25"
    api_lint: null
  scope_suites:
    api_integration: "pass | 2026-03-25"
    web_e2e: null
  notes: some notes

time_tracking:
  started_at: 2026-03-25T10:00:00-06:00
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	rs, ok := item.TestingEvidence["required_suites"]
	if !ok {
		t.Fatal("missing required_suites")
	}
	if m, ok := rs.(map[string]interface{}); ok {
		if m["api_unit"] != "pass | 2026-03-25" {
			t.Errorf("api_unit = %v", m["api_unit"])
		}
	}

	if item.TimeTracking["started_at"] != "2026-03-25T10:00:00-06:00" {
		t.Errorf("started_at = %v", item.TimeTracking["started_at"])
	}
}

func TestParseAllScalarFields(t *testing.T) {
	content := `id: I-001
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: 2026-03-25T12:00:00-06:00

title: Bug report
severity: high
category: billing
repo: theraprac-api
assigned_to: agent-a
last_touched_by: agent-b
parallel_group: A

context: |
  This is context
  with multiple lines.

summary: |
  This is a summary
  block.

acceptance_criteria:
- Must fix the bug
- Must add tests
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.ID != "I-001" {
		t.Errorf("ID = %q", item.ID)
	}
	if item.Severity != "high" {
		t.Errorf("Severity = %q", item.Severity)
	}
	if item.Repo != "theraprac-api" {
		t.Errorf("Repo = %q", item.Repo)
	}
	if item.AssignedTo != "agent-a" {
		t.Errorf("AssignedTo = %q", item.AssignedTo)
	}
	if item.LastTouchedBy != "agent-b" {
		t.Errorf("LastTouchedBy = %q", item.LastTouchedBy)
	}
	if item.Completed == nil {
		t.Error("Completed should not be nil")
	}
	if item.Context == "" {
		t.Error("Context should not be empty")
	}
	if item.Summary == "" {
		t.Error("Summary should not be empty")
	}
	if len(item.AcceptanceCriteria) != 2 {
		t.Errorf("AcceptanceCriteria = %v, want 2", item.AcceptanceCriteria)
	}
}

func TestParseLegacyTestingRuns(t *testing.T) {
	content := `id: T-001
type: task
status: completed
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Legacy task

testing_evidence:
  required: true
  runs:
    - command: "make test-unit"
      scope: "backend"
      result: pass
      run_at: "2026-03-25"
      notes: "all good"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.ID != "T-001" {
		t.Errorf("ID = %q", item.ID)
	}
}

func TestParseDeliveryFields(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Active task

work_tracking:
  branch: feat/T-001-test
  commits: []
  pr: []

delivery:
  stage: pr_open
  deployed_date: null

manifest:
  prs: []
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.WorkTracking["branch"] != "feat/T-001-test" {
		t.Errorf("branch = %v", item.WorkTracking["branch"])
	}
	if item.Delivery["stage"] != "pr_open" {
		t.Errorf("stage = %v", item.Delivery["stage"])
	}
}

func TestParseTimestampFormats(t *testing.T) {
	tests := []struct {
		name    string
		ts      string
		wantOK  bool
	}{
		{"RFC3339", "2026-03-25T10:00:00-06:00", true},
		{"UTC", "2026-03-25T10:00:00Z", true},
		{"no tz", "2026-03-25T10:00:00", true},
		{"date only", "2026-03-25", true},
		{"empty", "", false},
		{"null", "null", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTime(tt.ts)
			isZero := result.IsZero()
			if tt.wantOK && isZero {
				t.Errorf("parseTime(%q) returned zero, want valid", tt.ts)
			}
			if !tt.wantOK && !isZero {
				t.Errorf("parseTime(%q) returned valid, want zero", tt.ts)
			}
		})
	}
}

func TestFindInlineComment(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"value # comment", 5},
		{"value", -1},
		{`"quoted # not comment"`, -1},
		{`'quoted # not comment'`, -1},
		{"bare # yes", 4},
	}

	for _, tt := range tests {
		got := findInlineComment(tt.input)
		if got != tt.want {
			t.Errorf("findInlineComment(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseFileNotFound(t *testing.T) {
	_, err := File("/nonexistent/path.md")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestRoundtripWithAllFields(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Full task

summary: |
  A multi-line
  summary.

work_tracking:
  branch: feat/T-001
  commits: []
  pr: []

delivery:
  stage: coding
  deployed_date: null

testing_evidence:
  tests_written:
  - []
  required_suites:
    api_unit:      null
    api_lint:      null
  scope_suites:
    api_integration: null
  notes: null

depends_on:
- T-002

blocks:
- T-003

next_actions:
- Do something`

	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	os.WriteFile(path, []byte(content), 0644)

	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	got := item.Doc.String()
	if got != content {
		t.Errorf("roundtrip mismatch:\ngot:\n%s\nwant:\n%s", got, content)
	}
}
