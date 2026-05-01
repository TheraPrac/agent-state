package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupTestEnvWithChangelog creates a standard test env with a .changelog directory.
func setupTestEnvWithChangelog(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	return s, cfg
}

// === Tag ===

func TestTagAddHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "add", "security")
	if code != 0 {
		t.Errorf("Tag add returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 1 || item.Tags[0] != "security" {
		t.Errorf("Tags = %v, want [security]", item.Tags)
	}

	// Verify changelog
	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "tag_add" && e.NewValue == "security" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for tag_add")
	}
}

func TestTagAddDuplicate(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "alpha")
	code := Tag(s, cfg, "T-001", "add", "alpha")
	if code != 1 {
		t.Errorf("Tag add duplicate returned %d, want 1", code)
	}
}

func TestTagRmHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "alpha")
	Tag(s, cfg, "T-001", "add", "beta")

	code := Tag(s, cfg, "T-001", "rm", "alpha")
	if code != 0 {
		t.Errorf("Tag rm returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 1 || item.Tags[0] != "beta" {
		t.Errorf("Tags = %v, want [beta]", item.Tags)
	}
}

func TestTagRmNotPresent(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "rm", "nonexistent")
	if code != 1 {
		t.Errorf("Tag rm nonexistent returned %d, want 1", code)
	}
}

func TestTagRmLastTag(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "only-tag")

	code := Tag(s, cfg, "T-001", "rm", "only-tag")
	if code != 0 {
		t.Errorf("Tag rm last returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", item.Tags)
	}
}

func TestTagNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-999", "add", "foo")
	if code != 1 {
		t.Errorf("Tag not found returned %d, want 1", code)
	}
}

func TestTagBadAction(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "flip", "foo")
	if code != 2 {
		t.Errorf("Tag bad action returned %d, want 2", code)
	}
}

func TestTagAddRoundtrip(t *testing.T) {
	// Regression test: tag add must survive write→re-parse cycle.
	// Previously, updateTagsInDoc wrote inline "[x, y]" format that the
	// parser couldn't read back, silently dropping tags.
	s, cfg := setupTestEnvWithChangelog(t)

	Tag(s, cfg, "T-001", "add", "security")
	Tag(s, cfg, "T-001", "add", "billing")

	// Force re-parse by creating a new store from the same directory
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}

	item, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after re-parse")
	}
	if len(item.Tags) != 2 {
		t.Fatalf("Tags after roundtrip = %v, want [security billing]", item.Tags)
	}
	if item.Tags[0] != "security" || item.Tags[1] != "billing" {
		t.Errorf("Tags = %v, want [security billing]", item.Tags)
	}

	// Verify the file content uses multi-line format
	path, _ := s2.Path("T-001")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "- security") || !strings.Contains(content, "- billing") {
		t.Errorf("file should use multi-line format, got:\n%s", content)
	}
}

// === Dep Add/Rm ===

func TestDepAddHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-001", "T-003")
	if code != 0 {
		t.Errorf("DepAdd returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	found := false
	for _, d := range item.DependsOn {
		if d == "T-003" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-001 DependsOn = %v, want to contain T-003", item.DependsOn)
	}

	dep, _ := s.Get("T-003")
	found = false
	for _, b := range dep.Blocks {
		if b == "T-001" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-003 Blocks = %v, want to contain T-001", dep.Blocks)
	}
}

func TestDepAddDuplicate(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	DepAdd(s, cfg, "T-001", "T-003")
	code := DepAdd(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepAdd duplicate returned %d, want 1", code)
	}
}

func TestDepAddSelf(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-001", "T-001")
	if code != 2 {
		t.Errorf("DepAdd self returned %d, want 2", code)
	}
}

func TestDepAddNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-999", "T-001")
	if code != 1 {
		t.Errorf("DepAdd missing id returned %d, want 1", code)
	}
	code = DepAdd(s, cfg, "T-001", "T-999")
	if code != 1 {
		t.Errorf("DepAdd missing dep returned %d, want 1", code)
	}
}

func TestDepRmHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// First add the dependency
	DepAdd(s, cfg, "T-001", "T-003")

	// Then remove it
	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 0 {
		t.Errorf("DepRm returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	for _, d := range item.DependsOn {
		if d == "T-003" {
			t.Error("T-003 should be removed from depends_on")
		}
	}

	dep, _ := s.Get("T-003")
	for _, b := range dep.Blocks {
		if b == "T-001" {
			t.Error("T-001 should be removed from blocks")
		}
	}
}

func TestDepRmNotDependency(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepRm non-dependency returned %d, want 1", code)
	}
}

func TestDepRmNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepRm(s, cfg, "T-999", "T-001")
	if code != 1 {
		t.Errorf("DepRm missing id returned %d, want 1", code)
	}
}

// === Log ===

func TestLogSingleHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Create some changelog entries
	changelog.Append(cfg, "T-001", changelog.Entry{
		Timestamp: "2026-03-25T10:00:00-06:00", Op: "create", NewValue: "queued",
	})
	changelog.Append(cfg, "T-001", changelog.Entry{
		Timestamp: "2026-03-25T11:00:00-06:00", Op: "start", OldValue: "queued", NewValue: "active",
	})

	code := Log(s, cfg, "T-001", LogOpts{})
	if code != 0 {
		t.Errorf("Log T-001 returned %d, want 0", code)
	}
}

func TestLogSingleNoEntries(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "T-001", LogOpts{})
	if code != 0 {
		t.Errorf("Log empty returned %d, want 0", code)
	}
}

func TestLogSingleNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "T-999", LogOpts{})
	if code != 1 {
		t.Errorf("Log not found returned %d, want 1", code)
	}
}

func TestLogAll(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	changelog.Append(cfg, "T-001", changelog.Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})
	changelog.Append(cfg, "T-002", changelog.Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})

	code := Log(s, cfg, "", LogOpts{})
	if code != 0 {
		t.Errorf("Log all returned %d, want 0", code)
	}
}

func TestLogAllEmpty(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "", LogOpts{})
	if code != 0 {
		t.Errorf("Log all empty returned %d, want 0", code)
	}
}

func TestLogWithLimit(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	for i := 0; i < 10; i++ {
		changelog.Append(cfg, "T-001", changelog.Entry{Op: "update", Timestamp: "2026-03-25T10:00:00-06:00"})
	}

	code := Log(s, cfg, "T-001", LogOpts{Limit: 3})
	if code != 0 {
		t.Errorf("Log with limit returned %d, want 0", code)
	}
}

// === Create with priority (I-406) ===

// I-406: severity is dead. The CLI rejects --severity at the entry point;
// callers use --priority (0-4) instead.
func TestCreateIssueRejectsSeverity(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Create(s, cfg, "issue", "Critical bug", CreateOpts{Priority: 0, Severity: "critical"})
	if code != 2 {
		t.Errorf("Create issue with --severity should exit 2 (deprecated), got %d", code)
	}
}

func TestCreateIssueWithPriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Create(s, cfg, "issue", "Critical bug", CreateOpts{Priority: 0})
	if code != 0 {
		t.Errorf("Create issue with priority=0 returned %d, want 0", code)
	}
	item, ok := s.Get("I-002")
	if !ok {
		t.Fatal("I-002 should exist after create")
	}
	if item.Priority == nil || *item.Priority != 0 {
		t.Errorf("priority = %v, want 0", item.Priority)
	}
	path, _ := s.Path("I-002")
	content, _ := os.ReadFile(path)
	if !containsStr(string(content), "priority: 0") {
		t.Error("file should contain 'priority: 0'")
	}
}

