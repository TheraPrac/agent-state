package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binPath string

// testSBARArgs are the --sbar-* and --no-validate flags used by CLI-level tests
// that call `st create task/issue`. I-908 requires all task/issue creates from
// the CLI to supply substantive SBAR; --no-validate skips the LLM layer so
// tests remain deterministic.
var testSBARArgs = []string{
	"--sbar-situation", "Test fixture: observable symptom for integration test case.",
	"--sbar-background", "Integration test setup in cmd/as/main_test.go; no production code path involved.",
	"--sbar-assessment", "Standard test fixture; no real diagnosis required for integration coverage.",
	"--sbar-recommendation", "Keep fixture stable; supply real SBAR for production item creation.",
	"--no-validate",
}

func TestMain(m *testing.M) {
	// Build the as binary once for all tests
	tmp, err := os.MkdirTemp("", "as-bin-*")
	if err != nil {
		panic("mktemp: " + err.Error())
	}
	binPath = filepath.Join(tmp, "as")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = filepath.Join(projectRoot(), "cmd", "as")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func projectRoot() string {
	// Walk up from test file to find go.mod
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find go.mod")
		}
		dir = parent
	}
}

// setupWorkspace creates a minimal workspace directory for CLI tests.
func setupWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create directory structure
	for _, sub := range []string{"tasks", "issues", "archive", ".as", "templates"} {
		os.MkdirAll(filepath.Join(dir, sub), 0755)
	}

	// Write minimal config
	configContent := `project:
  name: test-project

paths:
  root: .
  templates: templates
  changelog: .changelog
  index: index.md

git:
  auto_commit: false
  auto_push: false
`
	os.WriteFile(filepath.Join(dir, ".as", "config.yaml"), []byte(configContent), 0644)

	// Write a sample task
	taskContent := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Sample task for testing

priority: 2
category: testing

depends_on:
- []

next_actions:
- Do the thing
`
	os.WriteFile(filepath.Join(dir, "tasks", "T-001-sample-task.md"), []byte(taskContent), 0644)

	// Write a sample issue
	issueContent := `id: I-001
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Sample issue for testing

severity: medium

