package command

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/evidence"
	"github.com/theraprac/agent-state/internal/manifest"
	"github.com/theraprac/agent-state/internal/model"
)

func gzipNewReader(b []byte) (io.Reader, error) {
	return gzip.NewReader(bytes.NewReader(b))
}

func testRecordOpts() TestRecordOpts {
	return TestRecordOpts{
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
	}
}

// --- Record-only mode (existing) ---

func TestTestRecordRequiredSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := testRecordOpts()

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("TestRecord returned %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "api_unit")
	if !ok || !strings.HasPrefix(ev, "pass abc1234") {
		t.Errorf("testing_evidence.api_unit = %q", ev)
	}
}

func TestTestRecordScopeSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := testRecordOpts()

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "api_integration", "required")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	code := TestRecord(s, cfg, "T-003", "api_integration", opts)
	if code != 0 {
		t.Fatalf("TestRecord returned %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "api_integration")
	if !ok || !strings.HasPrefix(ev, "pass") {
		t.Errorf("testing_evidence.api_integration = %q, want pass ...", ev)
	}
}

func TestTestRecordInvalidSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	code := TestRecord(s, cfg, "T-003", "nonexistent_suite", testRecordOpts())
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestTestRecordItemNotFound(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	code := TestRecord(s, cfg, "T-999", "api_unit", testRecordOpts())
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestTestRecordItemNotActive(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	code := TestRecord(s, cfg, "T-001", "api_unit", testRecordOpts())
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestTestRecordNoTestingConfig(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing = nil
	code := TestRecord(s, cfg, "T-003", "api_unit", testRecordOpts())
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

// I-776: when the item declares a scope_class, suites in that class's
// required-suite set are resolvable by `st test … --run` even though
// they are NOT in cfg.Testing.RequiredSuites. Evidence is recorded as
// "pass <sha> <ts>" under testing_evidence.<suite>, same shape as a
// default-class required-suite recording.
func TestTestRecord_ScopeClassSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// Configure a workspace-config scope class with one required suite.
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash claude-config/hooks/run-changed-hook-tests.sh"},
			},
		},
	}

	// Mark T-003 with the scope_class.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = "workspace-config"
		it.Doc.SetField("scope_class", "workspace-config")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// Record evidence for the class's required suite.
	code := TestRecord(s, cfg, "T-003", "workspace_test", testRecordOpts())
	if code != 0 {
		t.Fatalf("TestRecord returned %d, want 0 — class-scoped suite should resolve", code)
	}

	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "workspace_test")
	if !ok || !strings.HasPrefix(ev, "pass abc1234") {
		t.Errorf("testing_evidence.workspace_test = %q, want pass abc1234 …", ev)
	}
}

// I-776: agents must not be able to `--skip` a class's required suite —
// same enforcement as default-class required suites. Workspace-config
// items still need their workspace_test evidence; skipping it would
// re-create the gate-bypass problem the scope-class mechanism solves.
func TestTestRecord_ScopeClassSuiteRefusesSkip(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash run.sh"},
			},
		},
	}

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = "workspace-config"
		it.Doc.SetField("scope_class", "workspace-config")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := testRecordOpts()
	opts.Skip = "not applicable"
	code := TestRecord(s, cfg, "T-003", "workspace_test", opts)
	if code == 0 {
		t.Error("TestRecord should refuse --skip on a class's required suite (got exit 0)")
	}
}

// I-776: unknown scope_class must fail fast in TestRecord too — matching
// the gate's behavior. Without this, an agent could record evidence under
// the default class's suites for an item whose class is misspelled.
func TestTestRecord_UnknownScopeClassFailsFast(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash run.sh"},
			},
		},
	}
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = "workspace-confg" // typo
		it.Doc.SetField("scope_class", "workspace-confg")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// Trying to record api_unit (a default-class suite) on this item must
	// fail because the gate would never accept it — silent fallthrough
	// would leave dirty evidence.
	code := TestRecord(s, cfg, "T-003", "api_unit", testRecordOpts())
	if code == 0 {
		t.Error("TestRecord should fail when item has unknown scope_class")
	}
}

// I-776: when a suite name collides between a class's RequiredSuites and
// the global ScopeSuites, the class's command wins (precedence: class >
// default-required > scope). Without the precedence guard, ScopeSuites
// would overwrite the suiteCmd resolved from the class.
func TestTestRecord_ClassWinsOverScopeSuiteCollision(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash class.sh"},
			},
		},
	}
	cfg.Testing.ScopeSuites["workspace_test"] = config.ScopeSuiteConfig{Command: "bash scope.sh"}

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = "workspace-config"
		it.Doc.SetField("scope_class", "workspace-config")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// We don't actually run the suite here (Run not set); we just check the
	// record-only path resolves the class command via the lookup. Easiest
	// way to verify precedence is to confirm `--skip` is refused (would
	// only be refused if isRequired=true, which only the class branch sets).
	opts := testRecordOpts()
	opts.Skip = "n/a"
	code := TestRecord(s, cfg, "T-003", "workspace_test", opts)
	if code == 0 {
		t.Error("--skip should be refused — class entry makes workspace_test required")
	}
}

func TestTestRecordSHATruncation(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := TestRecordOpts{
		GitHeadSHA: func(dir string) (string, error) {
			return "abcdef1234567890abcdef1234567890abcdef12\n", nil
		},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_unit")
	if !strings.Contains(ev, "abcdef1") {
		t.Errorf("evidence = %q, want truncated SHA", ev)
	}
	if strings.Contains(ev, "abcdef1234567890") {
		t.Errorf("evidence = %q, SHA not truncated", ev)
	}
}

// --- Run mode ---

func TestTestRunPass(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\nok  tests 0.5s\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}

	// Verify evidence uploaded
	keys, _ := opts.Backend.List("T-003/api_unit/")
	if len(keys) < 2 {
		t.Errorf("uploaded %d files, want >= 2 (log.txt + summary.json)", len(keys))
	}

	// Verify summary.json content
	for _, k := range keys {
		if strings.HasSuffix(k, "summary.json") {
			var buf strings.Builder
			opts.Backend.Download(k, &buf)
			var summary testSummary
			json.Unmarshal([]byte(buf.String()), &summary)
			if summary.Status != "pass" || summary.ExitCode != 0 {
				t.Errorf("summary = %+v", summary)
			}
		}
	}

	// Verify item updated with evidence URI
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_unit")
	if !strings.HasPrefix(ev, "pass") || !strings.Contains(ev, "evidence:") {
		t.Errorf("evidence = %q", ev)
	}
}