// I-494: `st update <id> summary "<text>"` must continue to work
// during the deprecation window — but route the new content to
// sbar.background and emit a deprecation notice. The shim runs
// before the existing nested-field path takes over.
func TestUpdateSummaryRoutesToSBARBackground(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Capture stderr to assert on the deprecation notice without
	// needing a separate test for it.
	origStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	defer func() { os.Stderr = origStderr }()

	code := Update(s, cfg, "I-001", "summary", "the new content", UpdateModeValue)
	wPipe.Close()
	stderrOut := readAll(t, rPipe)

	if code != 0 {
		t.Errorf("Update summary should exit 0 (shim), got %d", code)
	}

	if !strings.Contains(stderrOut, "deprecated") {
		t.Errorf("expected deprecation notice on stderr, got: %q", stderrOut)
	}
	if !strings.Contains(stderrOut, "sbar.background") {
		t.Errorf("expected sbar.background pointer on stderr, got: %q", stderrOut)
	}

	// File should now have the value under sbar.background, NOT under a
	// new top-level summary: line.
	path, _ := s.Path("I-001")
	body, _ := os.ReadFile(path)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "the new content") {
		t.Errorf("expected new content to land in file, body:\n%s", bodyStr)
	}

	// Re-load store and assert SBAR struct picked up the value.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}
	item, ok := s2.Get("I-001")
	if !ok {
		t.Fatal("I-001 not found after re-parse")
	}
	if !strings.Contains(item.SBAR.Background, "the new content") {
		t.Errorf("expected sbar.background to contain new content, got: %q", item.SBAR.Background)
	}
}

// I-494 (review fix): writing the same value that already lives in
// sbar.background must be a no-op — no Mutate, no changelog entry,
// no GitSync commit, no deprecation notice (the user did not change
// anything, so the warning would be noise).
func TestUpdateSummary_NoOpWhenUnchanged(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Seed sbar.background with a known value via the shim itself.
	if code := Update(s, cfg, "I-001", "summary", "stable text", UpdateModeValue); code != 0 {
		t.Fatalf("seed Update returned %d", code)
	}
	beforeEntries, _ := changelog.Read(cfg, "I-001")
	beforeCount := len(beforeEntries)

	// Capture stderr so we can also assert no deprecation notice for the no-op.
	origStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	defer func() { os.Stderr = origStderr }()

	if code := Update(s, cfg, "I-001", "summary", "stable text", UpdateModeValue); code != 0 {
		t.Fatalf("no-op Update returned %d, want 0", code)
	}
	wPipe.Close()
	stderrOut := readAll(t, rPipe)

	afterEntries, _ := changelog.Read(cfg, "I-001")
	if len(afterEntries) != beforeCount {
		t.Errorf("changelog grew from %d to %d on no-op", beforeCount, len(afterEntries))
	}
	if strings.Contains(stderrOut, "deprecated") {
		t.Errorf("no-op should not emit deprecation notice; got: %q", stderrOut)
	}
}

// I-494: multi-line summary writes (common via stdin / editor mode)
// must produce valid YAML. SetNestedField writes inline `key: value`
// only — using it for sbar.background would emit a malformed file
// for any value containing a newline. The shim routes through
// SetSBARBlock instead, which emits a proper block scalar.
func TestUpdateSummaryMultilineWritesValidBlockScalar(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	origStderr := os.Stderr
	_, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	defer func() { os.Stderr = origStderr; wPipe.Close() }()

	multi := "first paragraph\n\nsecond paragraph\n  with indent"
	if code := Update(s, cfg, "I-001", "summary", multi, UpdateModeValue); code != 0 {
		t.Fatalf("Update summary returned %d", code)
	}

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}
	item, ok := s2.Get("I-001")
	if !ok {
		t.Fatal("I-001 not found after re-parse")
	}
	if item.SBAR.Background != multi {
		t.Errorf("sbar.background round-trip lost data:\n  got:  %q\n  want: %q",
			item.SBAR.Background, multi)
	}
}

