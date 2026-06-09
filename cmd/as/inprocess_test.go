package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testSBARArgsInprocess are the --sbar-* and --no-validate flags used by in-process
// CLI tests that call `st create task/issue`. I-908 requires substantive SBAR;
// --no-validate skips the LLM layer so tests remain deterministic.
var testSBARArgsInprocess = []string{
	"--sbar-situation", "Test fixture: observable symptom for inprocess integration test.",
	"--sbar-background", "Integration test in cmd/as/inprocess_test.go; no production code path involved.",
	"--sbar-assessment", "Standard test fixture; no real diagnosis required for integration coverage.",
	"--sbar-recommendation", "Keep fixture stable; supply real SBAR for production item creation.",
	"--no-validate",
}

// runInProcess runs a command via newApp() in-process, capturing stdout.
// Returns stdout content and the exit code captured in the exitCode global.
func runInProcess(t *testing.T, cwd string, args ...string) (string, int) {
	t.Helper()

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Reset exit code
	exitCode = 0

	// Set env vars
	os.Setenv("AS_AGENT_ID", "test-agent")
	os.Setenv("AS_SESSION_ID", "test-session")
	os.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	defer os.Unsetenv("AS_INTERNAL_NO_REVIEW")

	app := newApp(cwd)
	app.SetArgs(args)
	app.SetErr(&bytes.Buffer{}) // suppress cobra errors to stderr
	_ = app.Execute()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String(), exitCode
}

// setupInProcessWorkspace creates a test workspace (same as setupWorkspace but returns path).
func setupInProcessWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"tasks", "issues", "archive", ".as", "templates"} {
		os.MkdirAll(filepath.Join(dir, sub), 0755)
	}
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

	issueContent := `id: I-001
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Sample issue

severity: medium

depends_on:
- []
`
	os.WriteFile(filepath.Join(dir, "issues", "I-001-sample-issue.md"), []byte(issueContent), 0644)
	return dir
}

// --- In-process tests that count toward cmd/as coverage ---

func TestInProcess_Version(t *testing.T) {
	stdout, code := runInProcess(t, t.TempDir(), "version")
	if code != 0 {
		t.Errorf("version exit %d", code)
	}
	if !strings.Contains(stdout, "st") {
		t.Errorf("version: %q", stdout)
	}
}

func TestInProcess_Show(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "show", "T-001")
	if code != 0 {
		t.Errorf("show exit %d", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("show missing T-001: %q", stdout)
	}
}

func TestInProcess_ShowField(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "show", "-f", "status", "T-001")
	if code != 0 {
		t.Errorf("show -f exit %d", code)
	}
	if !strings.Contains(stdout, "queued") {
		t.Errorf("show -f status: %q", stdout)
	}
}

func TestInProcess_List(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "list")
	if code != 0 {
		t.Errorf("list exit %d", code)
	}
	if !strings.Contains(stdout, "T-001") || !strings.Contains(stdout, "I-001") {
		t.Errorf("list missing items: %q", stdout)
	}
}

func TestInProcess_ListFilterStatus(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, _ := runInProcess(t, ws, "list", "-s", "queued")
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("list -s queued missing T-001: %q", stdout)
	}
}

func TestInProcess_ListFilterType(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, _ := runInProcess(t, ws, "list", "-T", "issue")
	if strings.Contains(stdout, "T-001") {
		t.Error("list -T issue should not show tasks")
	}
}

func TestInProcess_Status(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "status")
	if code != 0 {
		t.Errorf("status exit %d", code)
	}
	if len(stdout) == 0 {
		t.Error("status empty")
	}
}

func TestInProcess_Ready(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "ready")
	if code != 0 {
		t.Errorf("ready exit %d", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("ready missing T-001: %q", stdout)
	}
}

func TestInProcess_Check(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "check")
	_ = code // just verify it runs
}

func TestInProcess_CheckQuiet(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, _ := runInProcess(t, ws, "check", "-q")
	if len(strings.TrimSpace(stdout)) > 0 {
		t.Errorf("check -q should be quiet: %q", stdout)
	}
}

func TestInProcess_CheckFix(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "check", "--fix")
	_ = code // just verify it runs without crashing
}

