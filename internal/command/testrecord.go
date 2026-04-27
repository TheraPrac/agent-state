package command

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/coverage"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TestRecordOpts holds flags and injectable functions for the test command.
type TestRecordOpts struct {
	Run      bool   // execute the suite command (--run)
	Coverage bool   // enforce per-file coverage (--coverage, requires --run)
	Skip     string // mark scope suite as intentionally skipped with reason (--skip)
	Agent    string // select an agent workspace/runtime explicitly (--agent)
	Cwd      string // injectable cwd for runtime resolution tests
	// Injectable for testing (nil = use real implementations)
	GitHeadSHA func(repoDir string) (string, error)
	RunCmd     func(command string) (output []byte, exitCode int, err error)
	ReadFile   func(path string) ([]byte, error)
	Backend    evidence.Backend
}

// testSummary is the structured result written as summary.json.
type testSummary struct {
	Status     string `json:"status"` // "pass" or "fail"
	Suite      string `json:"suite"`
	SHA        string `json:"sha"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	RecordedAt string `json:"recorded_at"`
}

// TestRecord records or executes a test suite for an item.
func TestRecord(s *store.Store, cfg *config.Config, id, suite string, opts TestRecordOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active to record test evidence\n", id, item.Status)
		return 1
	}

	if cfg.Testing == nil {
		fmt.Fprintln(os.Stderr, "no testing configuration found")
		return 1
	}

	// Look up suite
	suiteCmd := ""
	isRequired := false
	isScope := false
	if sc, ok := cfg.Testing.RequiredSuites[suite]; ok {
		isRequired = true
		suiteCmd = sc.Command
	}
	if sc, ok := cfg.Testing.ScopeSuites[suite]; ok {
		isScope = true
		suiteCmd = sc.Command
	}
	if !isRequired && !isScope {
		fmt.Fprintf(os.Stderr, "unknown suite %q — not in required_suites or scope_suites\n", suite)
		return 1
	}

	// Handle --skip: mark a scope suite as intentionally skipped.
	if opts.Skip != "" {
		if isRequired {
			fmt.Fprintf(os.Stderr, "cannot skip required suite %q\n", suite)
			return 1
		}
		ev := fmt.Sprintf("skip: %s", opts.Skip)
		nowStr := time.Now().Format(time.RFC3339)
		if err := s.Mutate(id, func(it *model.Item) error {
			it.SetNested("testing_evidence", suite, ev)
			it.Doc.SetField("last_touched", nowStr)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
			return 1
		}
		changelog.Append(cfg, id, changelog.Entry{
			Op: "test_skipped", Field: "testing_evidence." + suite, NewValue: ev,
		})
		fmt.Printf("Skipped %s on %s: %s\n", suite, id, opts.Skip)
		return 0
	}

	// For scope suites, warn if not triggered by st pr
	if isScope {
		current, _ := getNestedField(item, "testing_evidence", suite)
		if current != "required" && !strings.HasPrefix(current, "pass") && !strings.HasPrefix(current, "fail") {
			fmt.Fprintf(os.Stderr, "warning: scope suite %q was not triggered by `st pr` — recording anyway\n", suite)
		}
	}

	// Get SHA from the repo this suite targets, not cwd
	sha := getSHAForSuite(cfg, id, suite, suiteCmd, opts)

	if opts.Run {
		return testRunMode(s, cfg, id, suite, suiteCmd, sha, item, opts)
	}
	return testRecordOnly(s, cfg, id, suite, sha, item)
}

// testRecordOnly is the original record-only path (no --run).
func testRecordOnly(s *store.Store, cfg *config.Config, id, suite, sha string, item *model.Item) int {
	now := time.Now()
	ev := fmt.Sprintf("pass %s %s", sha, now.Format(time.RFC3339))

	if err := s.Mutate(id, func(it *model.Item) error {
		it.SetNested("testing_evidence", suite, ev)
		it.Doc.SetField("last_touched", now.Format(time.RFC3339))
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "test_recorded", Field: "testing_evidence." + suite, NewValue: ev,
	})

	fmt.Printf("Recorded %s pass on %s (sha:%s)\n", suite, id, sha)
	return 0
}

// testRunMode executes the suite, captures output, uploads evidence, records result.
func testRunMode(s *store.Store, cfg *config.Config, id, suite, suiteCmd, sha string, item *model.Item, opts TestRecordOpts) int {
	if suiteCmd == "" {
		fmt.Fprintf(os.Stderr, "suite %q has no command configured\n", suite)
		return 1
	}

	cmd := suiteCmd
	// I-400: when the item has an active worktree, the test must run against
	// the feature branch checkout, not the agent-root main branch. Worktree
	// rewrite runs FIRST so its absolute cd path replaces "cd ../theraprac-X"
	// before the agent-workspace rewrite can substitute the agent-root path.
	// Both rewrites match the same `cd ../repo` patterns; whichever fires
	// first wins, and the other becomes a no-op for that cd.
	cmd = rewriteSuiteForWorktree(cfg, id, cmd)
	if runtime, ok, err := resolveTestAgentRuntime(cfg, opts, cmd); err != nil {
		fmt.Fprintf(os.Stderr, "test runtime: %v\n", err)
		return 1
	} else if ok {
		cmd = rewriteSuiteForAgentWorkspace(runtime, cmd)
		cmd = injectAgentRuntimeEnv(runtime, cmd)
		fmt.Printf("Running %s on %s: %s\n", suite, runtime.AgentID, cmd)
	} else {
		fmt.Printf("Running %s: %s\n", suite, cmd)
	}
	start := time.Now()

	// Execute suite command. Stream output to stderr so the user (and
	// activity tracker) sees progress.
	var output []byte
	var exitCode int
	var runErr error
	if opts.RunCmd != nil {
		output, exitCode, runErr = opts.RunCmd(cmd)
	} else {
		output, exitCode, runErr = runCmdInDirStreaming(cfg.Root(), cmd)
	}
	duration := time.Since(start)

	if runErr != nil && exitCode == 0 {
		// runErr with exit 0 means execution failed (not the test)
		fmt.Fprintf(os.Stderr, "failed to execute command: %v\n", runErr)
		return 1
	}

	now := time.Now()
	status := "pass"
	if exitCode != 0 {
		status = "fail"
	}

	// Build summary
	summary := testSummary{
		Status:     status,
		Suite:      suite,
		SHA:        sha,
		ExitCode:   exitCode,
		DurationMs: duration.Milliseconds(),
		RecordedAt: now.Format(time.RFC3339),
	}

	// Upload evidence (best-effort — don't fail the test recording if upload fails)
	backend := opts.Backend
	if backend == nil {
		var berr error
		backend, berr = evidence.New(evidenceConfigFromCfg(cfg))
		if berr != nil {
			fmt.Fprintf(os.Stderr, "warning: evidence backend unavailable: %v\n", berr)
		}
	}

	ts := now.Format("20060102T150405")
	keyPrefix := fmt.Sprintf("%s/%s/%s/%s", id, suite, sha, ts)
	logURI := ""

	if backend != nil {
		// Upload log.txt (gzipped)
		uri, err := evidence.GzipUpload(backend, keyPrefix+"/log.txt", output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: evidence upload failed: %v\n", err)
		} else {
			logURI = uri
		}

		// Upload summary.json (gzipped)
		summaryJSON, _ := json.MarshalIndent(summary, "", "  ")
		evidence.GzipUpload(backend, keyPrefix+"/summary.json", summaryJSON)
	}

	// If test failed, record failure and stop
	if exitCode != 0 {
		ev := fmt.Sprintf("fail %s %s evidence:%s", sha, now.Format(time.RFC3339), logURI)
		_ = s.Mutate(id, func(it *model.Item) error {
			it.SetNested("testing_evidence", suite, ev)
			it.Doc.SetField("last_touched", now.Format(time.RFC3339))
			return nil
		})

		changelog.Append(cfg, id, changelog.Entry{
			Op: "test_failed", Field: "testing_evidence." + suite, NewValue: ev,
		})

		fmt.Printf("FAIL %s on %s (exit %d, %dms)\n", suite, id, exitCode, duration.Milliseconds())
		return 1
	}

	// Upload artifacts (if configured)
	_, isReq := cfg.Testing.RequiredSuites[suite]
	_, isScp := cfg.Testing.ScopeSuites[suite]
	artifactCount := uploadArtifacts(cfg, suite, keyPrefix, isReq, isScp, backend)
	if artifactCount > 0 {
		fmt.Printf("  uploaded %d artifact(s)\n", artifactCount)
	}

	// Coverage enforcement (only when --coverage and test passed)
	if opts.Coverage {
		if code := enforceCoverage(cfg, id, suite, sha, keyPrefix, item, opts, backend); code != 0 {
			return code
		}
	}

	// Record pass
	ev := fmt.Sprintf("pass %s %s evidence:%s", sha, now.Format(time.RFC3339), logURI)
	if err := s.Mutate(id, func(it *model.Item) error {
		it.SetNested("testing_evidence", suite, ev)
		it.Doc.SetField("last_touched", now.Format(time.RFC3339))
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "test_executed", Field: "testing_evidence." + suite, NewValue: ev,
	})

	fmt.Printf("PASS %s on %s (%dms) evidence:%s\n", suite, id, duration.Milliseconds(), logURI)
	return 0
}

// enforceCoverage parses coverage reports and checks thresholds against manifest files.
func enforceCoverage(cfg *config.Config, id, suite, sha, keyPrefix string, item *model.Item, opts TestRecordOpts, backend evidence.Backend) int {
	// Determine coverage format from suite prefix
	isGo := strings.HasPrefix(suite, "api_")
	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}

	var fileCov map[string]coverage.FileCoverage
	var covErr error

	if isGo {
		// Look for cover.out in the repo directory
		data, err := readFile("cover.out")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: --coverage specified but cover.out not found: %v\n", err)
			return 0 // non-fatal: coverage file might not exist
		}
		// Upload raw coverage
		backend.Upload(keyPrefix+"/cover.out", bytes.NewReader(data))

		// Determine module path from go.mod
		modPath := ""
		if modData, err := readFile("go.mod"); err == nil {
			for _, line := range strings.Split(string(modData), "\n") {
				if strings.HasPrefix(line, "module ") {
					modPath = strings.TrimPrefix(line, "module ") + "/"
					break
				}
			}
		}
		fileCov, covErr = coverage.ParseGoCoverprofile(bytes.NewReader(data), modPath)
	} else {
		// Vitest JSON summary
		data, err := readFile("coverage/coverage-summary.json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: --coverage specified but coverage-summary.json not found: %v\n", err)
			return 0
		}
		backend.Upload(keyPrefix+"/coverage-summary.json", bytes.NewReader(data))
		fileCov, covErr = coverage.ParseVitestSummary(bytes.NewReader(data))
	}

	if covErr != nil {
		fmt.Fprintf(os.Stderr, "parsing coverage: %v\n", covErr)
		return 1
	}

	// Upload parsed coverage as JSON
	covJSON, _ := json.MarshalIndent(fileCov, "", "  ")
	backend.Upload(keyPrefix+"/coverage.json", bytes.NewReader(covJSON))

	// Load manifest to get changed app files
	m, err := manifest.Load(cfg.ManifestDir(), id)
	if err != nil || len(m.PRs) == 0 {
		fmt.Fprintln(os.Stderr, "warning: no manifest found — skipping per-file coverage check")
		return 0
	}

	// Collect app files from the repo matching this suite
	repo := strings.Split(suite, "_")[0]
	var appFiles []string
	for _, pr := range m.PRs {
		if pr.Repo != repo {
			continue
		}
		for _, f := range pr.Files {
			if f.Type == "app" {
				appFiles = append(appFiles, f.Path)
			}
		}
	}

	if len(appFiles) == 0 {
		return 0
	}

	// Check thresholds
	thresh := coverage.DefaultThresholds()
	if cfg.Testing != nil && cfg.Testing.CoverageThresholds != nil {
		thresh = coverage.Thresholds{
			Lines:     cfg.Testing.CoverageThresholds.Lines,
			Branches:  cfg.Testing.CoverageThresholds.Branches,
			Functions: cfg.Testing.CoverageThresholds.Functions,
		}
	}

	violations := coverage.CheckThresholds(fileCov, appFiles, thresh)
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "coverage violations on changed files (%d):\n", len(violations))
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		return 1
	}

	fmt.Printf("  coverage: %d changed files meet thresholds\n", len(appFiles))
	return 0
}

// getSHAForSuite gets the HEAD SHA from the repo targeted by the suite.
func getSHAForSuite(cfg *config.Config, itemID, suite, suiteCmd string, opts TestRecordOpts) string {
	if opts.GitHeadSHA != nil {
		return getSHA(opts)
	}
	// Determine repo from suite prefix (api_unit → api repo, web_e2e → web repo)
	repo := strings.Split(suite, "_")[0]
	if runtime, ok, err := resolveTestAgentRuntime(cfg, opts, suiteCmd); err == nil && ok {
		if dir := runtime.RepoPaths[repoDirForSuiteRepo(repo)]; dir != "" {
			out, err := runGit(dir, "rev-parse", "HEAD")
			if err == nil {
				sha := strings.TrimSpace(out)
				if len(sha) > 7 {
					sha = sha[:7]
				}
				return sha
			}
		}
	}
	// Find matching repo name in worktree config
	if cfg.Worktree != nil {
		for _, r := range cfg.Worktree.Repos {
			if strings.Contains(r, repo) {
				dir := resolveRepoDirForItem(cfg, itemID, r)
				out, err := runGit(dir, "rev-parse", "HEAD")
				if err == nil {
					sha := strings.TrimSpace(out)
					if len(sha) > 7 {
						sha = sha[:7]
					}
					return sha
				}
			}
		}
	}
	return getSHA(opts)
}

func repoDirForSuiteRepo(repo string) string {
	switch repo {
	case "api", "web", "infra":
		return "theraprac-" + repo
	default:
		return repo
	}
}

func getSHA(opts TestRecordOpts) string {
	sha := "unknown"
	if opts.GitHeadSHA != nil {
		out, err := opts.GitHeadSHA(".")
		if err == nil {
			sha = strings.TrimSpace(out)
		}
	} else {
		out, err := runGit(".", "rev-parse", "HEAD")
		if err == nil {
			sha = strings.TrimSpace(out)
		}
	}
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return sha
}

func defaultRunCmd(command string) ([]byte, int, error) {
	return runCmdInDir("", command)
}

// runCmdInDir executes a shell command in a specific directory.
// If dir is empty, uses the current working directory.
func runCmdInDir(dir, command string) ([]byte, int, error) {
	cmd := exec.Command("sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return output, 0, err
		}
	}
	return output, exitCode, nil
}

// runCmdInDirStreaming executes a command, streams output to stderr in real time,
// and returns the captured output for evidence upload.
// Uses pipe + goroutine to avoid backpressure (MultiWriter blocks Playwright).
func runCmdInDirStreaming(dir, command string) ([]byte, int, error) {
	cmd := exec.Command("sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}

	// Use a pipe so we can read output asynchronously
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}

	// Read output in a goroutine — write to stderr (live) and buffer (capture)
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		tmp := make([]byte, 8192)
		for {
			n, readErr := stdoutPipe.Read(tmp)
			if n > 0 {
				os.Stderr.Write(tmp[:n])
				buf.Write(tmp[:n])
			}
			if readErr != nil {
				break
			}
		}
	}()

	cmdErr := cmd.Wait()
	<-done // wait for reader to finish

	exitCode := 0
	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return buf.Bytes(), 0, err
		}
	}
	return buf.Bytes(), exitCode, nil
}

// uploadArtifacts globs artifact patterns from the suite config, bundles matches
// into a tar.gz, and uploads it. Returns the number of files bundled.
func uploadArtifacts(cfg *config.Config, suite, keyPrefix string, isRequired, isScope bool, backend evidence.Backend) int {
	if cfg.Testing == nil {
		return 0
	}

	var patterns []string
	if isRequired {
		if sc, ok := cfg.Testing.RequiredSuites[suite]; ok {
			patterns = sc.Artifacts
		}
	}
	if isScope {
		if sc, ok := cfg.Testing.ScopeSuites[suite]; ok {
			patterns = sc.Artifacts
		}
	}
	if len(patterns) == 0 {
		return 0
	}

	// Glob all patterns and collect matching files
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		// Glob doesn't recurse into ** — handle manually
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

	// Bundle into tar.gz
	var buf bytes.Buffer
	if err := createTarGz(&buf, files); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to bundle artifacts: %v\n", err)
		return 0
	}

	// Upload
	key := keyPrefix + "/artifacts.tar.gz"
	if _, err := backend.Upload(key, &buf); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to upload artifacts: %v\n", err)
		return 0
	}

	return len(files)
}

// createTarGz creates a tar.gz archive from a list of file paths.
func createTarGz(w io.Writer, files []string) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			continue
		}
		header.Name = path // preserve relative path

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			continue
		}
		io.Copy(tw, f)
		f.Close()
	}
	return nil
}

// rewriteSuiteForWorktree rewrites a suite command's `cd ../repo` to use the
// item's worktree path instead of the main checkout.
// e.g., "cd ../theraprac-api && make test-unit" → "cd worktrees/I-112/theraprac-api && make test-unit"
func rewriteSuiteForWorktree(cfg *config.Config, itemID, suiteCmd string) string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled || cfg.Worktree.BaseDir == "" {
		return suiteCmd
	}

	wtBase := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir, itemID)
	if _, err := os.Stat(wtBase); err != nil {
		return suiteCmd // no worktree for this item
	}

	// Replace "cd ../repo-name" with "cd <worktree-path>/repo-name"
	for _, repo := range cfg.Worktree.Repos {
		repoDir := repo
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				repoDir = mapped
			}
		}
		wtRepo := filepath.Join(wtBase, repoDir)
		if _, err := os.Stat(wtRepo); err == nil {
			// Replace various relative path patterns
			for _, pattern := range []string{
				"cd ../" + repoDir,
				"cd ../" + repo,
			} {
				if strings.Contains(suiteCmd, pattern) {
					suiteCmd = strings.Replace(suiteCmd, pattern, "cd "+wtRepo, 1)
					return suiteCmd
				}
			}
		}
	}

	return suiteCmd
}

type testAgentRuntime struct {
	AgentID        string
	WorkspaceDir   string
	ComposeProject string
	Ports          agentWorkspacePorts
	RepoPaths      map[string]string
}

func resolveTestAgentRuntime(cfg *config.Config, opts TestRecordOpts, suiteCmd string) (testAgentRuntime, bool, error) {
	if cfg == nil {
		return testAgentRuntime{}, false, nil
	}
	if opts.Agent != "" {
		plan, err := buildAgentWorkspacePlan(cfg, opts.Agent, "")
		if err != nil {
			return testAgentRuntime{}, false, err
		}
		return testRuntimeFromPlan(plan), true, nil
	}

	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return testAgentRuntime{}, false, err
		}
	}
	agentID, ok := agentIDFromPath(cwd)
	if !ok {
		agentID, ok = agentIDFromPath(cfg.Root())
	}
	if !ok {
		if !suiteNeedsAgentRuntime(suiteCmd) {
			return testAgentRuntime{}, false, nil
		}
		return testAgentRuntime{}, false, fmt.Errorf("cannot resolve agent workspace from cwd; rerun with --agent <id>")
	}
	plan, err := buildAgentWorkspacePlan(cfg, agentID, "")
	if err != nil {
		return testAgentRuntime{}, false, err
	}
	return testRuntimeFromPlan(plan), true, nil
}

func suiteNeedsAgentRuntime(suiteCmd string) bool {
	return strings.Contains(suiteCmd, "../theraprac-") || strings.Contains(suiteCmd, "../../theraprac-")
}

func testRuntimeFromPlan(plan agentWorkspacePlan) testAgentRuntime {
	rt := testAgentRuntime{
		AgentID:        plan.AgentID,
		WorkspaceDir:   plan.TargetDir,
		ComposeProject: plan.ComposeProject,
		Ports:          plan.Ports,
		RepoPaths:      map[string]string{},
	}
	for _, repo := range plan.Repos {
		rt.RepoPaths[repo.Name] = repo.TargetPath
	}
	return rt
}

func agentIDFromPath(path string) (string, bool) {
	clean := filepath.Clean(path)
	for {
		base := filepath.Base(clean)
		if strings.HasPrefix(base, "theraprac-agent-") {
			return strings.TrimPrefix(base, "theraprac-"), true
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return "", false
		}
		clean = parent
	}
}

func rewriteSuiteForAgentWorkspace(runtime testAgentRuntime, suiteCmd string) string {
	for repo, path := range runtime.RepoPaths {
		for _, pattern := range []string{
			"cd ../" + repo,
			"cd ../../" + repo,
		} {
			if strings.Contains(suiteCmd, pattern) {
				return strings.Replace(suiteCmd, pattern, "cd "+shellQuote(path), 1)
			}
		}
	}
	return suiteCmd
}

func injectAgentRuntimeEnv(runtime testAgentRuntime, suiteCmd string) string {
	env := map[string]string{
		"AS_AGENT_ID":          runtime.AgentID,
		"ST_AGENT_ID":          runtime.AgentID,
		"THERAPRAC_AGENT_ID":   runtime.AgentID,
		"COMPOSE_PROJECT_NAME": runtime.ComposeProject,
		"THERAPRAC_WEB_PORT":   fmt.Sprintf("%d", runtime.Ports.Web),
		"THERAPRAC_API_PORT":   fmt.Sprintf("%d", runtime.Ports.API),
		"THERAPRAC_DB_PORT":    fmt.Sprintf("%d", runtime.Ports.DB),
		"MAILPIT_PORT":         fmt.Sprintf("%d", runtime.Ports.Mailpit),
		"STRIPE_WEBHOOK_PORT":  fmt.Sprintf("%d", runtime.Ports.Stripe),
		"API_BASE_URL":         fmt.Sprintf("http://localhost:%d", runtime.Ports.API),
		"NEXT_PUBLIC_API_URL":  fmt.Sprintf("http://localhost:%d", runtime.Ports.API),
		"PLAYWRIGHT_BASE_URL":  fmt.Sprintf("http://localhost:%d", runtime.Ports.Web),
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s ", key, shellQuote(env[key]))
	}
	b.WriteString(suiteCmd)
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func evidenceConfigFromCfg(cfg *config.Config) evidence.Config {
	if cfg.Evidence != nil {
		return evidence.Config{
			Backend:   cfg.Evidence.Backend,
			LocalDir:  cfg.EvidenceDir(),
			S3Bucket:  cfg.Evidence.S3Bucket,
			S3Region:  cfg.Evidence.S3Region,
			S3Prefix:  cfg.Evidence.S3Prefix,
			S3Profile: cfg.Evidence.S3Profile,
		}
	}
	return evidence.Config{
		Backend:  "local",
		LocalDir: cfg.EvidenceDir(),
	}
}