func TestTestRunFail(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("FAIL\n--- FAIL: TestBilling\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}

	// Verify fail recorded (not pass)
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_unit")
	if !strings.HasPrefix(ev, "fail") {
		t.Errorf("evidence = %q, want fail ...", ev)
	}
}

// I-1587: `st test` always mirrors the suite output to a local plain-text file
// under .as/test-logs/<id>/<suite>-<ts>.log, for both pass and fail, so a
// failure is never lost to scrollback / a truncated background tail.
func TestTestRunWritesLocalLog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		out      []byte
		exitCode int
	}{
		{"pass", []byte("PASS\nok  tests 0.5s\n"), 0},
		{"fail", []byte("FAIL\n--- FAIL: TestBilling\n"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := setupPRTestEnv(t)
			opts := TestRecordOpts{
				Run:        true,
				GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
				RunCmd:     func(command string) ([]byte, int, error) { return tc.out, tc.exitCode, nil },
				Backend:    &evidence.LocalBackend{Dir: t.TempDir()},
			}
			_ = TestRecord(s, cfg, "T-003", "api_unit", opts)

			dir := filepath.Join(cfg.Root(), ".as", "test-logs", "T-003")
			matches, _ := filepath.Glob(filepath.Join(dir, "api_unit-*.log"))
			if len(matches) != 1 {
				t.Fatalf("found %d log file(s) in %s, want 1", len(matches), dir)
			}
			got, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("read local log: %v", err)
			}
			if string(got) != string(tc.out) {
				t.Errorf("local log = %q, want %q", got, tc.out)
			}
		})
	}
}

// writeLocalTestLog is best-effort: when the target directory cannot be created
// it returns "" without panicking and never affects the test outcome.
func TestWriteLocalTestLog_UnwritableReturnsEmpty(t *testing.T) {
	_, cfg := setupPRTestEnv(t)
	// Plant a regular file where the test-logs tree needs a directory, so
	// MkdirAll under it fails.
	blocker := filepath.Join(cfg.Root(), ".as", "test-logs")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := writeLocalTestLog(cfg, "T-003", "api_unit", "20260101T000000", []byte("data")); got != "" {
		t.Errorf("writeLocalTestLog = %q, want empty string on unwritable target", got)
	}
}

func TestTestRunNoCommand(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	// Override suite to have empty command
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: ""}

	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc", nil },
		Backend:    &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1 (no command)", code)
	}
}

func TestTestRunExecutionError(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			return nil, 0, fmt.Errorf("command not found")
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestTestRunExplicitAgentRewritesRepoAndInjectsRuntime(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: "cd ../theraprac-api && make test-unit"}

	var gotCmd string
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	for _, want := range []string{
		"AS_AGENT_ID='agent-b'",
		"COMPOSE_PROJECT_NAME='theraprac_agent_b'",
		"THERAPRAC_API_PORT='8280'",
		"PLAYWRIGHT_BASE_URL='http://localhost:3200'",
		"cd '" + filepath.Join(agentsRoot, "theraprac-agent-b", "theraprac-api") + "'",
	} {
		if !strings.Contains(gotCmd, want) {
			t.Errorf("command missing %q:\n%s", want, gotCmd)
		}
	}
}

func TestTestRunResolvesAgentFromCwd(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: "cd ../theraprac-api && make test-unit"}
	cwd := filepath.Join(agentsRoot, "theraprac-agent-c", "theraprac-workspace")

	var gotCmd string
	opts := TestRecordOpts{
		Run: true,
		Cwd: cwd,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !strings.Contains(gotCmd, "AS_AGENT_ID='agent-c'") || !strings.Contains(gotCmd, "THERAPRAC_API_PORT='8380'") {
		t.Fatalf("command did not use agent-c runtime:\n%s", gotCmd)
	}
}

// I-400: when an item has an active worktree, st test --run must run
// against the worktree (feature branch), not the agent-root checkout (main).
// Otherwise a passing main-branch test produces misleading PASS evidence
// for a PR whose feature-branch tests would have failed.
func TestTestRunPrefersWorktreeOverAgentRoot(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: "cd ../theraprac-api && make test-unit"}

	// Configure worktree integration and create the worktree dir on disk
	// for T-003 — the active item used in setupPRTestEnv.
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"theraprac-api"},
	}
	worktreePath := filepath.Join(cfg.Root(), "worktrees", "T-003", "theraprac-api")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}

	var gotCmd string
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}

	// The cd should target the worktree, NOT the agent-root checkout.
	wantCd := "cd " + worktreePath
	if !strings.Contains(gotCmd, wantCd) {
		t.Errorf("command did not cd to worktree path:\nwant substring: %q\ngot: %s", wantCd, gotCmd)
	}
	agentRootRepo := filepath.Join(agentsRoot, "theraprac-agent-b", "theraprac-api")
	if strings.Contains(gotCmd, "cd '"+agentRootRepo+"'") {
		t.Errorf("command cd'd to agent-root checkout instead of worktree:\n%s", gotCmd)
	}
	// Agent runtime env vars must still be injected.
	for _, want := range []string{"AS_AGENT_ID='agent-b'", "THERAPRAC_API_PORT='8280'"} {
		if !strings.Contains(gotCmd, want) {
			t.Errorf("command missing env var %q:\n%s", want, gotCmd)
		}
	}
}

func TestTestRunFailsWhenAgentRuntimeAmbiguous(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: "cd ../theraprac-api && make test-unit"}
	opts := TestRecordOpts{
		Run: true,
		Cwd: t.TempDir(),
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("should not run"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Fatalf("returned %d, want 1", code)
	}
}

// I-422: A self-contained suite command (no `cd ../theraprac-…`) should not
// trigger agent-runtime resolution, even when the test cwd happens to sit
// inside a theraprac-agent-* dir on the developer's machine. Without this
// short-circuit, every TestTestRun* below this comment fails with
// "could not determine theraprac-agents root" unless THERAPRAC_AGENTS_ROOT
// is exported by the test runner.
func TestTestRunSkipsAgentRuntimeForSelfContainedSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	t.Setenv("THERAPRAC_AGENTS_ROOT", "")
	// Cwd inside a synthetic theraprac-agent-z tree. cfg.Root() is t.TempDir(),
	// which is not under theraprac-agents, so building a plan would fail.
	cwd := filepath.Join(t.TempDir(), "theraprac-agents", "theraprac-agent-z", "theraprac-workspace")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	opts := TestRecordOpts{
		Run: true,
		Cwd: cwd,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 — self-contained suite must not require agents-root resolution", code)
	}
}

// --- Coverage enforcement ---

