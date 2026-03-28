package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

// PipelineOpts holds injectable functions for pipeline commands.
type PipelineOpts struct {
	RunCmd    func(cmd string) ([]byte, int, error)
	Backend   evidence.Backend
	HTTPGet   func(url string) (int, error) // returns status code
}

// --- Merge ---

func Merge(s *store.Store, cfg *config.Config, id string, opts PipelineOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active\n", id, item.Status)
		return 1
	}

	// Verify close gates would pass (without actually closing)
	results := validate.EvaluateGates(item, "close", cfg, s.All())
	if !validate.GatesPassed(results) {
		failure := validate.FirstFailure(results)
		fmt.Fprintf(os.Stderr, "merge gate failed: %s — %s\n", failure.Gate, failure.Message)
		return 1
	}

	stepCfg := getStepConfig(cfg, "merge")
	if stepCfg == nil || stepCfg.Command == "" {
		fmt.Fprintln(os.Stderr, "pipeline.merge.command not configured")
		return 1
	}

	return runPipelineStep(s, cfg, id, "merge", "merged", stepCfg, opts)
}

// --- Deploy Check ---

func DeployCheck(s *store.Store, cfg *config.Config, id string, opts PipelineOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active\n", id, item.Status)
		return 1
	}

	stepCfg := getStepConfig(cfg, "deploy_check")

	// Health URL check (even without a command)
	if stepCfg != nil && stepCfg.HealthURL != "" {
		timeout := stepCfg.Timeout
		if timeout == 0 {
			timeout = 300
		}
		fmt.Printf("Checking health: %s (timeout %ds)\n", stepCfg.HealthURL, timeout)
		httpGet := opts.HTTPGet
		if httpGet == nil {
			httpGet = defaultHTTPGet
		}
		if err := checkHealth(stepCfg.HealthURL, timeout, httpGet); err != nil {
			fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
			return 1
		}
		fmt.Println("  health: OK")
	}

	if stepCfg == nil || stepCfg.Command == "" {
		// No command — just health check was enough (or nothing configured)
		if stepCfg != nil && stepCfg.HealthURL != "" {
			now := time.Now()
			setNestedField(item, "delivery", "stage", "deployed_dev")
			setNestedField(item, "delivery", "deployed_date", now.Format("2006-01-02"))
			item.Doc.SetField("last_touched", now.Format(time.RFC3339))
			s.Write(item)
			changelog.Append(cfg, id, changelog.Entry{
				Op: "deploy_checked", Field: "delivery.stage", NewValue: "deployed_dev",
			})
			fmt.Printf("Deploy verified for %s\n", id)
			return 0
		}
		fmt.Fprintln(os.Stderr, "pipeline.deploy_check not configured (no command or health_url)")
		return 1
	}

	return runPipelineStep(s, cfg, id, "deploy_check", "deployed_dev", stepCfg, opts)
}

// --- Smoke ---

func Smoke(s *store.Store, cfg *config.Config, id string, opts PipelineOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active\n", id, item.Status)
		return 1
	}

	stepCfg := getStepConfig(cfg, "smoke")
	if stepCfg == nil || stepCfg.Command == "" {
		fmt.Fprintln(os.Stderr, "pipeline.smoke.command not configured")
		return 1
	}

	return runPipelineStep(s, cfg, id, "smoke", "smoke_passed", stepCfg, opts)
}

// --- Shared runner ---

