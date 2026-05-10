package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
)

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

var _ io.Reader = (*bytes.Reader)(nil) // keep io import live