func TestTestRunWithCoveragePass(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	// Set up manifest with app files
	manifest.AppendPR(cfg.ManifestDir(), "T-003", manifest.PRRecord{
		Repo: "api",
		Files: []manifest.FileRecord{
			{Path: "internal/db/billing.go", Type: "app"},
		},
	})

	coverOut := `mode: set
github.com/user/repo/internal/db/billing.go:10.2,15.3 2 2
github.com/user/repo/internal/db/billing.go:17.2,20.3 1 1
`
	opts := TestRecordOpts{
		Run:      true,
		Coverage: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			switch {
			case strings.HasSuffix(path, "cover.out"):
				return []byte(coverOut), nil
			case strings.HasSuffix(path, "go.mod"):
				return []byte("module github.com/user/repo\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 (100%% coverage)", code)
	}

	// Verify coverage files uploaded
	keys, _ := opts.Backend.List("T-003/api_unit/")
	foundCoverOut := false
	for _, k := range keys {
		if strings.HasSuffix(k, "cover.out") {
			foundCoverOut = true
		}
	}
	if !foundCoverOut {
		t.Error("cover.out not uploaded")
	}
}

func TestTestRunWithCoverageViolation(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	manifest.AppendPR(cfg.ManifestDir(), "T-003", manifest.PRRecord{
		Repo: "api",
		Files: []manifest.FileRecord{
			{Path: "internal/db/billing.go", Type: "app"},
		},
	})

	// Coverage with 0% on the file
	coverOut := `mode: set
github.com/user/repo/internal/db/billing.go:10.2,15.3 5 0
`
	opts := TestRecordOpts{
		Run:      true,
		Coverage: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			switch {
			case strings.HasSuffix(path, "cover.out"):
				return []byte(coverOut), nil
			case strings.HasSuffix(path, "go.mod"):
				return []byte("module github.com/user/repo\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1 (coverage violation)", code)
	}
}

func TestTestRunWithCoverageNoCoverFile(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	opts := TestRecordOpts{
		Run:      true,
		Coverage: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	// Should pass (non-fatal when coverage file not found)
	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Errorf("returned %d, want 0 (coverage file missing is non-fatal)", code)
	}
}

func TestTestRunVitestCoverage(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	manifest.AppendPR(cfg.ManifestDir(), "T-003", manifest.PRRecord{
		Repo: "web",
		Files: []manifest.FileRecord{
			{Path: "src/Button.tsx", Type: "app"},
		},
	})

	vitestJSON := `{
  "total": {"lines": {"pct": 95}},
  "src/Button.tsx": {
    "lines": {"total": 50, "covered": 48, "pct": 96.0},
    "branches": {"total": 20, "covered": 18, "pct": 90.0},
    "functions": {"total": 10, "covered": 10, "pct": 100.0},
    "statements": {"total": 60, "covered": 58, "pct": 96.67}
  }
}`

	opts := TestRecordOpts{
		Run:      true,
		Coverage: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "coverage-summary.json") {
				return []byte(vitestJSON), nil
			}
			return nil, os.ErrNotExist
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	// web_e2e is a scope suite
	code := TestRecord(s, cfg, "T-003", "web_e2e", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
}

// --- Helpers ---

func TestDefaultRunCmd(t *testing.T) {
	// Test with a simple command that should succeed
	output, exitCode, err := defaultRunCmd("echo hello")
	if err != nil {
		t.Fatalf("defaultRunCmd: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d", exitCode)
	}
	if !strings.Contains(string(output), "hello") {
		t.Errorf("output = %q", string(output))
	}
}

func TestDefaultRunCmdFail(t *testing.T) {
	output, exitCode, err := defaultRunCmd("exit 1")
	if err != nil {
		t.Fatalf("defaultRunCmd err: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
	_ = output
}

func TestEvidenceConfigFromCfg(t *testing.T) {
	cfg := &config.Config{
		Evidence: &config.EvidenceConfig{
			Backend:  "s3",
			S3Bucket: "my-bucket",
			S3Region: "us-west-2",
		},
	}
	ec := evidenceConfigFromCfg(cfg)
	if ec.Backend != "s3" || ec.S3Bucket != "my-bucket" {
		t.Errorf("config = %+v", ec)
	}
}

func TestEvidenceConfigFromCfgDefault(t *testing.T) {
	cfg := &config.Config{}
	ec := evidenceConfigFromCfg(cfg)
	if ec.Backend != "local" {
		t.Errorf("backend = %q, want local", ec.Backend)
	}
}

func TestTestRunWithArtifacts(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()

	// Create some fake artifact files
	artDir := t.TempDir()
	os.MkdirAll(filepath.Join(artDir, "test-results"), 0755)
	os.WriteFile(filepath.Join(artDir, "test-results", "screenshot.png"), []byte("fake-png"), 0644)
	os.WriteFile(filepath.Join(artDir, "test-results", "video.webm"), []byte("fake-video"), 0644)

	// Configure suite with artifacts
	cfg.Testing.ScopeSuites["web_e2e"] = config.ScopeSuiteConfig{
		Command:   "make e2e",
		Artifacts: []string{filepath.Join(artDir, "test-results") + "/**"},
	}

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "web_e2e", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}

	// Verify artifacts.tar.gz was uploaded
	keys, _ := opts.Backend.List("T-003/web_e2e/")
	foundTarGz := false
	for _, k := range keys {
		if strings.HasSuffix(k, "artifacts.tar.gz") {
			foundTarGz = true
		}
	}
	if !foundTarGz {
		t.Errorf("artifacts.tar.gz not uploaded, got keys: %v", keys)
	}
}

func TestCreateTarGz(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0644)

	var buf bytes.Buffer
	err := createTarGz(&buf, []string{
		filepath.Join(dir, "a.txt"),
		filepath.Join(dir, "b.txt"),
	})
	if err != nil {
		t.Fatalf("createTarGz: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("empty tar.gz")
	}
}

func TestConfigArtifacts(t *testing.T) {
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	configContent := `testing:
  scope_suites:
    web_e2e:
      command: make e2e
      artifacts: [test-results/**, playwright-report/**]
`
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(configContent), 0644)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sc := cfg.Testing.ScopeSuites["web_e2e"]
	if sc.Command != "make e2e" {
		t.Errorf("command = %q", sc.Command)
	}
	if len(sc.Artifacts) != 2 {
		t.Errorf("artifacts = %v, want 2", sc.Artifacts)
	}
}

// Ensure imports are used
var _ = filepath.Join
var _ = os.ErrNotExist
var _ = fmt.Errorf
var _ manifest.PRRecord

// --- Preflight (I-587) ---

// preflightFakeBackend satisfies evidence.Backend with an injectable
// Upload error so the preflight test can simulate "AWS creds expired"
// without an actual S3 dependency.
type preflightFakeBackend struct {
	uploadErr   error
	uploadCalls int
	deleteCalls int
}

func (b *preflightFakeBackend) Upload(string, io.Reader) (string, error) {
	b.uploadCalls++
	return "fake://uploaded", b.uploadErr
}
func (b *preflightFakeBackend) Download(string, io.Writer) error { return nil }
func (b *preflightFakeBackend) List(string) ([]string, error)    { return nil, nil }
func (b *preflightFakeBackend) URI(k string) string              { return "fake://" + k }
func (b *preflightFakeBackend) Delete(string) error              { b.deleteCalls++; return nil }

func TestTestRunPreflight_BlocksOnUploadFailure(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	t.Setenv("AS_SKIP_EVIDENCE_PREFLIGHT", "")

	fake := &preflightFakeBackend{uploadErr: fmt.Errorf("AccessDenied")}
	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(string) (string, error) {
			return "abc1234567890", nil
		},
		// Run a command that would FAIL if invoked — proves the
		// preflight aborted before reaching the test cmd.
		RunCmd: func(string) ([]byte, int, error) {
			t.Fatal("RunCmd should not be invoked when preflight fails")
			return nil, 0, nil
		},
		Backend: fake,
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 2 {
		t.Errorf("returned %d, want 2 (preflight failure exit code)", code)
	}
	if fake.uploadCalls != 1 {
		t.Errorf("upload calls = %d, want 1 (the preflight probe)", fake.uploadCalls)
	}
}

// I-1116: tests for the worktree guard in testRunMode.
// "myrepo" (no "theraprac-" prefix) avoids suiteNeedsAgentRuntime, so RunCmd
// reaches execution without needing full agent workspace setup.

func TestTestRunFailsWhenWorktreePresentButSuiteUnrewritten(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.RequiredSuites["myrepo_e2e"] = config.SuiteConfig{Command: "cd ../myrepo && scripts/run.sh"}
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"myrepo"},
	}
	// Create worktree BASE for T-003, but NOT the "myrepo" dir inside it.
	wtBase := filepath.Join(cfg.Root(), "worktrees", "T-003")
	if err := os.MkdirAll(wtBase, 0755); err != nil {
		t.Fatalf("create worktree base: %v", err)
	}

	var ran bool
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(string) ([]byte, int, error) {
			ran = true
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "myrepo_e2e", opts)
	if code != 1 {
		t.Fatalf("returned %d, want 1 (guard should block running against main clone)", code)
	}
	if ran {
		t.Error("RunCmd was called; guard should have exited before executing the suite")
	}
}

func TestTestRunErrorsWhenNoWorktreeOnDisk(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.RequiredSuites["myrepo_e2e"] = config.SuiteConfig{Command: "cd ../myrepo && scripts/run.sh"}
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"myrepo"},
	}
	// No worktree directory created. --run must refuse rather than execute
	// against the main clone (which would clobber prior evidence records).

	var ran bool
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(string) ([]byte, int, error) {
			ran = true
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "myrepo_e2e", opts)
	if code == 0 {
		t.Fatal("returned 0, want non-zero: --run with missing worktree must refuse")
	}
	if ran {
		t.Error("RunCmd was called; expected --run to refuse when worktree does not exist")
	}
}