func runPipelineStep(s *store.Store, cfg *config.Config, id, stepName, nextStage string, stepCfg *config.PipelineStepConfig, opts PipelineOpts) int {
	item, _ := s.Get(id)
	runCmd := opts.RunCmd
	if runCmd == nil {
		// Always execute from workspace root so relative paths in config work
		root := cfg.Root()
		runCmd = func(command string) ([]byte, int, error) {
			return runCmdInDir(root, command)
		}
	}

	// Pre-checks
	for _, check := range stepCfg.PreChecks {
		fmt.Printf("  pre-check: %s\n", check)
		output, exitCode, err := runCmd(check)
		if err != nil && exitCode == 0 {
			fmt.Fprintf(os.Stderr, "pre-check failed to execute: %v\n", err)
			return 1
		}
		if exitCode != 0 {
			fmt.Fprintf(os.Stderr, "pre-check failed (exit %d):\n%s\n", exitCode, string(output))
			return 1
		}
	}

	// Main command
	fmt.Printf("Running %s: %s\n", stepName, stepCfg.Command)
	start := time.Now()
	output, exitCode, err := runCmd(stepCfg.Command)
	duration := time.Since(start)

	if err != nil && exitCode == 0 {
		fmt.Fprintf(os.Stderr, "failed to execute: %v\n", err)
		return 1
	}

	now := time.Now()

	// Upload evidence
	backend := opts.Backend
	if backend == nil {
		var berr error
		backend, berr = evidence.New(evidenceConfigFromCfg(cfg))
		if berr != nil {
			fmt.Fprintf(os.Stderr, "creating evidence backend: %v\n", berr)
			return 1
		}
	}

	sha := getSHA(TestRecordOpts{})
	ts := now.Format("20060102T150405")
	keyPrefix := fmt.Sprintf("%s/%s/%s/%s", id, stepName, sha, ts)

	backend.Upload(keyPrefix+"/log.txt", bytes.NewReader(output))

	status := "pass"
	if exitCode != 0 {
		status = "fail"
	}
	summary := map[string]interface{}{
		"status":      status,
		"step":        stepName,
		"sha":         sha,
		"exit_code":   exitCode,
		"duration_ms": duration.Milliseconds(),
		"recorded_at": now.Format(time.RFC3339),
	}
	summaryJSON, _ := json.MarshalIndent(summary, "", "  ")
	backend.Upload(keyPrefix+"/summary.json", bytes.NewReader(summaryJSON))

	// Collect artifacts
	if len(stepCfg.Artifacts) > 0 {
		// Reuse the artifact bundling from testrecord
		count := uploadArtifactsFromPatterns(stepCfg.Artifacts, keyPrefix, backend)
		if count > 0 {
			fmt.Printf("  uploaded %d artifact(s)\n", count)
		}
	}

	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "FAIL %s on %s (exit %d, %dms)\n", stepName, id, exitCode, duration.Milliseconds())
		return 1
	}

	// Post-record command (e.g., capture merge SHA)
	postOutput := ""
	if stepCfg.PostRecord != "" {
		out, _, perr := runCmd(stepCfg.PostRecord)
		if perr == nil {
			postOutput = strings.TrimSpace(string(out))
		}
	}

	// Advance delivery stage
	setNestedField(item, "delivery", "stage", nextStage)
	if stepName == "deploy_check" || nextStage == "deployed_dev" {
		setNestedField(item, "delivery", "deployed_date", now.Format("2006-01-02"))
	}
	if postOutput != "" && stepName == "merge" {
		setNestedField(item, "work_tracking", "merge_sha", postOutput)
	}
	item.Doc.SetField("last_touched", now.Format(time.RFC3339))

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: stepName + "_completed", Field: "delivery.stage", NewValue: nextStage,
	})

	fmt.Printf("PASS %s on %s (%dms) → stage: %s\n", stepName, id, duration.Milliseconds(), nextStage)
	return 0
}

// --- Helpers ---

func getStepConfig(cfg *config.Config, step string) *config.PipelineStepConfig {
	if cfg.Pipeline == nil {
		return nil
	}
	switch step {
	case "merge":
		return cfg.Pipeline.Merge
	case "deploy_check":
		return cfg.Pipeline.DeployCheck
	case "smoke":
		return cfg.Pipeline.Smoke
	}
	return nil
}

func checkHealth(url string, timeout int, httpGet func(string) (int, error)) error {
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		status, err := httpGet(url)
		if err == nil && status == 200 {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d", status)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout after %ds: %v", timeout, lastErr)
}

func defaultHTTPGet(url string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// uploadArtifactsFromPatterns bundles matching files and uploads as tar.gz.
func uploadArtifactsFromPatterns(patterns []string, keyPrefix string, backend evidence.Backend) int {
	var files []string
	for _, pattern := range patterns {
		if strings.Contains(pattern, "**") {
			dir := strings.Split(pattern, "**")[0]
			if dir == "" {
				dir = "."
			}
			filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				files = append(files, path)
				return nil
			})
		} else {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				info, err := os.Stat(m)
				if err != nil || info.IsDir() {
					continue
				}
				files = append(files, m)
			}
		}
	}
	if len(files) == 0 {
		return 0
	}

	var buf bytes.Buffer
	if err := createTarGz(&buf, files); err != nil {
		return 0
	}
	backend.Upload(keyPrefix+"/artifacts.tar.gz", &buf)
	return len(files)
}