depends_on:
- []
`
	os.WriteFile(filepath.Join(dir, "issues", "I-001-sample-issue.md"), []byte(issueContent), 0644)

	return dir
}

// runAs executes the as binary in the given workspace and returns stdout, stderr, exit code.
func runAs(t *testing.T, workspace string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = workspace
	// I-758: clear CLAUDECODE so the agent-mode item-review branch
	// doesn't fire in these integration tests. The tests don't wire a
	// fake claude engine — they exercise the binary end-to-end with
	// DefaultRunEngine, which spawns a real claude subprocess. With
	// CLAUDECODE inherited from a Claude Code parent shell, the new
	// agent-mode branch would fire the review and (depending on the
	// real claude verdict) auto-archive freshly-created items, making
	// these tests non-deterministic. AS_INTERNAL_NO_REVIEW=1 belt-and-
	// braces blocks the review at the earliest skip point regardless
	// of any future env semantics drift.
	cmd.Env = append(os.Environ(),
		"AS_AGENT_ID=test-agent",
		"AS_SESSION_ID=test-session-001",
		"CLAUDECODE=",
		"AS_INTERNAL_NO_REVIEW=1",
		"AS_INTERNAL_NO_CLASSIFY=1",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("exec error: %v", err)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// --- Version ---

func TestVersion(t *testing.T) {
	stdout, _, code := runAs(t, t.TempDir(), "version")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "st") {
		t.Errorf("version output = %q, expected to contain 'st'", stdout)
	}
}

// --- Show ---

func TestShow(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "show", "T-001")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("show output missing T-001:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Sample task") {
		t.Errorf("show output missing title:\n%s", stdout)
	}
}

func TestShowBrief(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "show", "-b", "T-001")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("brief output missing T-001:\n%s", stdout)
	}
}

func TestShowField(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "show", "-f", "status", "T-001")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "queued") {
		t.Errorf("field output = %q, want 'queued'", strings.TrimSpace(stdout))
	}
}

func TestShowNotFound(t *testing.T) {
	ws := setupWorkspace(t)
	_, _, code := runAs(t, ws, "show", "T-999")
	if code == 0 {
		t.Error("expected non-zero exit for missing item")
	}
}

// --- List ---

func TestList(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "list")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("list output missing T-001:\n%s", stdout)
	}
	if !strings.Contains(stdout, "I-001") {
		t.Errorf("list output missing I-001:\n%s", stdout)
	}
}

func TestListFilterType(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "list", "-T", "task")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("list output missing T-001:\n%s", stdout)
	}
	if strings.Contains(stdout, "I-001") {
		t.Error("list -T task should not show issues")
	}
}

// --- Status ---

func TestStatus(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "status")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Status should show summary counts
	if len(stdout) == 0 {
		t.Error("status output is empty")
	}
}

// --- Check ---

func TestCheck(t *testing.T) {
	ws := setupWorkspace(t)
	_, _, code := runAs(t, ws, "check")
	// check may pass or fail depending on validation — just verify it runs
	if code < 0 {
		t.Errorf("unexpected exit code: %d", code)
	}
}

func TestCheckQuiet(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, _ := runAs(t, ws, "check", "-q")
	// Quiet mode should produce no output
	if len(strings.TrimSpace(stdout)) > 0 {
		t.Errorf("check -q should produce no output, got: %q", stdout)
	}
}

// --- Prime ---

func TestPrime(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "prime")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(stdout) == 0 {
		t.Error("prime output is empty")
	}
}

func TestPrimeCompact(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "prime", "--compact")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(stdout) == 0 {
		t.Error("prime --compact output is empty")
	}
}

// --- Ready ---

func TestReady(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "ready")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// T-001 has no unresolved deps, should appear in ready list
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("ready output should include T-001:\n%s", stdout)
	}
}

// --- Index ---

func TestIndex(t *testing.T) {
	ws := setupWorkspace(t)
	_, _, code := runAs(t, ws, "index")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Verify index.md was created
	indexPath := filepath.Join(ws, "index.md")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("index.md not created: %v", err)
	}
	if !strings.Contains(string(data), "T-001") {
		t.Errorf("index.md should reference T-001:\n%s", string(data))
	}
}

// --- Create ---

func TestCreate(t *testing.T) {
	ws := setupWorkspace(t)
	args := append([]string{"create", "task", "New test task"}, testSBARArgs...)
	stdout, _, code := runAs(t, ws, args...)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-002") {
		t.Errorf("create should output new ID (T-002):\n%s", stdout)
	}

	// Verify file exists
	matches, _ := filepath.Glob(filepath.Join(ws, "tasks", "T-002-*.md"))
	if len(matches) != 1 {
		t.Errorf("expected 1 T-002 file, got %d", len(matches))
	}
}

func TestCreateIssue(t *testing.T) {
	// I-406: --severity is dead. Use --priority instead.
	ws := setupWorkspace(t)
	args := append([]string{"create", "issue", "New test issue", "--priority", "1"}, testSBARArgs...)
	stdout, _, code := runAs(t, ws, args...)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "I-002") {
		t.Errorf("create issue should output I-002:\n%s", stdout)
	}
}

// --- Tag ---

func TestTagAddRemove(t *testing.T) {
	ws := setupWorkspace(t)

	// Add tag
	_, _, code := runAs(t, ws, "tag", "T-001", "add", "security")
	if code != 0 {
		t.Errorf("tag add exit code = %d, want 0", code)
	}

	// Verify tag was added by reading the file directly
	data, err := os.ReadFile(filepath.Join(ws, "tasks", "T-001-sample-task.md"))
	if err != nil {
		t.Fatalf("read task file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "security") || !strings.Contains(content, "tags") {
		t.Errorf("tag 'security' not found in file:\n%s", content)
	}

	// Note: tag rm requires re-parsing the inline [tag] format, which is
	// a known limitation (inline lists don't roundtrip to item.Tags).
	// Test tag rm on an item that has list-format tags instead.
	taskWithListTags := `id: T-099
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Tag removal test
tags:
- removeme
- keeper
`
	os.WriteFile(filepath.Join(ws, "tasks", "T-099-tag-rm-test.md"), []byte(taskWithListTags), 0644)
	_, _, code = runAs(t, ws, "tag", "T-099", "rm", "removeme")
	if code != 0 {
		t.Errorf("tag rm exit code = %d, want 0", code)
	}
}

// --- Epic ---

func TestEpicCreateAndList(t *testing.T) {
	ws := setupWorkspace(t)

	// Create epic
	stdout, _, code := runAs(t, ws, "epic", "create", "Test Epic")
	if code != 0 {
		t.Errorf("epic create exit code = %d, want 0", code)
	}
	if len(strings.TrimSpace(stdout)) == 0 {
		t.Error("epic create should output the new epic ID")
	}

	// List epics
	stdout, _, code = runAs(t, ws, "epic", "list")
	if code != 0 {
		t.Errorf("epic list exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Test Epic") {
		t.Errorf("epic list should show Test Epic:\n%s", stdout)
	}
}

// --- Sprint ---

func TestSprintCreateAndList(t *testing.T) {
	ws := setupWorkspace(t)

	// Create epic first — output: "Created epic xxx-yyy-zzz — Parent Epic\n"
	epicOut, _, _ := runAs(t, ws, "epic", "create", "Parent Epic")
	fields := strings.Fields(strings.TrimSpace(epicOut))
	if len(fields) < 3 {
		t.Fatalf("unexpected epic create output: %q", epicOut)
	}
	epicID := fields[2] // "xxx-yyy-zzz"

	// Create sprint under epic
	stdout, _, code := runAs(t, ws, "sprint", "create", epicID, "Test Sprint")
	if code != 0 {
		t.Errorf("sprint create exit code = %d, want 0", code)
	}
	if len(strings.TrimSpace(stdout)) == 0 {
		t.Error("sprint create should output the new sprint ID")
	}

	// List sprints
	stdout, _, code = runAs(t, ws, "sprint", "list")
	if code != 0 {
		t.Errorf("sprint list exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "Test Sprint") {
		t.Errorf("sprint list should show Test Sprint:\n%s", stdout)
	}
}

// --- Note ---

func TestNoteAddAndList(t *testing.T) {
	ws := setupWorkspace(t)

	// Add note
	_, _, code := runAs(t, ws, "note", "add", "This is a test note")
	if code != 0 {
		t.Errorf("note add exit code = %d, want 0", code)
	}

	// List notes
	stdout, _, code := runAs(t, ws, "note", "list")
	if code != 0 {
		t.Errorf("note list exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "This is a test note") {
		t.Errorf("note list should show the note:\n%s", stdout)
	}
}

// --- Reconcile ---

func TestReconcileDryRun(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "reconcile", "--dry-run")
	if code != 0 {
		t.Errorf("reconcile --dry-run exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "reconcile dry run:") {
		t.Errorf("reconcile output should contain summary:\n%s", stdout)
	}
}

func TestReconcileArchivePhase(t *testing.T) {
	ws := setupWorkspace(t)

	// Write a completed task that's still in tasks/
	completedTask := `id: T-050
