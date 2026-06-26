package command

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/evidence"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

func setupPipelineTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupPRTestEnv(t) // active T-003, testing config

	cfg.Pipeline = &config.PipelineConfig{
		Merge: &config.PipelineStepConfig{
			Command:    "echo merge-done",
			PreChecks:  []string{"echo checks-pass"},
			PostRecord: "echo abc123merge",
		},
		DeployCheck: &config.PipelineStepConfig{
			Command: "echo deploy-ok",
		},
		Smoke: &config.PipelineStepConfig{
			Command: "echo smoke-pass",
		},
	}

	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "smoke_passed"},
		ArchiveGate: "smoke_passed",
	}

	cfg.Gates = map[string][]config.GateConfig{
		"close": {
			{Type: "testing_complete"},
			{Type: "manifest_exists"},
		},
	}

	// Give T-003 test evidence so gates pass
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "api_unit", "pass abc123 2026-03-27")
		it.SetNested("testing_evidence", "api_lint", "pass abc123 2026-03-27")
		it.SetNested("manifest", "prs", "api#42")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	return s, cfg
}

func mockPipelineOpts(t *testing.T) PipelineOpts {
	return PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte(fmt.Sprintf("executed: %s\n", cmd)), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}
}

// --- Merge ---

func TestMergePass(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := mockPipelineOpts(t)

	code := Merge(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("Merge returned %d", code)
	}

	item, _ := s.Get("T-003")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "merged" {
		t.Errorf("stage = %q, want merged", stage)
	}

	sha, _ := getNestedField(item, "work_tracking", "merge_sha")
	if !strings.Contains(sha, "abc123merge") {
		t.Errorf("merge_sha = %q", sha)
	}
}

func TestMergeGateFail(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := mockPipelineOpts(t)

	// Remove test evidence so gate fails
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "api_unit", "null")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	code := Merge(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Merge with missing evidence returned %d, want 1", code)
	}
}

func TestMergePreCheckFail(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			if strings.Contains(cmd, "checks") {
				return []byte("CI failed\n"), 1, nil
			}
			return []byte("ok\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := Merge(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Merge with failed pre-check returned %d, want 1", code)
	}
}

func TestMergeNotConfigured(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	cfg.Pipeline.Merge = nil
	opts := mockPipelineOpts(t)

	code := Merge(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Merge with no config returned %d, want 1", code)
	}
}