func TestTestRunNoGuardWhenWorktreeDisabled(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.RequiredSuites["myrepo_e2e"] = config.SuiteConfig{Command: "cd ../myrepo && scripts/run.sh"}
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: false, // disabled — guard must not fire
		BaseDir: "worktrees",
		Repos:   []string{"myrepo"},
	}
	// Worktree base exists on disk; guard still must not fire because Enabled=false.
	wtBase := filepath.Join(cfg.Root(), "worktrees", "T-003")
	if err := os.MkdirAll(wtBase, 0755); err != nil {
		t.Fatalf("create worktree base: %v", err)
	}

	var ran bool
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(string) ([]byte, int, error) {
			ran = true
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "myrepo_e2e", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 (worktree disabled; guard must not fire)", code)
	}
	if !ran {
		t.Error("RunCmd was not called; expected execution to proceed when worktree integration is disabled")
	}
}

func TestTestRunNoGuardWhenRewriteFired(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.RequiredSuites["myrepo_e2e"] = config.SuiteConfig{Command: "cd ../myrepo && scripts/run.sh"}
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"myrepo"},
	}
	// Create both the worktree base AND the "myrepo" dir so the rewrite fires.
	wtRepo := filepath.Join(cfg.Root(), "worktrees", "T-003", "myrepo")
	if err := os.MkdirAll(wtRepo, 0755); err != nil {
		t.Fatalf("create worktree repo dir: %v", err)
	}

	var gotCmd string
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "myrepo_e2e", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 (rewrite fired; guard must not fire)", code)
	}
	if !strings.Contains(gotCmd, filepath.Join("worktrees", "T-003", "myrepo")) {
		t.Errorf("cmd was not rewritten to worktree path; got: %s", gotCmd)
	}
}

func TestTestRunPreflight_OptOutEnvSkipsProbe(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	t.Setenv("AS_SKIP_EVIDENCE_PREFLIGHT", "1")

	// Backend would fail upload — but with opt-out set the preflight
	// should be skipped entirely and the test should run normally.
	fake := &preflightFakeBackend{uploadErr: fmt.Errorf("AccessDenied")}
	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(string) ([]byte, int, error) {
			return []byte("PASS"), 0, nil
		},
		Backend: fake,
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Errorf("returned %d, want 0 (test should pass when preflight is opted out)", code)
	}
	// Upload was attempted for the actual test artifacts (log.txt +
	// summary.json) but NOT for the preflight probe — so calls = 2,
	// not 3.
	if fake.uploadCalls != 2 {
		t.Errorf("upload calls = %d, want 2 (log.txt + summary.json, no preflight probe)", fake.uploadCalls)
	}
}

func TestTestRunPreflight_SuccessProceedsWithCleanup(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	t.Setenv("AS_SKIP_EVIDENCE_PREFLIGHT", "")

	fake := &preflightFakeBackend{uploadErr: nil}
	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(string) ([]byte, int, error) {
			return []byte("PASS"), 0, nil
		},
		Backend: fake,
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Errorf("returned %d, want 0", code)
	}
	// preflight: 1 upload + 1 delete; test record: 2 uploads (log + summary)
	if fake.uploadCalls != 3 {
		t.Errorf("upload calls = %d, want 3 (probe + log.txt + summary.json)", fake.uploadCalls)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("delete calls = %d, want 1 (the preflight probe cleanup)", fake.deleteCalls)
	}
}

// I-802 — per-agent golangci-lint / Go build caches + lock-aware retry.
// Each test exercises a distinct failure mode of the original "shared
// ~/Library/Caches/golangci-lint flock" behavior; together they pin the
// allow-list classifier, the env injection, the regex match, the bounded
// retry, and the summary fields against regression.

