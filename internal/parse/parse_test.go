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

func TestParseTimeTracking(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

time_tracking:
  started_at: 2026-03-25T10:00:00-06:00
  completed_at: null
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.TimeTracking["started_at"] != "2026-03-25T10:00:00-06:00" {
		t.Errorf("time_tracking.started_at = %v", item.TimeTracking["started_at"])
	}
}

func TestParseManifest(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

manifest:
  sha: abc123
  files_changed: 5
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Manifest["sha"] != "abc123" {
		t.Errorf("manifest.sha = %v", item.Manifest["sha"])
	}
	if item.Manifest["files_changed"] != "5" {
		t.Errorf("manifest.files_changed = %v", item.Manifest["files_changed"])
	}
}

func TestParseTestingEvidenceFlat(t *testing.T) {
	// The parser stores nested testing_evidence fields flat (not in sub-maps).
	// required_suites:/scope_suites: sub-keys end up as top-level TestingEvidence keys.
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

testing_evidence:
  notes: some notes here

  required_suites:
    api_unit: "pass | 2026-03-25"

  scope_suites:
    api_integration: "pass | 2026-03-25"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	// notes stored directly
	if item.TestingEvidence["notes"] != "some notes here" {
		t.Errorf("notes = %v", item.TestingEvidence["notes"])
	}
}

func TestParseDelivery(t *testing.T) {
	content := `id: T-001
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

delivery:
  stage: pushed
  deployed_date: null
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Delivery["stage"] != "pushed" {
		t.Errorf("delivery.stage = %v", item.Delivery["stage"])
	}
}

func TestParseMultilineBlock(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

summary: |
  This is a multiline
  summary block.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !containsStr(item.Summary, "multiline") {
		t.Errorf("summary = %q, want multiline content", item.Summary)
	}
}

func TestParseContext(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

context: |
  Background info here.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !containsStr(item.Context, "Background") {
		t.Errorf("context = %q", item.Context)
	}
}

func TestParseNullValuesV2(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
severity: ~
category: null
assigned_to: agent-a
last_touched_by: agent-b
epic: my-epic
sprint: sprint-1
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Severity != "" {
		t.Errorf("severity should be empty for ~, got %q", item.Severity)
	}
	if item.Category != "" {
		t.Errorf("category should be empty for null, got %q", item.Category)
	}
	if item.AssignedTo != "agent-a" {
		t.Errorf("assigned_to = %q", item.AssignedTo)
	}
	if item.Epic != "my-epic" {
		t.Errorf("epic = %q", item.Epic)
	}
}

func TestParseListOfMaps(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

testing_evidence:
  runs:
    - command: make test
      result: pass
    - command: make lint
      result: pass

priority: 1
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	runs, ok := item.TestingEvidence["runs"]
	if !ok {
		t.Fatal("missing testing_evidence.runs")
	}
	if runsList, ok := runs.([]map[string]string); ok {
		if len(runsList) != 2 {
			t.Errorf("runs has %d entries, want 2", len(runsList))
		}
	}
}

func TestParseInlineCommentOnValue(t *testing.T) {
	content := `id: T-001
type: task
status: queued # active soon
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test
priority: 1 # high priority

testing_evidence:
  notes: important # not really
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Status != "queued" {
		t.Errorf("status = %q, want queued (comment stripped)", item.Status)
	}
	if item.Priority == nil || *item.Priority != 1 {
		t.Errorf("priority = %v, want 1 (comment stripped)", item.Priority)
	}
}

func TestParseFlushListOfMapsOnBlank(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

testing_evidence:
  runs:
    - command: make test
      result: pass

title: Test
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	_ = item // just ensure no panic
}

func TestParseSessions(t *testing.T) {
	content := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: test

sessions:
- session-abc-123
- session-def-456
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.Sessions) != 2 || item.Sessions[0] != "session-abc-123" {
		t.Errorf("sessions = %v", item.Sessions)
	}
}