func TestMergeItemNotFound(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	code := Merge(s, cfg, "T-999", mockPipelineOpts(t))
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

// --- Deploy Check ---

func TestDeployCheckPass(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := mockPipelineOpts(t)

	code := DeployCheck(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("DeployCheck returned %d", code)
	}

	item, _ := s.Get("T-003")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "deployed_dev" {
		t.Errorf("stage = %q, want deployed_dev", stage)
	}
}

func TestDeployCheckHealthURL(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	cfg.Pipeline.DeployCheck = &config.PipelineStepConfig{
		HealthURL: "http://localhost:9999/health",
		Timeout:   1,
	}

	opts := PipelineOpts{
		HTTPGet: func(url string) (int, string, error) {
			return 200, "", nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := DeployCheck(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("DeployCheck with health returned %d", code)
	}

	item, _ := s.Get("T-003")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "deployed_dev" {
		t.Errorf("stage = %q, want deployed_dev", stage)
	}
}

func TestDeployCheckHealthFail(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	cfg.Pipeline.DeployCheck = &config.PipelineStepConfig{
		HealthURL: "http://localhost:9999/health",
		Timeout:   1,
	}

	opts := PipelineOpts{
		HTTPGet: func(url string) (int, string, error) {
			return 503, `{"status":"unhealthy"}`, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := DeployCheck(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("DeployCheck with failing health returned %d, want 1", code)
	}
}

func TestDeployCheckNotConfigured(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	cfg.Pipeline.DeployCheck = nil

	code := DeployCheck(s, cfg, "T-003", mockPipelineOpts(t))
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

// --- Smoke ---

func TestSmokePass(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := mockPipelineOpts(t)

	code := Smoke(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("Smoke returned %d", code)
	}

	item, _ := s.Get("T-003")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "smoke_passed" {
		t.Errorf("stage = %q, want smoke_passed", stage)
	}
}

func TestSmokeFail(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	opts := PipelineOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte("smoke failed\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := Smoke(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Smoke with failure returned %d, want 1", code)
	}
}

func TestSmokeNotConfigured(t *testing.T) {
	s, cfg := setupPipelineTestEnv(t)
	cfg.Pipeline.Smoke = nil

	code := Smoke(s, cfg, "T-003", mockPipelineOpts(t))
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

// --- watchMainCI ---

// dispatchByCmd dispatches a stub runCmd by inspecting the command
// string. I-150 added an up-front `gh workflow list` call before the
// poll loop, so single-callback stubs can no longer cover both
// branches. Tests that want a specific workflow-list response use
// this helper; tests that don't care can pass `"1"` for the
// workflow-list count to skip the short-circuit and exercise the
// poll loop.
func dispatchByCmd(workflowListCount string, pollResp []byte, pollExit int) func(string) ([]byte, int, error) {
	return func(cmd string) ([]byte, int, error) {
		if strings.Contains(cmd, "gh workflow list") {
			return []byte(workflowListCount + "\n"), 0, nil
		}
		return pollResp, pollExit, nil
	}
}

// I-150: when no Deploy workflow exists in the repo, the up-front
// check returns immediately without entering the poll loop.
func TestWatchMainCISkipsWhenNoDeployWorkflow(t *testing.T) {
	calls := 0
	runCmd := func(cmd string) ([]byte, int, error) {
		calls++
		if strings.Contains(cmd, "gh workflow list") {
			return []byte("0\n"), 0, nil
		}
		t.Errorf("poll-loop should not run when no Deploy workflow; got cmd=%q", cmd)
		return nil, 1, nil
	}
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error when skipping: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 runCmd call, got %d", calls)
	}
}

// I-150: when `gh workflow list` itself fails (gh missing, no
// auth), fall through to the poll loop's existing gh-failure
// handling. The poll loop will then exit-skip on its own.
func TestWatchMainCIFallsThroughWhenWorkflowListFails(t *testing.T) {
	runCmd := func(cmd string) ([]byte, int, error) {
		if strings.Contains(cmd, "gh workflow list") {
			return []byte(""), 1, nil
		}
		// Poll loop's exitCode != 0 path: skip CI watch.
		return []byte(""), 1, nil
	}
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error on fall-through: %v", err)
	}
}

func TestWatchMainCINullRuns(t *testing.T) {
	// When gh returns "null null null" (no Deploy workflow), watchMainCI should
	// skip gracefully instead of looping forever. Workflow-list returns "1"
	// so we exercise the poll loop's null-handling path.
	runCmd := dispatchByCmd("1", []byte("null null null\n"), 0)
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for null runs: %v", err)
	}
}

func TestWatchMainCIEmptyOutput(t *testing.T) {
	// When gh poll returns exit != 0 (gh missing mid-loop), should
	// skip. Workflow-list returns "1" to bypass the up-front
	// short-circuit and exercise the poll loop's gh-failure path.
	runCmd := dispatchByCmd("1", []byte(""), 1)
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for gh failure: %v", err)
	}
}

func TestWatchMainCISuccess(t *testing.T) {
	runCmd := dispatchByCmd("1", []byte("12345 completed success\n"), 0)
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for success: %v", err)
	}
}

func TestWatchMainCIFailed(t *testing.T) {
	runCmd := dispatchByCmd("1", []byte("12345 completed failure\n"), 0)
	err := watchMainCI(runCmd)
	if err == nil {
		t.Error("watchMainCI should return error for failed CI")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error should mention failure: %v", err)
	}
}

// --- Config ---

func TestPipelineConfig(t *testing.T) {
	root := t.TempDir()
	asDir := root + "/.as"
	os.MkdirAll(asDir, 0755)
	os.WriteFile(asDir+"/config.yaml", []byte(`pipeline:
  merge:
    command: gh pr merge --squash
    pre_checks: [gh pr checks --watch]
    post_record: gh pr view --json mergeCommit -q .mergeCommit.oid
  deploy_check:
    command: scripts/check-deploy.sh
    health_url: https://dev.example.com/health
    timeout: 120
  smoke:
    command: scripts/smoke-test.sh
    artifacts: [smoke-results/**]
`), 0644)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Pipeline == nil {
		t.Fatal("pipeline config nil")
	}
	if cfg.Pipeline.Merge == nil || cfg.Pipeline.Merge.Command != "gh pr merge --squash" {
		t.Errorf("merge = %+v", cfg.Pipeline.Merge)
	}
	if cfg.Pipeline.Merge.PostRecord != "gh pr view --json mergeCommit -q .mergeCommit.oid" {
		t.Errorf("post_record = %q", cfg.Pipeline.Merge.PostRecord)
	}
	if cfg.Pipeline.DeployCheck == nil || cfg.Pipeline.DeployCheck.HealthURL != "https://dev.example.com/health" {
		t.Errorf("deploy_check = %+v", cfg.Pipeline.DeployCheck)
	}
	if cfg.Pipeline.DeployCheck.Timeout != 120 {
		t.Errorf("timeout = %d", cfg.Pipeline.DeployCheck.Timeout)
	}
	if cfg.Pipeline.Smoke == nil || cfg.Pipeline.Smoke.Command != "scripts/smoke-test.sh" {
		t.Errorf("smoke = %+v", cfg.Pipeline.Smoke)
	}
}

// --- Health check ---

func TestCheckHealthPass(t *testing.T) {
	err := checkHealth("http://test", 1, func(url string) (int, string, error) {
		return 200, "", nil
	})
	if err != nil {
		t.Errorf("checkHealth: %v", err)
	}
}

func TestCheckHealthTimeout(t *testing.T) {
	err := checkHealth("http://test", 1, func(url string) (int, string, error) {
		return 503, `{"status":"unhealthy","mode":"grounded"}`, nil
	})
	if err == nil {
		t.Error("expected timeout error")
	}
	if !strings.Contains(err.Error(), "grounded") {
		t.Errorf("expected body in error, got: %v", err)
	}
}