func TestTestRunInjectsPerAgentCacheForApiLint(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	var gotCmd string
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	wantGL := filepath.Join(cfg.Root(), ".as", "cache", "golangci-lint", "agent-b")
	wantGC := filepath.Join(cfg.Root(), ".as", "cache", "go-build", "agent-b")
	for _, want := range []string{
		"GOLANGCI_LINT_CACHE='" + wantGL + "'",
		"GOCACHE='" + wantGC + "'",
	} {
		if !strings.Contains(gotCmd, want) {
			t.Errorf("api_lint command missing %q:\n%s", want, gotCmd)
		}
	}
	for _, dir := range []string{wantGL, wantGC} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("cache dir not created: %s (err=%v)", dir, err)
		}
	}
}

func TestTestRunSkipsPerAgentCacheForNonContendedSuite(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_unit"] = config.SuiteConfig{Command: "cd ../theraprac-api && make test-unit"}

	var gotCmd string
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_unit", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	for _, leak := range []string{"GOLANGCI_LINT_CACHE=", "GOCACHE="} {
		if strings.Contains(gotCmd, leak) {
			t.Errorf("non-contended suite must not inject %s:\n%s", leak, gotCmd)
		}
	}
}

func TestTestRunRetriesOnCacheLockError(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	calls := 0
	evDir := t.TempDir()
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			calls++
			if calls == 1 {
				return []byte("ERRO Running error: directory /Users/.../golangci-lint is being used by another process\n"), 3, nil
			}
			return []byte("ok\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 (retry should have succeeded)", code)
	}
	if calls != 2 {
		t.Errorf("RunCmd called %d times, want 2 (1 fail + 1 retry)", calls)
	}
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_lint")
	if !strings.HasPrefix(ev, "pass") {
		t.Errorf("evidence = %q, want pass prefix after retry", ev)
	}
	gotSummary := readBackTestSummary(t, opts.Backend, "T-003", "api_lint")
	if !gotSummary.Retried {
		t.Errorf("summary.retried = false, want true after a retry fired")
	}
}

func TestTestRunDoesNotRetryOnRealLintFailure(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	calls := 0
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			calls++
			return []byte("foo.go:10: declared and not used: x\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 1 {
		t.Fatalf("returned %d, want 1 (real lint failure must not be retried away)", code)
	}
	if calls != 1 {
		t.Errorf("RunCmd called %d times, want 1 (no retry on genuine failure)", calls)
	}
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_lint")
	if !strings.HasPrefix(ev, "fail") {
		t.Errorf("evidence = %q, want fail prefix", ev)
	}
}