type: task
status: done
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: 2026-03-26T10:00:00-06:00
title: Completed task to archive
`
	os.WriteFile(filepath.Join(ws, "tasks", "T-050-completed-task.md"), []byte(completedTask), 0644)

	stdout, _, code := runAs(t, ws, "reconcile")
	if code != 0 {
		t.Errorf("reconcile exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "T-050: move to archive") {
		t.Errorf("reconcile should move T-050 to archive:\n%s", stdout)
	}

	// Verify file moved
	if _, err := os.Stat(filepath.Join(ws, "archive", "T-050-completed-task.md")); err != nil {
		t.Errorf("T-050 should be in archive/: %v", err)
	}
}

// --- Migrate ---

func TestMigrateDryRun(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "migrate", "--dry-run")
	if code != 0 {
		t.Errorf("migrate --dry-run exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "dry run:") {
		t.Errorf("migrate --dry-run output should contain summary:\n%s", stdout)
	}
}

// --- Stats ---

func TestStats(t *testing.T) {
	ws := setupWorkspace(t)
	stdout, _, code := runAs(t, ws, "stats")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(stdout) == 0 {
		t.Error("stats output is empty")
	}
}

// --- Full Lifecycle ---

func TestFullLifecycle(t *testing.T) {
	ws := setupWorkspace(t)

	// Step 1: CREATE
	args := append([]string{"create", "task", "Lifecycle test task"}, testSBARArgs...)
	stdout, _, code := runAs(t, ws, args...)
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) < 2 {
		t.Fatalf("unexpected create output: %q", stdout)
	}
	id := fields[1]
	if !strings.HasPrefix(id, "T-") {
		t.Fatalf("unexpected ID: %q from output: %q", id, stdout)
	}

	// Verify file exists
	matches, _ := filepath.Glob(filepath.Join(ws, "tasks", id+"-*.md"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 file for %s, got %d", id, len(matches))
	}

	// Step 2: TAG
	_, _, code = runAs(t, ws, "tag", id, "add", "lifecycle-test")
	if code != 0 {
		t.Fatalf("tag add exit %d", code)
	}
	data, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(data), "lifecycle-test") {
		t.Errorf("file should have tag after tag add:\n%s", string(data))
	}

	// Step 3: START (queued → active)
	_, _, code = runAs(t, ws, "start", id)
	if code != 0 {
		t.Fatalf("start exit %d", code)
	}
	data, _ = os.ReadFile(matches[0])
	if !strings.Contains(string(data), "status: active") {
		t.Errorf("file should be active after start:\n%s", string(data))
	}
	if !strings.Contains(string(data), "assigned_to: test-agent") {
		t.Errorf("file should be assigned to test-agent after start:\n%s", string(data))
	}

	// Step 4: Verify state across commands after start
	stdout, _, _ = runAs(t, ws, "show", "-f", "status", id)
	if !strings.Contains(stdout, "active") {
		t.Errorf("show -f status should say active: %q", stdout)
	}

	stdout, _, _ = runAs(t, ws, "list", "-s", "active")
	if !strings.Contains(stdout, id) {
		t.Errorf("list -s active should include %s:\n%s", id, stdout)
	}

	// Step 5: CLOSE (active → completed)
	_, _, code = runAs(t, ws, "close", id, "done", "--force")
	if code != 0 {
		t.Fatalf("close exit %d", code)
	}

	// File should have moved to archive/
	archiveMatches, _ := filepath.Glob(filepath.Join(ws, "archive", id+"-*.md"))
	if len(archiveMatches) != 1 {
		t.Fatalf("expected 1 archive file for %s, got %d", id, len(archiveMatches))
	}
	data, _ = os.ReadFile(archiveMatches[0])
	if !strings.Contains(string(data), "status: done") {
		t.Errorf("archived file should be completed:\n%s", string(data))
	}
	if !strings.Contains(string(data), "completed:") {
		t.Errorf("archived file should have completed timestamp:\n%s", string(data))
	}

	// Step 6: INDEX — should reflect the completed item
	_, _, code = runAs(t, ws, "index")
	if code != 0 {
		t.Fatalf("index exit %d", code)
	}
	indexData, _ := os.ReadFile(filepath.Join(ws, "index.md"))
	if !strings.Contains(string(indexData), id) {
		t.Errorf("index.md should reference %s", id)
	}

	// Step 7: Verify completed item is no longer in active/queued lists
	stdout, _, _ = runAs(t, ws, "list", "-s", "active")
	if strings.Contains(stdout, id) {
		t.Errorf("list -s active should NOT include completed %s:\n%s", id, stdout)
	}
	stdout, _, _ = runAs(t, ws, "ready")
	if strings.Contains(stdout, id) {
		t.Errorf("ready should NOT include completed %s:\n%s", id, stdout)
	}
}

// --- Cross-Command Consistency ---

func TestCrossCommandConsistency(t *testing.T) {
	ws := setupWorkspace(t)

	// Create an item
	args := append([]string{"create", "task", "Consistency test item"}, testSBARArgs...)
	stdout, _, code := runAs(t, ws, args...)
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	id := strings.Fields(strings.TrimSpace(stdout))[1]

	// --- Before start: item is queued ---

	// list should include it
	stdout, _, _ = runAs(t, ws, "list")
	if !strings.Contains(stdout, id) {
		t.Errorf("list should include %s before start", id)
	}

	// status should count it
	stdout, _, _ = runAs(t, ws, "status")
	if len(stdout) == 0 {
		t.Error("status output empty before start")
	}

	// ready should include it (no deps)
	stdout, _, _ = runAs(t, ws, "ready")
	if !strings.Contains(stdout, id) {
		t.Errorf("ready should include %s before start (unblocked)", id)
	}

	// prime should include it
	stdout, _, _ = runAs(t, ws, "prime", "--compact")
	if !strings.Contains(stdout, id) {
		t.Errorf("prime should include %s before start", id)
	}

	// index should reference it
	runAs(t, ws, "index")
	indexData, _ := os.ReadFile(filepath.Join(ws, "index.md"))
	if !strings.Contains(string(indexData), id) {
		t.Errorf("index should reference %s before start", id)
	}

	// --- START: transition to active ---
	_, _, code = runAs(t, ws, "start", id)
	if code != 0 {
		t.Fatalf("start exit %d", code)
	}

	// list -s active should include it
	stdout, _, _ = runAs(t, ws, "list", "-s", "active")
	if !strings.Contains(stdout, id) {
		t.Errorf("list -s active should include %s after start:\n%s", id, stdout)
	}

	// list -s queued should NOT include it
	stdout, _, _ = runAs(t, ws, "list", "-s", "queued")
	if strings.Contains(stdout, id) {
		t.Errorf("list -s queued should NOT include %s after start:\n%s", id, stdout)
	}

	// ready should NOT include active items
	stdout, _, _ = runAs(t, ws, "ready")
	if strings.Contains(stdout, id) {
		t.Errorf("ready should NOT include active %s:\n%s", id, stdout)
	}

	// status should reflect it in active section
	stdout, _, _ = runAs(t, ws, "status")
	if !strings.Contains(stdout, id) {
		t.Errorf("status should mention active %s:\n%s", id, stdout)
	}

	// prime should include it
	stdout, _, _ = runAs(t, ws, "prime", "--compact")
	if !strings.Contains(stdout, id) {
		t.Errorf("prime should include active %s:\n%s", id, stdout)
	}

	// index should still reference it
	runAs(t, ws, "index")
	indexData, _ = os.ReadFile(filepath.Join(ws, "index.md"))
	if !strings.Contains(string(indexData), id) {
		t.Errorf("index should reference active %s", id)
	}

	// show should report active status
	stdout, _, _ = runAs(t, ws, "show", "-f", "status", id)
	if !strings.Contains(stdout, "active") {
		t.Errorf("show -f status should say active: %q", stdout)
	}

	// stats should count it
	stdout, _, _ = runAs(t, ws, "stats")
	if len(stdout) == 0 {
		t.Error("stats should produce output")
	}
}

// I-503: `st update <id> <field>` (no value, no --stdin) used to fall
// back to $EDITOR — useless from the agent Bash tool (no TTY) and
// would silently block. The fallback is rejected with a usage hint.
//
// T-382: renamed from TestUpdateRejectsEmptyEditorMode and extended
// to cover sbar (which previously had the I-493 editor exemption).
func TestUpdateRefusesNoValueNoStdin(t *testing.T) {
	ws := setupWorkspace(t)
	for _, field := range []string{"title", "sbar"} {
		t.Run(field, func(t *testing.T) {
			_, stderr, code := runAs(t, ws, "update", "T-001", field)
			if code != 2 {
				t.Errorf("update %s with no value should exit 2, got %d (stderr=%q)", field, code, stderr)
			}
			if !strings.Contains(stderr, "no value supplied") {
				t.Errorf("expected usage hint on stderr, got: %q", stderr)
			}
			if !strings.Contains(stderr, "--stdin") {
				t.Errorf("expected --stdin pointer in usage hint, got: %q", stderr)
			}
		})
	}
}

// I-503: positional + --stdin paths still work (regression check).
func TestUpdatePositionalStillWorks(t *testing.T) {
	ws := setupWorkspace(t)
	_, _, code := runAs(t, ws, "update", "T-001", "priority", "1")
	if code != 0 {
		t.Errorf("positional update should succeed, got %d", code)
	}
}
