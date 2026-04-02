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

	runCmd := opts.RunCmd
	if runCmd == nil {
		root := cfg.Root()
		runCmd = func(command string) ([]byte, int, error) {
			return runCmdInDir(root, command)
		}
	}

	// Phase 1: Watch CI on main branch (GH Actions run must complete first)
	if stepCfg != nil && stepCfg.WatchCI {
		if err := watchMainCI(runCmd); err != nil {
			fmt.Fprintf(os.Stderr, "CI watch failed: %v\n", err)
			return 1
		}
	}

	// Collect all health URLs (singular + plural config)
	var healthURLs []string
	if stepCfg != nil {
		if stepCfg.HealthURL != "" {
			healthURLs = append(healthURLs, stepCfg.HealthURL)
		}
		for _, u := range stepCfg.HealthURLs {
			found := false
			for _, existing := range healthURLs {
				if existing == u {
					found = true
					break
				}
			}
			if !found {
				healthURLs = append(healthURLs, u)
			}
		}
	}

	// Phase 2: Health URL checks — all must pass within timeout
	// The timeout covers the orchestrator/Lambda deploy time after CI completes
	if len(healthURLs) > 0 {
		timeout := 300
		if stepCfg != nil && stepCfg.Timeout > 0 {
			timeout = stepCfg.Timeout
		}
		httpGet := opts.HTTPGet
		if httpGet == nil {
			httpGet = defaultHTTPGet
		}
		for _, url := range healthURLs {
			fmt.Printf("Checking health: %s (timeout %ds)\n", url, timeout)
			if err := checkHealth(url, timeout, httpGet); err != nil {
				fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
				return 1
			}
			fmt.Println("  health: OK")
		}
	}

	if stepCfg == nil || stepCfg.Command == "" {
		// No command — just health checks were enough (or nothing configured)
		if len(healthURLs) > 0 {
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

	// For merge: gh pr merge exits 1 in worktrees because post-merge
	// "git checkout main" fails (main is checked out elsewhere).
	// Check if the merge actually succeeded despite the exit code.
	if exitCode != 0 && stepName == "merge" {
		outStr := string(output)
		if strings.Contains(outStr, "already merged") || strings.Contains(outStr, "Merged") || strings.Contains(outStr, "merged") {
			fmt.Println("  merge succeeded (post-merge checkout failed in worktree — expected)")
			exitCode = 0
		}
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

	logURI, _ := evidence.GzipUpload(backend, keyPrefix+"/log.txt", output)

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
	evidence.GzipUpload(backend, keyPrefix+"/summary.json", summaryJSON)

	// Collect artifacts
	if len(stepCfg.Artifacts) > 0 {
		// Reuse the artifact bundling from testrecord
		count := uploadArtifactsFromPatterns(stepCfg.Artifacts, keyPrefix, backend)
		if count > 0 {
			fmt.Printf("  uploaded %d artifact(s)\n", count)
		}
	}

	if exitCode != 0 {
		setNestedField(item, "delivery", stepName+"_evidence", logURI)
		s.Write(item)
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

	// Record evidence URI and advance delivery stage
	setNestedField(item, "delivery", stepName+"_evidence", logURI)
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

// watchMainCI watches the latest GH Actions runs on the main/master branch
// across all configured repos until they complete.
func watchMainCI(runCmd func(string) ([]byte, int, error)) error {
	fmt.Println("Watching CI on main branch...")

	// Use gh run list to find the latest in-progress run, then watch it
	// gh run list --branch main --limit 1 --json databaseId,status,conclusion
	deadline := time.Now().Add(20 * time.Minute)
	lastStatus := ""
	for time.Now().Before(deadline) {
		// Watch the deploy workflow — it's the last step after merge.
		// Tests and build run first; deploy triggers after build succeeds.
		out, exitCode, _ := runCmd(`gh run list --branch main --limit 5 --json databaseId,status,conclusion,name --jq '[.[] | select(.name | test("Deploy"; "i"))][0] | "\(.databaseId) \(.status) \(.conclusion)"' 2>/dev/null`)
		if exitCode != 0 {
			// gh not available or no runs — skip CI watch
			fmt.Println("  could not query GH runs — skipping CI watch")
			return nil
		}

		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) < 2 {
			time.Sleep(10 * time.Second)
			continue
		}

		runID := parts[0]
		status := parts[1]

		// jq returns "null null null" when no matching workflow exists
		if runID == "null" || status == "null" {
			fmt.Println("  no Deploy workflow found — skipping CI watch")
			return nil
		}
		conclusion := ""
		if len(parts) >= 3 {
			conclusion = parts[2]
		}

		if status != lastStatus {
			fmt.Printf("  CI run %s: %s", runID, status)
			if conclusion != "" {
				fmt.Printf(" (%s)", conclusion)
			}
			fmt.Println()
			lastStatus = status
		}

		if status == "completed" {
			if conclusion == "success" {
				fmt.Println("  CI passed — proceeding to health checks")
				return nil
			}
			if conclusion == "skipped" {
				// Skipped means path filter didn't match — no CI needed, proceed
				fmt.Println("  CI skipped (no matching changes) — proceeding to health checks")
				return nil
			}
			return fmt.Errorf("CI run %s failed: %s", runID, conclusion)
		}

		time.Sleep(20 * time.Second)
	}
	return fmt.Errorf("CI watch timed out after 20 minutes")
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