func TestTestRunSummaryRecordsCacheRootsAndRetry(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			return []byte("PASS\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	got := readBackTestSummary(t, opts.Backend, "T-003", "api_lint")
	wantGL := filepath.Join(cfg.Root(), ".as", "cache", "golangci-lint", "agent-b")
	wantGC := filepath.Join(cfg.Root(), ".as", "cache", "go-build", "agent-b")
	if got.GolangciLintCache != wantGL {
		t.Errorf("summary.golangci_lint_cache = %q, want %q", got.GolangciLintCache, wantGL)
	}
	if got.GoCache != wantGC {
		t.Errorf("summary.go_cache = %q, want %q", got.GoCache, wantGC)
	}
	if got.Retried {
		t.Errorf("summary.retried = true on clean run, want false")
	}
}

// I-802 code-review follow-ups:

// Retry succeeds → evidence string carries " retried" tag so `st show`
// surfaces cache flakes without an S3 round-trip.
func TestTestRunRetriedEvidenceStringTagged(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	calls := 0
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			calls++
			if calls == 1 {
				return []byte("is being used by another process\n"), 3, nil
			}
			return []byte("ok\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_lint")
	if !strings.HasPrefix(ev, "pass retried ") {
		t.Errorf("evidence = %q, want prefix \"pass retried \" so st show surfaces the flake", ev)
	}
}

// Retry must concatenate first-attempt + delimiter + second-attempt into
// the uploaded log.txt so the cache-lock signature stays diagnostically
// available even after a successful retry. Otherwise summary.retried=true
// is unverifiable from evidence.
func TestTestRunRetryConcatenatesAttempts(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	agentsRoot := filepath.Join(t.TempDir(), "theraprac-agents")
	t.Setenv("THERAPRAC_AGENTS_ROOT", agentsRoot)
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "cd ../theraprac-api && make lint"}

	evDir := t.TempDir()
	calls := 0
	opts := TestRecordOpts{
		Run:   true,
		Agent: "b",
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			calls++
			if calls == 1 {
				return []byte("FIRST-RUN-LOCK: is being used by another process\n"), 3, nil
			}
			return []byte("SECOND-RUN-CLEAN: ok\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	log := readBackTestLog(t, opts.Backend, "T-003", "api_lint")
	for _, want := range []string{
		"FIRST-RUN-LOCK",
		"--- I-802 retry boundary",
		"SECOND-RUN-CLEAN",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("uploaded log missing %q — retry evidence trail incomplete\n--- log ---\n%s\n", want, log)
		}
	}
}

// Cache-contended suite with no resolved runtime must emit a loud warning
// (not silently run against the shared cache) and must not inject empty
// cache env vars into the command.
func TestTestRunCacheContendedWithoutRuntimeWarns(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	// No --agent / suite command has no cd ../theraprac-* →
	// resolveTestAgentRuntime returns ok=false even though api_lint is
	// cache-contended.
	cfg.Testing.RequiredSuites["api_lint"] = config.SuiteConfig{Command: "echo lint-self-contained"}

	var gotCmd string
	stderrBuf := new(bytes.Buffer)
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	done := make(chan struct{})
	go func() {
		io.Copy(stderrBuf, r)
		close(done)
	}()
	// Round-2 review: leak-safe even on t.Fatalf. Close the writer so the
	// io.Copy goroutine sees EOF, wait for it to finish, then close the
	// reader and restore stderr. Cleanup runs LIFO; defining it before the
	// inline cleanup means t.Fatalf paths still drain everything.
	t.Cleanup(func() {
		_ = w.Close()
		<-done
		_ = r.Close()
		os.Stderr = origStderr
	})

	opts := TestRecordOpts{
		Run: true,
		GitHeadSHA: func(dir string) (string, error) {
			return "abc1234567890", nil
		},
		RunCmd: func(command string) ([]byte, int, error) {
			gotCmd = command
			return []byte("ok\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	// Force the stderr drain BEFORE assertions so stderrBuf is populated.
	_ = w.Close()
	<-done
	os.Stderr = origStderr

	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !strings.Contains(stderrBuf.String(), "I-802 warning") {
		t.Errorf("expected I-802 warning on stderr when cache-contended suite runs without runtime; got:\n%s", stderrBuf.String())
	}
	for _, unwanted := range []string{"GOLANGCI_LINT_CACHE=", "GOCACHE="} {
		if strings.Contains(gotCmd, unwanted) {
			t.Errorf("command unexpectedly injected %s without resolved runtime:\n%s", unwanted, gotCmd)
		}
	}
}

// looksLikeCacheLockError must NOT fire on `concurrent map writes`
// (a Go runtime panic surfacing a real race) or on free-form output that
// merely contains the substrings 'cache' and 'locked' on the same line.
// Round-2 additions: word-boundary on `lock` so "lock-free" / "lock semantics"
// don't match; explicit coverage for the "cannot acquire" intransitive form.
func TestLooksLikeCacheLockErrorRegexTightened(t *testing.T) {
	for _, want := range []string{
		"saving cache: failed to acquire file lock",
		"unable to acquire file lock on /tmp/x",
		"cannot acquire file lock",
		"cannot acquire lock",
		"directory \"/Users/x/.cache/golangci-lint\" is being used by another process",
		"cache directory is locked",
	} {
		if !looksLikeCacheLockError([]byte(want)) {
			t.Errorf("expected true for documented signature: %q", want)
		}
	}
	for _, notWant := range []string{
		"fatal error: concurrent map writes\n",
		"cache_test.go:42: TestLockedConsistency failed",
		"foo.go:10: declared and not used: x",
		"some unrelated cache config dump locked-file.yml",
		// Word-boundary cases (round 2 review):
		"benchmark: unable to acquire lock-free queue slot",
		"docstring: failed to acquire lock-step semantics for batch",
		"cannot acquire lockable resource handle",
	} {
		if looksLikeCacheLockError([]byte(notWant)) {
			t.Errorf("regex should NOT match: %q", notWant)
		}
	}
}

// cachePathsForRuntime rollback must NOT wipe a pre-existing populated
// golangci-lint cache when the goCache MkdirAll fails. Round-2 review
// flagged a real regression where a warm 100s-of-MB cache could be nuked
// by a transient disk error.
func TestCachePathsForRuntimeRollbackPreservesWarmCache(t *testing.T) {
	root := t.TempDir()
	agentID := "agent-b"
	glPath := filepath.Join(root, ".as", "cache", "golangci-lint", agentID)

	// Pre-populate the golangci-lint cache: simulate a warm prior run.
	if err := os.MkdirAll(glPath, 0o755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	warmFile := filepath.Join(glPath, "warm-marker.bin")
	if err := os.WriteFile(warmFile, []byte("warm cache contents"), 0o644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}

	// Force goCache MkdirAll to fail by planting a FILE at the path it
	// would create (not a directory). os.MkdirAll fails with "not a
	// directory" because the intermediate path component is a file.
	goCacheParent := filepath.Join(root, ".as", "cache", "go-build")
	if err := os.MkdirAll(goCacheParent, 0o755); err != nil {
		t.Fatalf("setup goCache parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goCacheParent, agentID), []byte("blocker"), 0o644); err != nil {
		t.Fatalf("setup blocker: %v", err)
	}

	_, _, err := cachePathsForRuntime(root, agentID)
	if err == nil {
		t.Fatalf("expected MkdirAll(goCache) failure, got nil")
	}
	// Pin the failure path — round-3 review: a future early-return validation
	// could short-circuit BEFORE either MkdirAll runs, silently moving this
	// test off the rollback path. The blocker is a file at the goCache leaf,
	// so MkdirAll fails with "not a directory" or ENOTDIR.
	if !strings.Contains(err.Error(), "not a directory") && !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("err = %v, want ENOTDIR-shaped failure (so we know the goCache MkdirAll is the failing step)", err)
	}
	// The warm cache marker MUST survive — rollback should not have fired.
	if _, statErr := os.Stat(warmFile); statErr != nil {
		t.Errorf("rollback wiped pre-existing warm cache: %v (this is the round-2 review regression)", statErr)
	}
}

// Complement: when WE created golangciLintCache fresh and goCache fails,
// the rollback SHOULD remove what we just made (no leftover empty dirs).
func TestCachePathsForRuntimeRollbackRemovesFreshCreate(t *testing.T) {
	root := t.TempDir()
	agentID := "agent-b"
	glPath := filepath.Join(root, ".as", "cache", "golangci-lint", agentID)

	// Plant the goCache blocker without pre-creating golangciLintCache.
	goCacheParent := filepath.Join(root, ".as", "cache", "go-build")
	if err := os.MkdirAll(goCacheParent, 0o755); err != nil {
		t.Fatalf("setup goCache parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goCacheParent, agentID), []byte("blocker"), 0o644); err != nil {
		t.Fatalf("setup blocker: %v", err)
	}

	_, _, err := cachePathsForRuntime(root, agentID)
	if err == nil {
		t.Fatalf("expected MkdirAll(goCache) failure, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") && !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("err = %v, want ENOTDIR-shaped failure", err)
	}
	// Fresh-created golangciLintCache should be gone.
	if _, statErr := os.Stat(glPath); statErr == nil {
		t.Errorf("fresh-created golangciLintCache survived rollback at %s", glPath)
	}
}

// readBackTestLog fetches the uploaded log.txt.gz, decompresses, returns
// it as string. Used by retry-concatenation test.
func readBackTestLog(t *testing.T, backend evidence.Backend, id, suite string) string {
	t.Helper()
	keys, err := backend.List(id + "/" + suite + "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, "log.txt.gz") {
			continue
		}
		var buf bytes.Buffer
		if err := backend.Download(k, &buf); err != nil {
			t.Fatalf("Download: %v", err)
		}
		gz, err := gzipNewReader(buf.Bytes())
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		return string(raw)
	}
	t.Fatalf("no log.txt.gz found under %s/%s/", id, suite)
	return ""
}

// readBackTestSummary fetches the uploaded summary.json.gz for an item+suite
// and decodes it. Walks all uploaded keys (timestamped) and returns the
// first summary it finds — tests in this package upload exactly once.
func readBackTestSummary(t *testing.T, backend evidence.Backend, id, suite string) testSummary {
	t.Helper()
	keys, err := backend.List(id + "/" + suite + "/")
	if err != nil {
		t.Fatalf("List(%s/%s/): %v", id, suite, err)
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, "summary.json.gz") {
			continue
		}
		var buf bytes.Buffer
		if err := backend.Download(k, &buf); err != nil {
			t.Fatalf("Download(%s): %v", k, err)
		}
		gz, err := gzipNewReader(buf.Bytes())
		if err != nil {
			t.Fatalf("gzip reader on %s: %v", k, err)
		}
		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Fatalf("read summary %s: %v", k, err)
		}
		var got testSummary
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal summary %s: %v", k, err)
		}
		return got
	}
	t.Fatalf("no summary.json.gz found under %s/%s/ (keys: %v)", id, suite, keys)
	return testSummary{}
}

var _ io.Reader = (*bytes.Reader)(nil) // keep io import live

// --- I-997: scope_repos guard on --run path ---

// TestTestRecordRunProceedsWhenRequiredEvidenceSet verifies that a suite whose
// testing_evidence is "required" (set by st pr) is never auto-skipped by the
// scope_repos guard — the "required" marker takes precedence.
func TestTestRecordRunProceedsWhenRequiredEvidenceSet(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()
	// Suite's repo is NOT in scope_repos, but it has been triggered by st pr.
	cfg.Testing.ScopeSuites["web_e2e"] = config.ScopeSuiteConfig{Command: "make e2e"}
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "web_e2e", "required")
		it.Doc.SetField("scope_repos", "theraprac-api")
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	called := false
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			called = true
			return []byte("ok"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}
	code := TestRecord(s, cfg, "T-003", "web_e2e", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !called {
		t.Error("RunCmd not called — 'required' evidence should override scope_repos guard")
	}
}

// TestTestRecordRunSkipsWhenNotInScopeRepos verifies that --run auto-skips a suite
// whose target repo is absent from the item's scope_repos.
func TestTestRecordRunSkipsWhenNotInScopeRepos(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	// Item only touches theraprac-infra; api_lint targets theraprac-api.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Doc.SetField("scope_repos", "theraprac-infra")
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
	}
	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0 (auto-skip should succeed)", code)
	}
	item, _ := s.Get("T-003")
	ev, _ := getNestedField(item, "testing_evidence", "api_lint")
	if !strings.HasPrefix(ev, "auto-skip:") || !strings.Contains(ev, "theraprac-api") {
		t.Errorf("evidence = %q, want auto-skip: not in scope_repos: theraprac-api", ev)
	}
}