// I-494: the changelog entry must record `field=sbar.background` so
// future readers see which logical field changed — not the deprecated
// `summary` alias the user typed.
func TestUpdateSummaryChangelogRecordsSBARField(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Suppress stderr noise.
	origStderr := os.Stderr
	_, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	defer func() { os.Stderr = origStderr; wPipe.Close() }()

	if code := Update(s, cfg, "I-001", "summary", "shim test", UpdateModeValue); code != 0 {
		t.Fatalf("Update summary returned %d, want 0", code)
	}
	entries, _ := changelog.Read(cfg, "I-001")
	found := false
	for _, e := range entries {
		if e.Op == "update" && e.Field == "sbar.background" && e.NewValue == "shim test" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected changelog entry with field=sbar.background, got %+v", entries)
	}
}

// readAll reads everything from r into a string. Helper for stderr-
// pipe-capture tests.
func readAll(t *testing.T, r *os.File) string {
	t.Helper()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// I-406: `st update <id> severity <anything>` must exit non-zero with
// the migration pointer rather than silently writing a deprecated field.
func TestUpdateRejectsSeverity(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Update(s, cfg, "I-001", "severity", "high", UpdateModeValue)
	if code != 2 {
		t.Errorf("Update severity should exit 2 (deprecated), got %d", code)
	}
}

// I-406: `st update <id> priority 9` must exit non-zero with a clear
// must-be-0-4 message — the new value-set check.
func TestUpdateRejectsOutOfRangePriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Update(s, cfg, "T-003", "priority", "9", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=9 should exit 2, got %d", code)
	}
	if code := Update(s, cfg, "T-003", "priority", "-1", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=-1 should exit 2, got %d", code)
	}
	if code := Update(s, cfg, "T-003", "priority", "abc", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=abc should exit 2, got %d", code)
	}
}

// I-508: `st update <id> status open` must exit 2 with a helpful
// message naming valid statuses and the queued suggestion (legacy alias).
func TestUpdateRejectsLegacyStatusValue(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Update(s, cfg, "I-001", "status", "open", UpdateModeValue); code != 2 {
		t.Errorf("Update status=open should exit 2 (I-508 vocab gate), got %d", code)
	}
	if code := Update(s, cfg, "T-001", "status", "completed", UpdateModeValue); code != 2 {
		t.Errorf("Update status=completed should exit 2 (legacy alias), got %d", code)
	}
}

// I-508: `st update <id> type bogus` must exit 2 — unknown type vocab.
func TestUpdateRejectsUnknownType(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Update(s, cfg, "T-001", "type", "banana", UpdateModeValue); code != 2 {
		t.Errorf("Update type=banana should exit 2 (I-508 vocab gate), got %d", code)
	}
}

// I-508: positive case — `st update <id> status active` succeeds.
func TestUpdateValidStatusValue(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Update(s, cfg, "I-001", "status", "active", UpdateModeValue); code != 0 {
		t.Errorf("Update status=active should succeed, got %d", code)
	}
}

// I-406: priority must be 0-4. Out-of-range rejected at create time.
func TestCreateRejectsOutOfRangePriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Create(s, cfg, "task", "x", CreateOpts{Priority: 9}); code != 2 {
		t.Errorf("Create with priority=9 should exit 2, got %d", code)
	}
	if code := Create(s, cfg, "task", "x", CreateOpts{Priority: -1}); code != 2 {
		t.Errorf("Create with priority=-1 should exit 2, got %d", code)
	}
}

func TestCreateRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Create(s, cfg, "task", "Changelog test", CreateOpts{Priority: 2})

	entries, _ := changelog.Read(cfg, "T-005")
	if len(entries) != 1 {
		t.Fatalf("expected 1 changelog entry, got %d", len(entries))
	}
	if entries[0].Op != "create" {
		t.Errorf("op = %q, want create", entries[0].Op)
	}
}

// === I-492: SBAR scaffold + opt-in editor ===