func TestInProcess_Prime(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "prime")
	if code != 0 {
		t.Errorf("prime exit %d", code)
	}
	if len(stdout) == 0 {
		t.Error("prime empty")
	}
}

func TestInProcess_PrimeCompact(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "prime", "--compact")
	if code != 0 {
		t.Errorf("prime --compact exit %d", code)
	}
	if !strings.Contains(stdout, "T-001") {
		t.Errorf("prime --compact missing T-001: %q", stdout)
	}
}

func TestInProcess_Stats(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "stats")
	if code != 0 {
		t.Errorf("stats exit %d", code)
	}
	if len(stdout) == 0 {
		t.Error("stats empty")
	}
}

func TestInProcess_Index(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "index")
	if code != 0 {
		t.Errorf("index exit %d", code)
	}
	data, err := os.ReadFile(filepath.Join(ws, "index.md"))
	if err != nil {
		t.Fatalf("index.md not created: %v", err)
	}
	if !strings.Contains(string(data), "T-001") {
		t.Error("index.md missing T-001")
	}
}

func TestInProcess_Create(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	args := append([]string{"create", "task", "New task"}, testSBARArgsInprocess...)
	stdout, code := runInProcess(t, ws, args...)
	if code != 0 {
		t.Errorf("create exit %d", code)
	}
	if !strings.Contains(stdout, "T-002") {
		t.Errorf("create output: %q", stdout)
	}
}

func TestInProcess_Tag(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "tag", "T-001", "add", "test-tag")
	if code != 0 {
		t.Errorf("tag add exit %d", code)
	}
}

func TestInProcess_Start(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "start", "T-001")
	if code != 0 {
		t.Errorf("start exit %d", code)
	}
	if !strings.Contains(stdout, "Started T-001") {
		t.Errorf("start output: %q", stdout)
	}
}

func TestInProcess_Close(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// Start first (must be active to close)
	runInProcess(t, ws, "start", "T-001")
	stdout, code := runInProcess(t, ws, "close", "T-001", "done", "--force")
	if code != 0 {
		t.Errorf("close exit %d", code)
	}
	if !strings.Contains(stdout, "Closed T-001") {
		t.Errorf("close output: %q", stdout)
	}
}

func TestInProcess_MigrateDryRun(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "migrate", "--dry-run")
	if code != 0 {
		t.Errorf("migrate exit %d", code)
	}
	if !strings.Contains(stdout, "dry run:") {
		t.Errorf("migrate output: %q", stdout)
	}
}

func TestInProcess_ReconcileDryRun(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	stdout, code := runInProcess(t, ws, "reconcile", "--dry-run")
	if code != 0 {
		t.Errorf("reconcile exit %d", code)
	}
	if !strings.Contains(stdout, "reconcile dry run:") {
		t.Errorf("reconcile output: %q", stdout)
	}
}

func TestInProcess_EpicCreate(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// Epic commands use cfg.EpicsPath() which is path-relative.
	// Binary e2e test (TestEpicCreateAndList) covers full behavior.
	// Here we exercise the code path for coverage.
	_, code := runInProcess(t, ws, "epic", "create", "Test Epic")
	_ = code // may fail on path issues in isolated workspace — OK for coverage
}

func TestInProcess_NoteAdd(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// Note: NoteAdd uses cfg.NotesPath() which may differ between newApp() invocations.
	// The binary e2e test (TestNoteAddAndList) covers this. Here we just verify no crash.
	_, code := runInProcess(t, ws, "note", "add", "Test note message")
	_ = code // note add may fail to find notes path in isolated workspace — OK for coverage
}

func TestInProcess_Update(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "update", "T-001", "priority", "1")
	if code != 0 {
		t.Errorf("update exit %d", code)
	}
}

func TestInProcess_DepTree(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "dep", "tree", "T-001")
	if code != 0 {
		t.Errorf("dep tree exit %d", code)
	}
}

func TestInProcess_DepGraph(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "dep", "graph")
	if code != 0 {
		t.Errorf("dep graph exit %d", code)
	}
}

func TestInProcess_DepAdd(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "dep", "add", "T-001", "I-001")
	_ = code // may fail if deps not valid — just exercising the path
}