func TestParseACPreservesInternalQuotes(t *testing.T) {
	// Regression: strings.Trim(val, `"'`) stripped the trailing quote from
	// ACs like `cmd: grep -q 'foo'`, producing an unbalanced shell command
	// that failed at UAT time with "unexpected EOF while looking for matching".
	content := `id: I-001
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: AC quote preservation

acceptance_criteria:
- cmd: grep -q 'playbook: ansible/basic-server/deploy-api.yml' file.yml
- cmd: grep -q "double quoted value"
- "cmd: fully single-wrapped"
- 'cmd: fully single-wrapped 2'
- cmd: mixed 'single' and "double"
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.AcceptanceCriteria) != 5 {
		t.Fatalf("AcceptanceCriteria len = %d, want 5: %v", len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	want := []string{
		"cmd: grep -q 'playbook: ansible/basic-server/deploy-api.yml' file.yml",
		`cmd: grep -q "double quoted value"`,
		"cmd: fully single-wrapped",
		"cmd: fully single-wrapped 2",
		`cmd: mixed 'single' and "double"`,
	}
	for i, w := range want {
		if item.AcceptanceCriteria[i] != w {
			t.Errorf("AC[%d] = %q, want %q", i, item.AcceptanceCriteria[i], w)
		}
	}
}

// TestParseFencedCodeBlockNotFrontmatter is the I-394 regression test:
// a description body containing a ```yaml fenced block whose body has
// `type: warning` must NOT corrupt the item's top-level Type field.
func TestParseFencedCodeBlockNotFrontmatter(t *testing.T) {
	content := "id: T-304\n" +
		"type: task\n" +
		"status: queued\n" +
		"created: 2026-04-23T20:04:53-07:00\n" +
		"last_touched: 2026-04-23T20:06:22-07:00\n" +
		"title: T-304 fixture\n" +
		"description: ## Problem\n" +
		"\n" +
		"Example mailbox message:\n" +
		"\n" +
		"```yaml\n" +
		"from: agent-a-1\n" +
		"to: agent-a-2\n" +
		"type: warning\n" +
		"body: \"auth changed\"\n" +
		"```\n" +
		"\n" +
		"After the fence — back to body.\n"

	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Type != "task" {
		t.Errorf("Type = %q after parsing fence content; want %q (fenced `type: warning` leaked into frontmatter)", item.Type, "task")
	}
}

// TestParseFencedCodeBlockClosing verifies that the closing ``` line
// flips the parser back into normal frontmatter mode so that a `---`
// separator after the fence is honored as the body marker.
func TestParseFencedCodeBlockClosing(t *testing.T) {
	content := "id: T-FENCE\n" +
		"type: task\n" +
		"status: queued\n" +
		"created: 2026-04-23T20:04:53-07:00\n" +
		"last_touched: 2026-04-23T20:06:22-07:00\n" +
		"title: closing fence test\n" +
		"description: |-\n" +
		"  ```yaml\n" +
		"  type: warning\n" +
		"  ```\n"

	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Type != "task" {
		t.Errorf("Type = %q; want %q", item.Type, "task")
	}
	// Roundtrip preserves the fence content verbatim.
	out := item.Doc.String()
	if !strings.Contains(out, "```yaml") || !strings.Contains(out, "type: warning") {
		t.Errorf("roundtrip lost fence content:\n%s", out)
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

// I-487: SBAR with all four multiline children parses into the typed
// struct AND survives a serialize-reparse round-trip without churn.
func TestParse_SBARBlock(t *testing.T) {
	content := `id: I-001
type: issue
status: queued
created: 2026-04-30T10:00:00-06:00
last_touched: 2026-04-30T10:00:00-06:00
title: SBAR-shaped issue

sbar:
  situation: |
    CreateAppointment returns RLS_VIOLATION on every fresh-signup tenant.
  background: |
    UserContextMiddleware sets app.tenant_id on a per-request *sql.Conn
    pinned in ctx; service methods that bypass s.querier(ctx) get a
    fresh pool conn without RLS context.
  assessment: |
    CreateAppointment passes s.db to scheduling.ValidateResource*
    helpers — the helpers query through the raw pool. Reproducible 100%
    of the time on fresh signup.
  recommendation: |
    Widen scheduling validators from *sql.DB to a Querier interface;
    switch the 4 call sites in db/appointments.go to s.querier(ctx).
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if !strings.Contains(item.SBAR.Situation, "RLS_VIOLATION") {
		t.Errorf("SBAR.Situation = %q", item.SBAR.Situation)
	}
	if !strings.Contains(item.SBAR.Background, "UserContextMiddleware") {
		t.Errorf("SBAR.Background = %q", item.SBAR.Background)
	}
	if !strings.Contains(item.SBAR.Assessment, "CreateAppointment passes") {
		t.Errorf("SBAR.Assessment = %q", item.SBAR.Assessment)
	}
	if !strings.Contains(item.SBAR.Recommendation, "Widen scheduling") {
		t.Errorf("SBAR.Recommendation = %q", item.SBAR.Recommendation)
	}

	// Round-trip: serialize and re-parse — fields survive.
	out := item.Doc.String()
	path2 := writeTempFile(t, out)
	item2, err := File(path2)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if item2.SBAR.Situation != item.SBAR.Situation {
		t.Errorf("Situation churn:\n  before=%q\n  after =%q", item.SBAR.Situation, item2.SBAR.Situation)
	}
	if item2.SBAR.Recommendation != item.SBAR.Recommendation {
		t.Errorf("Recommendation churn:\n  before=%q\n  after =%q", item.SBAR.Recommendation, item2.SBAR.Recommendation)
	}
}

// I-487 backwards-compat: a legacy item with summary: but no sbar:
// parses with empty SBAR struct and the original Summary preserved.
// Round-trip is byte-stable.
func TestParse_LegacySummaryStillWorks(t *testing.T) {
	content := `id: I-002
type: issue
status: queued
created: 2026-04-30T10:00:00-06:00
last_touched: 2026-04-30T10:00:00-06:00
title: Pre-SBAR legacy item

summary: |-
  Single-blob summary written before the SBAR schema landed.
  Two paragraphs of context here.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !item.SBAR.IsEmpty() {
		t.Errorf("legacy item should have empty SBAR, got %+v", item.SBAR)
	}
	if !strings.Contains(item.Summary, "Single-blob summary") {
		t.Errorf("Summary churn: %q", item.Summary)
	}
}