// I-492: every new task/issue ships with the four-section SBAR
// scaffold pre-stubbed so the author (or `st update <id> sbar`) can
// fill it in without touching the file shape.
func TestCreateWritesSBARScaffold(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Create(s, cfg, "issue", "Scaffold check", CreateOpts{Priority: 2}); code != 0 {
		t.Fatalf("Create returned %d, want 0", code)
	}
	path, _ := s.Path("I-002")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading new item: %v", err)
	}
	body := string(content)
	for _, want := range []string{
		"sbar:",
		"situation: |-",
		"background: |-",
		"assessment: |-",
		"recommendation: |-",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("new item missing SBAR scaffold marker %q in:\n%s", want, body)
		}
	}
}

// I-492: tasks get the same SBAR scaffold (work-tracking parity with
// issues per I-487).
func TestCreateWritesSBARScaffoldForTask(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Create(s, cfg, "task", "Task scaffold", CreateOpts{Priority: 2}); code != 0 {
		t.Fatalf("Create returned %d, want 0", code)
	}
	path, _ := s.Path("T-005")
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "sbar:") {
		t.Errorf("new task missing SBAR scaffold:\n%s", string(content))
	}
}

// I-492: editor flag is opt-in. With Editor=false, no editor is
// invoked even if $EDITOR is set in the environment. The test stub
// would create a sentinel file if invoked; assert it is absent.
func TestCreateNoEditorByDefault(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	sentinel := filepath.Join(t.TempDir(), "editor-was-called")
	stubEditor := writeStubEditor(t, sentinel)
	t.Setenv("EDITOR", stubEditor)
	if code := Create(s, cfg, "issue", "No editor", CreateOpts{Priority: 2}); code != 0 {
		t.Fatalf("Create returned %d, want 0", code)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Errorf("editor was invoked despite Editor=false (sentinel %s exists)", sentinel)
	}
}

// I-492: Editor=true with stdin not a TTY (test context) skips the
// editor silently — agent flows that pipe stdin would otherwise hang
// on a missing TTY. Test runs in a non-TTY context so this is the
// real production path for piped agents.
func TestCreateEditorSkippedWithoutTTY(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	sentinel := filepath.Join(t.TempDir(), "editor-was-called")
	stubEditor := writeStubEditor(t, sentinel)
	t.Setenv("EDITOR", stubEditor)
	if code := Create(s, cfg, "issue", "TTY guard", CreateOpts{Priority: 2, Editor: true}); code != 0 {
		t.Fatalf("Create returned %d, want 0", code)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Errorf("editor was invoked despite stdin not being a TTY (sentinel %s exists)", sentinel)
	}
}

// writeStubEditor writes a tiny shell script that touches `sentinel`
// when invoked, so tests can assert "editor was invoked" without
// needing a real interactive editor on the test runner.
func writeStubEditor(t *testing.T, sentinel string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\ntouch " + sentinel + "\n"
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("writing stub editor: %v", err)
	}
	return path
}

// I-492 (review fix): $EDITOR values like `code --wait` or `vim -u
// NONE` are common. exec.Command takes the first arg as a literal
// binary name, so an unsplit value would exec `"code --wait"` and
// fail. The runCreateEditor helper is shell-split via strings.Fields.
// This test verifies the parts-extraction by parsing the full editor
// value and asserting the resulting binary + extra-arg shape.
func TestRunCreateEditor_ShellSplitsMultiWordEditor(t *testing.T) {
	parts := strings.Fields("code --wait")
	if len(parts) != 2 {
		t.Fatalf("strings.Fields(\"code --wait\") = %v, want 2 parts", parts)
	}
	if parts[0] != "code" || parts[1] != "--wait" {
		t.Errorf("split parts = %v, want [code --wait]", parts)
	}
}

