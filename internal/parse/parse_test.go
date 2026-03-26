package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBasicTask(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: A simple task

summary: |
  This is a multiline
  summary block.

depends_on:
- []

next_actions:
- Do something
- Do another thing
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.ID != "T-001" {
		t.Errorf("ID = %q, want %q", item.ID, "T-001")
	}
	if item.Type != "task" {
		t.Errorf("Type = %q, want %q", item.Type, "task")
	}
	if item.Status != "queued" {
		t.Errorf("Status = %q, want %q", item.Status, "queued")
	}
	if item.Title != "A simple task" {
		t.Errorf("Title = %q, want %q", item.Title, "A simple task")
	}
	if item.Summary != "This is a multiline\nsummary block." {
		t.Errorf("Summary = %q", item.Summary)
	}
	if len(item.NextActions) != 2 {
		t.Errorf("NextActions = %v, want 2 items", item.NextActions)
	}
}

func TestParseQuotedTitle(t *testing.T) {
	content := `id: T-002
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: "Bootstrap ` + "`as`" + ` CLI tool"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	want := "Bootstrap `as` CLI tool"
	if item.Title != want {
		t.Errorf("Title = %q, want %q", item.Title, want)
	}
}

func TestParseInlineComment(t *testing.T) {
	content := `id: T-003
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: CSRF protection

depends_on:
- T-108  # must split staff role first
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if len(item.DependsOn) != 1 || item.DependsOn[0] != "T-108" {
		t.Errorf("DependsOn = %v, want [T-108]", item.DependsOn)
	}
}

func TestParseEmptyListVariants(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			"dash-brackets",
			"id: T-001\ntype: task\nstatus: queued\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: test\n\ndepends_on:\n- []\n",
		},
		{
			"bare-brackets",
			"id: T-001\ntype: task\nstatus: queued\ncreated: 2026-03-25T10:00:00-06:00\nlast_touched: 2026-03-25T10:00:00-06:00\ntitle: test\n\ndepends_on: []\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, tt.content)
			item, err := File(path)
			if err != nil {
				t.Fatalf("File: %v", err)
			}
			if len(item.DependsOn) != 0 {
				t.Errorf("DependsOn = %v, want empty", item.DependsOn)
			}
		})
	}
}

func TestParseNestedWorkTracking(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

work_tracking:
  branch: feat/T-001-test
  commits: []
  pr: []
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.WorkTracking["branch"] != "feat/T-001-test" {
		t.Errorf("branch = %v, want feat/T-001-test", item.WorkTracking["branch"])
	}
}

func TestParseTestingEvidenceModern(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

testing_evidence:
  tests_written:
  - []
  required_suites:
    api_unit:      null
    api_lint:      null
    web_typecheck: null
    web_unit:      null
  scope_suites:
    api_integration: null
    web_integration: null
    web_e2e:         null
  notes: null
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
		if m["api_unit"] != "" {
			t.Errorf("api_unit = %v, want empty (null)", m["api_unit"])
		}
	}
}

func TestParseMarkdownBody(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

---

# T-001 — Test

## Goal
Do the thing.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.ID != "T-001" {
		t.Errorf("ID = %q", item.ID)
	}

	// Verify the document preserves the markdown body
	output := item.Doc.String()
	if output != strings.TrimSuffix(content, "\n") {
		// Just verify it contains the markdown
		if !contains(output, "# T-001") {
			t.Error("document lost markdown body")
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Roundtrip test: parse then serialize, compare byte-for-byte.
func TestRoundtripSimple(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: A simple task

depends_on:
- []

next_actions:
- Do something`

	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	got := item.Doc.String()
	if got != content {
		t.Errorf("roundtrip mismatch:\ngot:\n%s\n\nwant:\n%s", got, content)
	}
}

func TestParseNumericPriority(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
priority: 2
category: security
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.Priority == nil {
		t.Fatal("Priority is nil, want 2")
	}
	if *item.Priority != 2 {
		t.Errorf("Priority = %d, want 2", *item.Priority)
	}
	if item.Category != "security" {
		t.Errorf("Category = %q, want %q", item.Category, "security")
	}
}

func TestParseStringPriorityDoesNotPollute(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
priority: alpha-critical
category: security
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	// String priority should NOT be parsed into Priority field
	if item.Priority != nil {
		t.Errorf("Priority = %d, want nil (string priority should not parse)", *item.Priority)
	}
	// Category must NOT be overwritten by priority string
	if item.Category != "security" {
		t.Errorf("Category = %q, want %q (priority leaked into category)", item.Category, "security")
	}
	// Raw document should still have the string value for migration
	val, ok := item.Doc.GetField("priority")
	if !ok || val != "alpha-critical" {
		t.Errorf("Doc.GetField(priority) = %q/%v, want alpha-critical/true", val, ok)
	}
}

func TestParsePriorityZero(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
priority: 0
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.Priority == nil {
		t.Fatal("Priority is nil, want 0")
	}
	if *item.Priority != 0 {
		t.Errorf("Priority = %d, want 0", *item.Priority)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