// TestTestRecordRunProceedsWhenInScopeRepos verifies that --run proceeds normally
// when the suite's repo is present in scope_repos.
func TestTestRecordRunProceedsWhenInScopeRepos(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Doc.SetField("scope_repos", "theraprac-api")
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	called := false
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			called = true
			return []byte("ok"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}
	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !called {
		t.Error("RunCmd not called — suite repo is in scope_repos, should proceed to testRunMode")
	}
}

// TestTestRecordRunProceedsWhenScopeReposEmpty verifies backward compatibility:
// when scope_repos is absent, --run proceeds for all suites.
func TestTestRecordRunProceedsWhenScopeReposEmpty(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()
	// No scope_repos set on T-003.
	called := false
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			called = true
			return []byte("ok"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}
	code := TestRecord(s, cfg, "T-003", "api_lint", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !called {
		t.Error("RunCmd not called — no scope_repos should not skip any suite")
	}
}

// TestTestRecordRunProceedsForSuiteWithNoRepoMapping verifies that a suite whose
// name has no known prefix mapping (autoScopeRepo returns "") is never auto-skipped,
// even when scope_repos is set.
func TestTestRecordRunProceedsForSuiteWithNoRepoMapping(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	evDir := t.TempDir()
	cfg.Testing.ScopeSuites["custom_check"] = config.ScopeSuiteConfig{Command: "echo custom"}
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "custom_check", "required")
		it.Doc.SetField("scope_repos", "theraprac-infra")
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	called := false
	opts := TestRecordOpts{
		Run:        true,
		GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
		RunCmd: func(command string) ([]byte, int, error) {
			called = true
			return []byte("ok"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: evDir},
	}
	code := TestRecord(s, cfg, "T-003", "custom_check", opts)
	if code != 0 {
		t.Fatalf("returned %d, want 0", code)
	}
	if !called {
		t.Error("RunCmd not called — suite with no repo mapping should never auto-skip")
	}
}

// I-1304: --skip on a required suite must succeed (recording "auto-skip: ...")
// when the suite's repo has no diff. Without worktree dirs present, detectTouchedRepos
// finds no git repos and returns an empty map — correctly treating all repos as untouched.
func TestTestRecord_SkipRequiredSuite_NotApplicable_I1304(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// Add a worktree config so the not-applicable check can run.
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		Repos:   []string{"theraprac-api"},
		RepoMap: map[string]string{"theraprac-api": "theraprac-api"},
		BaseDir: t.TempDir(), // no real git repos here → detectTouchedRepos returns empty
	}

	opts := testRecordOpts()
	opts.Skip = "no api changes in this PR"

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			code := TestRecord(s, cfg, "T-003", "api_lint", opts)
			if code != 0 {
				t.Errorf("TestRecord returned %d, want 0 (required suite not applicable)", code)
			}
		})
	})

	if strings.Contains(stderr, "cannot skip") {
		t.Errorf("should not reject skip when repo has no changes; stderr: %s", stderr)
	}

	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "api_lint")
	if !ok || !strings.HasPrefix(ev, "auto-skip:") {
		t.Errorf("testing_evidence.api_lint = %q, want auto-skip:...", ev)
	}
}

// I-1304: --skip on a class-required suite must be rejected even when the
// suite's repo has no diff. Before the ScopeClass guard was added to the
// --skip path, a class item with cfg.Worktree != nil and no repo changes
// would silently accept the skip — violating the class-suite invariant.
func TestTestRecord_SkipRequiredSuite_ClassItem_Rejected_I1304(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	cfg.Testing.ScopeClasses = map[string]config.ScopeClassConfig{
		"workspace-config": {
			RequiredSuites: map[string]config.SuiteConfig{
				"workspace_test": {Command: "bash run.sh"},
			},
		},
	}
	// Worktree configured with temp dir — detectTouchedRepos returns empty map
	// (no real git repos). Pre-fix this made notApplicable=true and skip accepted.
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		Repos:   []string{"as"},
		RepoMap: map[string]string{"as": "as"},
		BaseDir: t.TempDir(),
	}

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = "workspace-config"
		it.Doc.SetField("scope_class", "workspace-config")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := testRecordOpts()
	opts.Skip = "no workspace changes"

	stderr := captureStderrStr(t, func() {
		captureStdout(t, func() {
			code := TestRecord(s, cfg, "T-003", "workspace_test", opts)
			if code == 0 {
				t.Error("TestRecord should reject --skip on class-required suite even when repo has no diff")
			}
		})
	})
	if !strings.Contains(stderr, "class item") {
		t.Errorf("expected 'class item' in error message; got: %s", stderr)
	}
}

// I-1304: when cfg.Worktree is nil (worktree not configured), the rejection
// message should say "worktree not configured" — not "repo has changes" (which
// is factually wrong since no diff check was performed).
func TestTestRecord_SkipRequiredSuite_WorktreeNil_ClearMessage_I1304(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	// cfg.Worktree is nil by default from setupPRTestEnv.

	opts := testRecordOpts()
	opts.Skip = "no api changes"

	var stderrOut string
	captureStdout(t, func() {
		stderrOut = captureStderrStr(t, func() {
			code := TestRecord(s, cfg, "T-003", "api_lint", opts)
			if code == 0 {
				t.Error("TestRecord should reject --skip when worktree is not configured")
			}
		})
	})
	if strings.Contains(stderrOut, "has changes") {
		t.Errorf("rejection message should not claim 'has changes' when worktree unconfigured; got: %s", stderrOut)
	}
	if !strings.Contains(stderrOut, "worktree not configured") {
		t.Errorf("expected 'worktree not configured' in error message; got: %s", stderrOut)
	}
}