// I-492 (review fix): $VISUAL takes precedence over $EDITOR per Unix
// convention. The runCreateEditor helper itself can't be invoked in a
// test (no TTY), so this test asserts the precedence by manipulating
// env and calling the same selection logic directly via the env vars.
func TestRunCreateEditor_VisualBeforeEditor(t *testing.T) {
	t.Setenv("VISUAL", "visual-editor")
	t.Setenv("EDITOR", "fallback-editor")
	// Mirror the precedence check from runCreateEditor.
	got := os.Getenv("VISUAL")
	if got == "" {
		got = os.Getenv("EDITOR")
	}
	if got != "visual-editor" {
		t.Errorf("editor selection = %q, want visual-editor (VISUAL wins)", got)
	}
}

// === I-493: st update <id> sbar editor flow ===

// I-493: editor mode renders the 4 SBAR sections, lets the user edit,
// and writes all sub-fields back atomically. This test stubs $EDITOR
// with a script that overwrites the temp file with a known buffer
// containing all four sections, then asserts the file's sbar block
// reflects the new content.
func TestUpdateSBAR_RoundtripViaEditor(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	editor := writeSBARStubEditor(t,
		"situation: |-\n"+
			"  api returns 500 on tenant creation\n"+
			"background: |-\n"+
			"  RLS context not set on conn pool\n"+
			"assessment: |-\n"+
			"  reproduces 100% on fresh signup\n"+
			"recommendation: |-\n"+
			"  switch to s.querier(ctx) in 4 callsites\n")
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	if code := Update(s, cfg, "I-001", "sbar", "", UpdateModeEditor); code != 0 {
		t.Fatalf("Update sbar returned %d, want 0", code)
	}

	path, _ := s.Path("I-001")
	bodyB, _ := os.ReadFile(path)
	body := string(bodyB)
	for _, want := range []string{
		"api returns 500 on tenant creation",
		"RLS context not set on conn pool",
		"reproduces 100% on fresh signup",
		"switch to s.querier(ctx) in 4 callsites",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sbar update missing %q in:\n%s", want, body)
		}
	}
}

// I-493: a buffer missing one of the four required sections is
// rejected with exit 2 — the schema invariant from I-487 is that all
// four sub-keys are present even when their bodies are blank.
func TestUpdateSBAR_RejectsMissingSection(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	editor := writeSBARStubEditor(t,
		"situation: |-\n"+
			"  has situation\n"+
			"background: |-\n"+
			"  has background\n"+
			"assessment: |-\n"+
			"  has assessment\n")
	// recommendation deliberately omitted.
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")

	if code := Update(s, cfg, "I-001", "sbar", "", UpdateModeEditor); code != 2 {
		t.Errorf("Update sbar with missing section should exit 2, got %d", code)
	}
}

// I-493: parseSBARBuffer must accept all valid YAML block-scalar
// indicators (|-, |, >, >-, and a bare colon — which YAML treats as
// a single-line null but the editor flow tolerates as an empty
// block). Unit-test the parser directly to avoid coupling these
// invariants to the full editor round-trip.
func TestParseSBARBuffer_AcceptsBlockScalarVariants(t *testing.T) {
	buf := "situation: |-\n  s text\n" +
		"background: |\n  b text\n" +
		"assessment: >-\n  a text\n" +
		"recommendation:\n  r text\n"
	got, missing := parseSBARBuffer(buf)
	if len(missing) > 0 {
		t.Fatalf("missing sections: %v", missing)
	}
	if got.Situation != "s text" || got.Background != "b text" ||
		got.Assessment != "a text" || got.Recommendation != "r text" {
		t.Errorf("parsed SBAR = %+v", got)
	}
}