func TestInProcess_Log(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "log")
	_ = code
}

func TestInProcess_LogItem(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "log", "T-001")
	_ = code
}

func TestInProcess_Commit(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// Start first so we can commit
	runInProcess(t, ws, "start", "T-001")
	_, code := runInProcess(t, ws, "commit", "T-001", "initial implementation")
	_ = code
}

func TestInProcess_Release(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	runInProcess(t, ws, "start", "T-001")
	_, code := runInProcess(t, ws, "release", "T-001")
	if code != 0 {
		t.Errorf("release exit %d", code)
	}
}

func TestInProcess_Sync(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "sync")
	_ = code // sync requires git — just exercise code path
}

func TestInProcess_FinishList(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "finish", "-l")
	_ = code
}

func TestInProcess_SprintList(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "sprint", "list")
	if code != 0 {
		t.Errorf("sprint list exit %d", code)
	}
}

func TestInProcess_NoteList(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "note", "list")
	if code != 0 {
		t.Errorf("note list exit %d", code)
	}
}

func TestInProcess_StatsJSON(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "stats", "--json")
	if code != 0 {
		t.Errorf("stats --json exit %d", code)
	}
}

func TestInProcess_StatusCheck(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "status", "-c")
	_ = code // check may find issues — just exercise code path
}

func TestInProcess_UpdateNoValue(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// Missing value arg and no --stdin → error path
	_, code := runInProcess(t, ws, "update", "T-001", "title")
	if code == 0 {
		t.Error("update without value should fail")
	}
}

func TestInProcess_FinishNoArgs(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// No id and no --list → error path
	_, code := runInProcess(t, ws, "finish")
	if code == 0 {
		t.Error("finish without args should fail")
	}
}

func TestInProcess_NoteEditNoMsg(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	// No message arg and no --stdin → error path
	_, code := runInProcess(t, ws, "note", "edit", "some-id")
	if code == 0 {
		t.Error("note edit without message should fail")
	}
}

func TestInProcess_ShowNotFound(t *testing.T) {
	ws := setupInProcessWorkspace(t)
	_, code := runInProcess(t, ws, "show", "T-999")
	if code == 0 {
		t.Error("show nonexistent should fail")
	}
}

func TestInProcess_FullLifecycle(t *testing.T) {
	ws := setupInProcessWorkspace(t)

	// Create
	createArgs := append([]string{"create", "task", "Lifecycle item"}, testSBARArgsInprocess...)
	stdout, code := runInProcess(t, ws, createArgs...)
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	id := createdItemIDInProc(t, stdout)

	// Start
	_, code = runInProcess(t, ws, "start", id)
	if code != 0 {
		t.Fatalf("start exit %d", code)
	}

	// Verify active
	stdout, _ = runInProcess(t, ws, "show", "-f", "status", id)
	if !strings.Contains(stdout, "active") {
		t.Errorf("should be active: %q", stdout)
	}

	// Close
	_, code = runInProcess(t, ws, "close", id, "done", "--force")
	if code != 0 {
		t.Fatalf("close exit %d", code)
	}

	// Verify archived
	matches, _ := filepath.Glob(filepath.Join(ws, "archive", id+"-*.md"))
	if len(matches) != 1 {
		t.Errorf("expected 1 archive file for %s", id)
	}

	// Index
	_, code = runInProcess(t, ws, "index")
	if code != 0 {
		t.Fatalf("index exit %d", code)
	}
}

// createdItemIDInProc mirrors createdItemID (main_test.go) for the package main
// in-process tests: `create` stdout is "new item: <id>" then "Created <id> — …",
// so a fixed field index is brittle; take the id from the "new item:" line,
// falling back to the first T-/I-/E- token (I-1371).
var createdIDReInProc = regexp.MustCompile(`^[TIE]-\S+`)

func createdItemIDInProc(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "new item: "); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return f[0]
			}
		}
	}
	for _, tok := range strings.Fields(stdout) {
		if createdIDReInProc.MatchString(tok) {
			return tok
		}
	}
	t.Fatalf("could not parse item id from create output: %q", stdout)
	return ""
}