// I-1304: --skip on a required suite must be rejected when the suite's repo
// cannot be determined (suite has no repo prefix mapping) — we can't confirm
// not-applicable without a repo to check.
func TestTestRecord_SkipRequiredSuite_NoRepoMapping_Rejected_I1304(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// Add a required suite with no repo prefix mapping (name doesn't start with api/web/etc.)
	cfg.Testing.RequiredSuites["custom_required"] = config.SuiteConfig{Command: "make custom"}

	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		Repos:   []string{"theraprac-api"},
		RepoMap: map[string]string{"theraprac-api": "theraprac-api"},
		BaseDir: t.TempDir(),
	}

	opts := testRecordOpts()
	opts.Skip = "not applicable"

	code := TestRecord(s, cfg, "T-003", "custom_required", opts)
	if code == 0 {
		t.Error("TestRecord should reject --skip when suite has no repo mapping (cannot verify not-applicable)")
	}
}

// --- I-757: env/target/vendor-tier capture ---

func TestTestRecord_EnvFrom(t *testing.T) {
	tests := []struct {
		name         string
		envFrom      string
		targetFrom   []string
		vendorTiers  []string
		envVars      map[string]string
		suiteExit    int
		wantCode     int
		wantEnvInEv  string // non-empty → evidence line must contain this substring
		wantEnvField string // expected summary.Env (empty string = field should be absent)
		wantSuiteRan bool
	}{
		{
			name:         "env_from_set",
			envFrom:      "$TARGET_ENV",
			envVars:      map[string]string{"TARGET_ENV": "demo"},
			suiteExit:    0,
			wantCode:     0,
			wantEnvInEv:  "env=demo",
			wantEnvField: "demo",
			wantSuiteRan: true,
		},
		{
			name:         "env_from_unset",
			envFrom:      "$TARGET_ENV",
			envVars:      map[string]string{}, // TARGET_ENV not set
			wantCode:     1,
			wantSuiteRan: false, // suite must NOT run when env_from is unset
		},
		{
			name:         "env_from_absent",
			envFrom:      "", // no env_from declared
			envVars:      map[string]string{"TARGET_ENV": "demo"},
			suiteExit:    0,
			wantCode:     0,
			wantEnvInEv:  "", // no env tag in evidence line
			wantEnvField: "",
			wantSuiteRan: true,
		},
		{
			name:        "target_vendor_stamped",
			envFrom:     "$TARGET_ENV",
			targetFrom:  []string{"db_endpoint=$DB_HOST", "api_endpoint=$API_BASE_URL"},
			vendorTiers: []string{"stedi=$STEDI_TIER"},
			envVars: map[string]string{
				"TARGET_ENV":   "demo",
				"DB_HOST":      "rds.example.com",
				"API_BASE_URL": "https://api.example.com",
				"STEDI_TIER":   "T",
			},
			suiteExit:    0,
			wantCode:     0,
			wantEnvInEv:  "env=demo",
			wantEnvField: "demo",
			wantSuiteRan: true,
		},
		{
			name:         "env_from_set_suite_fails",
			envFrom:      "$TARGET_ENV",
			envVars:      map[string]string{"TARGET_ENV": "demo"},
			suiteExit:    1,
			wantCode:     1,
			wantEnvInEv:  "env=demo", // env tag appears on fail path too
			wantEnvField: "demo",
			wantSuiteRan: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := setupPRTestEnv(t)
			backend := &evidence.LocalBackend{Dir: t.TempDir()}

			// Set up a scope suite with the test's config
			cfg.Testing.ScopeSuites["live_acceptance"] = config.ScopeSuiteConfig{
				Command:     "make live-acceptance",
				EnvFrom:     tc.envFrom,
				TargetFrom:  tc.targetFrom,
				VendorTiers: tc.vendorTiers,
			}
			// Mark the suite as required so TestRecord's --run path enters testRunMode
			if err := s.Mutate("T-003", func(it *model.Item) error {
				it.SetNested("testing_evidence", "live_acceptance", "required")
				return nil
			}); err != nil {
				t.Fatalf("mutate: %v", err)
			}

			// Set/unset env vars
			for k, v := range tc.envVars {
				t.Setenv(k, v)
			}
			// Ensure vars not in the map are cleared
			for _, varName := range []string{"TARGET_ENV", "DB_HOST", "API_BASE_URL", "STEDI_TIER"} {
				if _, set := tc.envVars[varName]; !set {
					t.Setenv(varName, "")
				}
			}

			suiteRan := false
			opts := TestRecordOpts{
				Run: true,
				GitHeadSHA: func(dir string) (string, error) { return "abc1234567890", nil },
				RunCmd: func(command string) ([]byte, int, error) {
					suiteRan = true
					return []byte("output\n"), tc.suiteExit, nil
				},
				Backend: backend,
			}

			code := TestRecord(s, cfg, "T-003", "live_acceptance", opts)

			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if suiteRan != tc.wantSuiteRan {
				t.Errorf("suiteRan = %v, want %v", suiteRan, tc.wantSuiteRan)
			}

			if !tc.wantSuiteRan {
				return // no evidence to check
			}

			// Check evidence line in item
			item, _ := s.Get("T-003")
			ev, _ := getNestedField(item, "testing_evidence", "live_acceptance")
			if tc.wantEnvInEv != "" && !strings.Contains(ev, tc.wantEnvInEv) {
				t.Errorf("evidence = %q, want to contain %q", ev, tc.wantEnvInEv)
			}
			if tc.wantEnvInEv == "" && strings.Contains(ev, " env=") {
				t.Errorf("evidence = %q, unexpected env= tag", ev)
			}

			// Check summary.json fields
			summary := readBackTestSummary(t, backend, "T-003", "live_acceptance")
			if summary.Env != tc.wantEnvField {
				t.Errorf("summary.Env = %q, want %q", summary.Env, tc.wantEnvField)
			}

			// target/vendor checks only for target_vendor_stamped
			if tc.name == "target_vendor_stamped" {
				if summary.Target["db_endpoint"] != "rds.example.com" {
					t.Errorf("summary.Target[db_endpoint] = %q", summary.Target["db_endpoint"])
				}
				if summary.Target["api_endpoint"] != "https://api.example.com" {
					t.Errorf("summary.Target[api_endpoint] = %q", summary.Target["api_endpoint"])
				}
				if summary.VendorTiers["stedi"] != "T" {
					t.Errorf("summary.VendorTiers[stedi] = %q", summary.VendorTiers["stedi"])
				}
			}
		})
	}
}