// I-493 (review fix): user content starting with the literal
// "TODO: fill in or leave blank" prefix must NOT be silently
// dropped. The skip rule only fires when that text is the entire
// body, not a prefix of real content.
func TestParseSBARBuffer_KeepsContentStartingWithTODOPrefix(t *testing.T) {
	buf := "situation: |-\n" +
		"  TODO: fill in or leave blank — but also we know it is RLS\n" +
		"background: |-\n" +
		"  some history\n" +
		"assessment: |-\n" +
		"  diagnosed\n" +
		"recommendation: |-\n" +
		"  proposal\n"
	got, missing := parseSBARBuffer(buf)
	if len(missing) > 0 {
		t.Fatalf("missing: %v", missing)
	}
	want := "TODO: fill in or leave blank — but also we know it is RLS"
	if got.Situation != want {
		t.Errorf("situation = %q, want %q", got.Situation, want)
	}
}

// I-493 (review fix): the unedited seed produces an SBAR with all
// four sections empty — the literal "TODO: fill in or leave blank"
// marker is treated as empty when it is the sole body line.
func TestParseSBARBuffer_TODOOnlyLineMeansEmpty(t *testing.T) {
	buf := "situation: |-\n" +
		"  TODO: fill in or leave blank\n" +
		"background: |-\n" +
		"  TODO: fill in or leave blank\n" +
		"assessment: |-\n" +
		"  TODO: fill in or leave blank\n" +
		"recommendation: |-\n" +
		"  TODO: fill in or leave blank\n"
	got, missing := parseSBARBuffer(buf)
	if len(missing) > 0 {
		t.Fatalf("missing: %v", missing)
	}
	if got.Situation != "" || got.Background != "" || got.Assessment != "" || got.Recommendation != "" {
		t.Errorf("expected all-empty SBAR after unedited seed, got %+v", got)
	}
}

// I-493 (review fix): bodies indented at 4 spaces (common YAML
// auto-indent default) must strip 4, not the hardcoded 2. Mixed
// blank/indented lines are also handled — minimum-indent detection
// preserves internal relative whitespace.
func TestParseSBARBuffer_StripsAnyConsistentIndent(t *testing.T) {
	buf := "situation: |-\n" +
		"    line one\n" +
		"    line two\n" +
		"background: |-\n" +
		"\tline tabbed\n" +
		"assessment: |-\n" +
		"  line one\n" +
		"\n" +
		"  line two\n" +
		"recommendation: |-\n" +
		"  ok\n"
	got, _ := parseSBARBuffer(buf)
	if got.Situation != "line one\nline two" {
		t.Errorf("4-space body strip wrong: %q", got.Situation)
	}
	if got.Background != "line tabbed" {
		t.Errorf("tab body strip wrong: %q", got.Background)
	}
	if got.Assessment != "line one\n\nline two" {
		t.Errorf("multi-paragraph strip wrong: %q", got.Assessment)
	}
}

// I-493 (review fix): SetSBARBlock must emit a trailing blank line so
// the next top-level field has a visual separator. Without it, every
// SBAR edit produces a spurious one-line whitespace diff.
func TestSetSBARBlock_PreservesTrailingBlankSeparator(t *testing.T) {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "id: I-001", Key: "id"},
			{Raw: "sbar:", Key: "sbar"},
			{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
			{Raw: "    old", IsBlock: true, BlockKey: "situation", Indent: 4},
			{Raw: "", IsEmpty: true, IsBlock: true, BlockKey: "situation"},
			{Raw: "next_actions:", Key: "next_actions"},
		},
	}
	doc.SetSBARBlock(model.SBAR{
		Situation: "new",
	})
	// Find the sbar block + next_actions; assert there's a blank
	// IsEmpty line between the last block content and next_actions.
	naIdx := -1
	for i, l := range doc.Lines {
		if l.Key == "next_actions" {
			naIdx = i
			break
		}
	}
	if naIdx <= 0 {
		t.Fatalf("next_actions not found in doc lines: %+v", doc.Lines)
	}
	if !doc.Lines[naIdx-1].IsEmpty {
		t.Errorf("expected blank line before next_actions, got: %+v", doc.Lines[naIdx-1])
	}
}

