package command

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/store"
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
	item, _ := s.Get("T-003")
	setNestedField(item, "testing_evidence", "api_unit", "pass abc123 2026-03-27")
	setNestedField(item, "testing_evidence", "api_lint", "pass abc123 2026-03-27")
	setNestedField(item, "manifest", "prs", "api#42")
	s.Write(item)

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
	item, _ := s.Get("T-003")
	setNestedField(item, "testing_evidence", "api_unit", "null")
	s.Write(item)

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

func TestWatchMainCINullRuns(t *testing.T) {
	// When gh returns "null null null" (no Deploy workflow), watchMainCI should
	// skip gracefully instead of looping forever.
	runCmd := func(cmd string) ([]byte, int, error) {
		return []byte("null null null\n"), 0, nil
	}
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for null runs: %v", err)
	}
}

func TestWatchMainCIEmptyOutput(t *testing.T) {
	// When gh returns empty output (no runs at all), should keep polling.
	// But if exit code != 0, should skip.
	runCmd := func(cmd string) ([]byte, int, error) {
		return []byte(""), 1, nil
	}
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for gh failure: %v", err)
	}
}

func TestWatchMainCISuccess(t *testing.T) {
	runCmd := func(cmd string) ([]byte, int, error) {
		return []byte("12345 completed success\n"), 0, nil
	}
	err := watchMainCI(runCmd)
	if err != nil {
		t.Errorf("watchMainCI returned error for success: %v", err)
	}
}

func TestWatchMainCIFailed(t *testing.T) {
	runCmd := func(cmd string) ([]byte, int, error) {
		return []byte("12345 completed failure\n"), 0, nil
	}
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