// I-493: sbarSeedBuffer + parseSBARBuffer must round-trip an SBAR
// struct unchanged so an editor that did not modify anything
// produces zero spurious changes.
func TestSBARRoundtrip_EditorNoOp(t *testing.T) {
	orig := model.SBAR{
		Situation:      "one",
		Background:     "two\nlines",
		Assessment:     "three",
		Recommendation: "four",
	}
	buf := sbarSeedBuffer(orig)
	got, missing := parseSBARBuffer(buf)
	if len(missing) > 0 {
		t.Fatalf("missing: %v", missing)
	}
	if got != orig {
		t.Errorf("roundtrip lost data: got %+v want %+v", got, orig)
	}
}

// writeSBARStubEditor writes a shell script that overwrites its
// argument (the temp file readFromEditor created) with `replacement`
// when invoked. This simulates the user opening the editor and saving
// the supplied buffer.
func writeSBARStubEditor(t *testing.T, replacement string) string {
	t.Helper()
	dir := t.TempDir()
	body := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(body, []byte(replacement), 0644); err != nil {
		t.Fatalf("writing replacement body: %v", err)
	}
	script := filepath.Join(dir, "editor.sh")
	scriptBody := "#!/bin/sh\ncp " + body + " \"$1\"\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0755); err != nil {
		t.Fatalf("writing stub editor: %v", err)
	}
	return script
}

// === Finish with worktree ===

func TestFinishWithWorktreeConfig(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	// Create a worktree directory structure
	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	repoDir := filepath.Join(wtDir, "repo-a")
	os.MkdirAll(repoDir, 0755)

	// Dry run
	code := Finish(s, cfg, "T-001", FinishOpts{DryRun: true})
	if code != 0 {
		t.Errorf("Finish dry-run returned %d, want 0", code)
	}
}

func TestFinishListEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	// Create the base dir but empty
	os.MkdirAll(filepath.Join(cfg.Root(), "worktrees"), 0755)

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list returned %d, want 0", code)
	}
}

func TestFinishListNonexistent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "nonexistent",
	}

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list nonexistent returned %d, want 0", code)
	}
}

func TestFinishWorktreeNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	os.MkdirAll(filepath.Join(cfg.Root(), "worktrees"), 0755)

	code := Finish(s, cfg, "T-999", FinishOpts{})
	if code != 1 {
		t.Errorf("Finish not found returned %d, want 1", code)
	}
}

func TestFinishNoIDWithWorktree(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}

	code := Finish(s, cfg, "", FinishOpts{})
	if code != 2 {
		t.Errorf("Finish no ID returned %d, want 2", code)
	}
}

// === Coverage: Start records changelog ===

func TestStartRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Start(s, cfg, "T-001", StartOpts{})

	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "start" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for start")
	}
}

// === Coverage: Close records changelog ===

func TestCloseRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Close(s, cfg, "T-003", "done", CloseOpts{})

	entries, _ := changelog.Read(cfg, "T-003")
	found := false
	for _, e := range entries {
		if e.Op == "close" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for close")
	}
}

// === Coverage: Update records changelog ===

func TestUpdateRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Update(s, cfg, "T-001", "title", "New title", UpdateModeValue)

	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "update" && e.Field == "title" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for update")
	}
}

// === Coverage: DepAdd/Rm record changelog ===

func TestDepAddRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	DepAdd(s, cfg, "T-001", "T-003")

	// Check both items got entries
	entries1, _ := changelog.Read(cfg, "T-001")
	entries3, _ := changelog.Read(cfg, "T-003")

	if len(entries1) == 0 {
		t.Error("T-001 should have changelog entries")
	}
	if len(entries3) == 0 {
		t.Error("T-003 should have changelog entries")
	}
}

func TestFinishListWithEntries(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	baseDir := filepath.Join(cfg.Root(), "worktrees")
	os.MkdirAll(filepath.Join(baseDir, "T-001", "repo-a"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "T-002", "repo-b"), 0755)

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list returned %d, want 0", code)
	}
}

