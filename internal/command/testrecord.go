package command

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/awsauth"
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
	// I-802: cache roots used for cache-contended suites and whether a
	// lock-aware retry fired. Omitempty keeps non-contended-suite summaries
	// unchanged from pre-I-802 evidence.
	GolangciLintCache string `json:"golangci_lint_cache,omitempty"`
	GoCache           string `json:"go_cache,omitempty"`
	Retried           bool   `json:"retried,omitempty"`
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

	// I-776: route required-suite lookup through the central helper so this
	// command agrees with the gate on which suites apply to THIS item. Unknown
	// scope_class fails fast — without this check, an agent could `st test`
	// against the default class's suites for an item the gate will later
	// reject for an unknown class, leaving dirty evidence behind.
	requiredSuites, classOK := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	if !classOK {
		fmt.Fprintf(os.Stderr, "unknown scope_class %q — declare in config.testing.scope_classes or remove from item\n", item.ScopeClass)
		return 1
	}

	// Suite-name precedence: class-required (when scope_class is set) → default
	// required → scope. ScopeSuites lookup is now an `else if` instead of
	// unconditional so a name collision (same suite in a class and ScopeSuites)
	// cannot silently overwrite the resolved class command.
	suiteCmd := ""
	isRequired := false
	isScope := false
	if sc, ok := requiredSuites[suite]; ok {
		isRequired = true
		suiteCmd = sc.Command
	} else if sc, ok := cfg.Testing.ScopeSuites[suite]; ok {
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
		if err := autoSync(s, fmt.Sprintf("st test skip: %s %s", id, suite)); err != nil {
			return 1
		}
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
	if err := autoSync(s, fmt.Sprintf("st test: %s %s", id, suite)); err != nil {
		return 1
	}
	return 0
}

// testRunMode executes the suite, captures output, uploads evidence, records result.
func testRunMode(s *store.Store, cfg *config.Config, id, suite, suiteCmd, sha string, item *model.Item, opts TestRecordOpts) int {
	if suiteCmd == "" {
		fmt.Fprintf(os.Stderr, "suite %q has no command configured\n", suite)
		return 1
	}

	// I-586: load the agent's AWS session into env vars BEFORE the
	// preflight so the probe upload uses the right credentials.
	// No-op when no agent identity (developer running st test --run
	// from their own shell) or when AWS_ACCESS_KEY_ID is already set
	// (operator sourced agent-aws-auth.sh themselves).
	if err := awsauth.EnsureAgentSession(cfg.AgentID(), cfg.Root()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load agent AWS session: %v\n", err)
		// Don't fail here — let the preflight surface the resulting
		// upload failure with its own clear message + runbook.
	}

	// I-587: preflight evidence-write. A 1-byte probe upload to a
	// __health/preflight-* key proves the configured backend can
	// write before we sink minutes into the test command. Hard-fail
	// if it can't; the opt-out env var exists for offline/dev cases
	// where deliberately recording without uploading is fine.
	if os.Getenv("AS_SKIP_EVIDENCE_PREFLIGHT") != "1" {
		if rc := preflightEvidenceWrite(cfg, opts); rc != 0 {
			return rc
		}
	}

	cmd := suiteCmd
	// I-400: when the item has an active worktree, the test must run against
	// the feature branch checkout, not the agent-root main branch. Worktree
	// rewrite runs FIRST so its absolute cd path replaces "cd ../theraprac-X"
	// before the agent-workspace rewrite can substitute the agent-root path.
	// Both rewrites match the same `cd ../repo` patterns; whichever fires
	// first wins, and the other becomes a no-op for that cd.
	cmd = rewriteSuiteForWorktree(cfg, id, cmd)
	// I-1116: guard against silently running a cross-repo suite against the main
	// clone when an active worktree exists for this item. Mirror the same repo/
	// repoDir pattern set that rewriteSuiteForWorktree iterates — if any of those
	// patterns still appear in cmd, the rewrite did not fire (the repo dir is absent
	// inside the worktree). Fail loudly; running against the main clone produces
	// misleading pass/fail signals.
	// NOTE: the RepoMap resolution below must stay in sync with rewriteSuiteForWorktree.
	if cfg.Worktree != nil && cfg.Worktree.Enabled && cfg.Worktree.BaseDir != "" {
		if wtBase := cfg.WorktreeForItem(id); wtBase != "" {
			wtExists := true
			if _, statErr := os.Stat(wtBase); statErr != nil {
				wtExists = false
			}
			for _, repo := range cfg.Worktree.Repos {
				repoDir := repo
				if cfg.Worktree.RepoMap != nil {
					if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
						repoDir = mapped
					}
				}
				// Deduplicate: when repoDir == repo (no RepoMap entry), checking both
				// patterns would run strings.Contains twice on the same string.
				patterns := []string{"cd ../" + repoDir}
				if repo != repoDir {
					patterns = append(patterns, "cd ../"+repo)
				}
				for _, pattern := range patterns {
					if strings.Contains(cmd, pattern) {
						if !wtExists {
							// Worktree never created or was removed — cmd references a repo
							// that can only run correctly inside the worktree.
							fmt.Fprintf(os.Stderr,
								"error: item %s has no active worktree at %s — running would produce misleading results.\n"+
									"  Run `st start %s` to recreate the worktree, then retry.\n",
								id, wtBase, id)
						} else {
							// Worktree exists but the repo dir is missing inside it —
							// the rewrite did not fire, so cmd still points at the main clone.
							fmt.Fprintf(os.Stderr,
								"error: suite %q references %q but the worktree rewrite did not fire for item %s.\n"+
									"  Running would execute against the main clone, not the feature-branch worktree.\n"+
									"  Expected repo at: %s\n"+
									"  If the repo dir is missing, run `st start %s` to recreate the worktree.\n",
								suite, pattern, id, filepath.Join(wtBase, repoDir), id)
						}
						return 1
					}
				}
			}
		}
	}
	var resolvedRuntime testAgentRuntime
	var runtimeResolved bool
	if runtime, ok, err := resolveTestAgentRuntime(cfg, opts, cmd); err != nil {
		fmt.Fprintf(os.Stderr, "test runtime: %v\n", err)
		return 1
	} else if ok {
		resolvedRuntime = runtime
		runtimeResolved = true
		cmd = rewriteSuiteForAgentWorkspace(runtime, cmd)
	}
	// I-802: compute cache paths ONCE (not twice — env injection + summary)
	// and treat any failure as loud, never silent. Silent fall-through to the
	// shared cache is exactly the regression I-802 is supposed to fix.
	var golangciLintCache, goCache string
	if isCacheContendedSuite(suite) {
		switch {
		case !runtimeResolved:
			fmt.Fprintf(os.Stderr,
				"I-802 warning: suite %q is cache-contended but no agent runtime resolved; "+
					"running against the shared golangci-lint/Go cache (lock contention possible). "+
					"Re-run with --agent <id> or from an agent workspace to enable per-agent caches.\n",
				suite)
		default:
			gl, gc, err := cachePathsForRuntime(cfg.Root(), resolvedRuntime.AgentID)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"I-802 warning: failed to create per-agent caches for %q on %s: %v\n"+
						"  running against the shared cache; lock contention possible.\n",
					suite, resolvedRuntime.AgentID, err)
			} else {
				golangciLintCache = gl
				goCache = gc
			}
		}
	}
	if runtimeResolved {
		cmd = injectAgentRuntimeEnv(resolvedRuntime, golangciLintCache, goCache, cmd)
		fmt.Printf("Running %s on %s: %s\n", suite, resolvedRuntime.AgentID, cmd)
	} else {
		fmt.Printf("Running %s: %s\n", suite, cmd)
	}
	start := time.Now()

	// Execute suite command. Stream output to stderr so the user (and
	// activity tracker) sees progress.
	runOnce := func() ([]byte, int, error) {
		if opts.RunCmd != nil {
			return opts.RunCmd(cmd)
		}
		return runCmdInDirStreaming(cfg.Root(), cmd)
	}
	result := runWithLockAwareRetry(suite, opts.RunCmd != nil, runOnce)
	output, exitCode, runErr, retried := result.Output, result.ExitCode, result.RunErr, result.Retried
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
		Status:            status,
		Suite:             suite,
		SHA:               sha,
		ExitCode:          exitCode,
		DurationMs:        duration.Milliseconds(),
		RecordedAt:        now.Format(time.RFC3339),
		GolangciLintCache: golangciLintCache,
		GoCache:           goCache,
		Retried:           retried,
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

	// I-802: surface retried= in the human-readable evidence string so
	// `st show` reports cache-flakes without requiring an S3 round-trip to
	// summary.json.gz. Keep the prefix ("pass" / "fail") stable since
	// downstream tooling matches on it.
	retryTag := ""
	if retried {
		retryTag = " retried"
	}

	// If test failed, record failure and stop
	if exitCode != 0 {
		ev := fmt.Sprintf("fail%s %s %s evidence:%s", retryTag, sha, now.Format(time.RFC3339), logURI)
		_ = s.Mutate(id, func(it *model.Item) error {
			it.SetNested("testing_evidence", suite, ev)
			it.Doc.SetField("last_touched", now.Format(time.RFC3339))
			return nil
		})

		changelog.Append(cfg, id, changelog.Entry{
			Op: "test_failed", Field: "testing_evidence." + suite, NewValue: ev,
		})

		fmt.Printf("FAIL %s on %s (exit %d, %dms)\n", suite, id, exitCode, duration.Milliseconds())
		autoSync(s, fmt.Sprintf("st test fail: %s %s", id, suite)) //nolint:errcheck
		return 1
	}

	// Upload artifacts (if configured). I-776: pass through the item's
	// scope_class so uploadArtifacts looks in the right bucket — a class-only
	// required suite has its Artifacts under cfg.Testing.ScopeClasses[class],
	// not under the default RequiredSuites.
	artifactCount := uploadArtifacts(cfg, suite, keyPrefix, item.ScopeClass, backend)
	if artifactCount > 0 {
		fmt.Printf("  uploaded %d artifact(s)\n", artifactCount)
	}

	// Coverage enforcement (only when --coverage and test passed)
	if opts.Coverage {
		if code := enforceCoverage(cfg, id, suite, sha, keyPrefix, item, opts, backend); code != 0 {
			return code
		}
	}

	// Record pass (with I-802 retry tag when applicable)
	ev := fmt.Sprintf("pass%s %s %s evidence:%s", retryTag, sha, now.Format(time.RFC3339), logURI)
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
	if err := autoSync(s, fmt.Sprintf("st test: %s %s", id, suite)); err != nil {
		return 1
	}
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

// runCmdInDirWithTimeout executes a shell command like runCmdInDir but cancels
// after the given timeout. On timeout the exit code is -1 and the error message
// describes how long the command ran before being killed.
func runCmdInDirWithTimeout(dir, command string, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	// Put the child in its own process group so the Cancel kills the whole
	// tree (grandchildren of sh), not just sh itself — mirrors the I-752
	// pattern in runCmdGuarded.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	output, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg := fmt.Sprintf("exit timeout: command exceeded %.0fs\n%s", timeout.Seconds(), string(output))
		return []byte(msg), -1, fmt.Errorf("killed: timeout after %.0fs", timeout.Seconds())
	}
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
//
// I-776: precedence mirrors the suite lookup in TestRecord — class-required
// (when scopeClass is set) → default required → scope. The artifact bucket
// has to match the bucket the suite came from, or class-only suites would
// silently drop their configured Artifacts patterns.
func uploadArtifacts(cfg *config.Config, suite, keyPrefix, scopeClass string, backend evidence.Backend) int {
	if cfg.Testing == nil {
		return 0
	}

	var patterns []string
	if requiredSuites, ok := cfg.Testing.RequiredSuitesFor(scopeClass); ok {
		if sc, found := requiredSuites[suite]; found {
			patterns = sc.Artifacts
		}
	}
	if patterns == nil {
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

	wtBase := cfg.WorktreeForItem(itemID)
	if wtBase == "" {
		return suiteCmd
	}
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

	// Implicit (cwd-based) agent resolution is only worth doing when the
	// suite command actually needs agent runtime injection — i.e. when it
	// references a sibling repo via `cd ../theraprac-...`. Skipping the
	// resolution here keeps tests with self-contained suite commands from
	// requiring THERAPRAC_AGENTS_ROOT just because the dev cwd happens to
	// be under a theraprac-agent-* dir.
	if !suiteNeedsAgentRuntime(suiteCmd) {
		return testAgentRuntime{}, false, nil
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

// injectAgentRuntimeEnv prefixes suiteCmd with the agent runtime env vars
// (ports, identity, compose project) plus the optional per-agent cache
// vars from I-802. Empty cache paths skip the cache vars — callers compute
// paths upstream so the decision is centralized in testRunMode and never
// silent.
func injectAgentRuntimeEnv(runtime testAgentRuntime, golangciLintCache, goCache, suiteCmd string) string {
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
	if golangciLintCache != "" {
		env["GOLANGCI_LINT_CACHE"] = golangciLintCache
	}
	if goCache != "" {
		env["GOCACHE"] = goCache
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

// isCacheContendedSuite returns true for suites whose underlying tool
// takes a process-wide flock on a shared cache directory and therefore
// serializes (or fails) when multiple agents run concurrently. Start
// narrow with api_lint (golangci-lint's analysis cache); extend only
// when contention is observed elsewhere — keeps blast radius scoped to
// the suite we know is hurting (I-802).
func isCacheContendedSuite(suite string) bool {
	switch suite {
	case "api_lint":
		return true
	}
	return false
}

// cachePathsForRuntime returns the per-agent golangci-lint and Go build
// cache directories under <workspace-root>/.as/cache/, creating them lazily.
// Each agent's clone has its own workspace, so the resulting paths are
// structurally isolated from peer agents — no shared flock, no contention.
//
// Rollback (round-2 review): on second-MkdirAll failure we only RemoveAll
// the first directory when our pre-check said it didn't exist as a
// directory. `dirExists` (from spawn.go) returns false on any Stat
// failure — including EACCES — which is the safe default: when we can't
// confirm a directory was already there, we treat it as fresh and
// rollback. A pre-existing warm cache (100s of MB built up across prior
// runs) is preserved because Stat would have succeeded. Rollback errors
// are logged but not propagated — a partial leftover is recoverable on
// the next call but worth surfacing for diagnostics.
//
// Note (gitignore): callers ensure <root>/.as/cache/ is gitignored at the
// workspace level. The companion `.as/cache/` line lives in
// theraprac-workspace/.gitignore (added in the I-802 review-round-2 sweep
// — verify with `grep '\.as/cache/' theraprac-workspace/.gitignore`).
func cachePathsForRuntime(root, agentID string) (golangciLintCache, goCache string, err error) {
	if root == "" || agentID == "" {
		return "", "", fmt.Errorf("cachePathsForRuntime: root and agentID required")
	}
	golangciLintCache = filepath.Join(root, ".as", "cache", "golangci-lint", agentID)
	goCache = filepath.Join(root, ".as", "cache", "go-build", agentID)
	glPreexisted := dirExists(golangciLintCache)
	if err := os.MkdirAll(golangciLintCache, 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(goCache, 0o755); err != nil {
		// Only roll back when WE just created golangciLintCache — never
		// destroy a pre-existing warm cache on a transient goCache error.
		if !glPreexisted {
			if rmErr := os.RemoveAll(golangciLintCache); rmErr != nil {
				fmt.Fprintf(os.Stderr,
					"I-802 warning: failed to roll back fresh cache dir %s after goCache MkdirAll error: %v\n",
					golangciLintCache, rmErr)
			}
		}
		return "", "", err
	}
	return golangciLintCache, goCache, nil
}

// cacheLockErrorPattern is built from specific phrases golangci-lint and
// the Go file-lock helper actually emit on contention — no greedy `.*`,
// no Go runtime panic strings (a race in the linter is a bug to surface,
// not a transient to retry around).
//
// Round-2 review fixes:
//   - Anchor `lock` with `[^-\pL\pN_]|$` (NOT `\b` — `\b` matches between
//     a word char and a non-word char, and `-` is non-word, so `\b` would
//     still match in "lock-free" / "lockable" via the `able` form). We want:
//     no `-` and no Unicode letter/digit/underscore right after `lock`.
//     `\pL` and `\pN` cover any-script letters/digits so a localized error
//     doesn't accidentally re-open the false-positive window.
//   - Removed the dead `cannot to acquire` branch — grammatically wrong,
//     real tools say "cannot acquire". Split into two clauses so the
//     transitive ("failed/unable to acquire") and intransitive ("cannot
//     acquire") forms are both expressible.
//
// Sources (manually verified against upstream stderr):
//   - "is being used by another process" — Windows-style EBUSY surfaced
//     by Go's lockedfile when another process holds the cache flock
//     (golang/go/src/cmd/go/internal/lockedfile).
//   - "failed to acquire file lock" / "unable to acquire file lock" —
//     golangci-lint v1's cache layer (internal/cache/default.go).
//   - "cannot acquire lock" — alternate intransitive phrasing.
//   - "cache directory is locked" — older golangci-lint variant.
//
// Extend by appending a tested phrase, never by widening with `.*`.
var cacheLockErrorPattern = regexp.MustCompile(
	`(?i)(is being used by another process` +
		`|(failed|unable) to acquire (file )?lock([^-\pL\pN_]|$)` +
		`|cannot acquire (file )?lock([^-\pL\pN_]|$)` +
		`|cache directory is locked([^-\pL\pN_]|$))`,
)

// looksLikeCacheLockError reports whether the suite output matches one of
// the documented cache-contention phrases (see cacheLockErrorPattern).
// False positives only cost one bonus retry; false negatives leave the
// original behavior in place.
func looksLikeCacheLockError(output []byte) bool {
	return cacheLockErrorPattern.Match(output)
}

// lockAwareRetryResult bundles the four return values of
// runWithLockAwareRetry so call sites don't depend on positional ordering
// and a future addition (e.g. RetryReason) can land without silently
// misaligning a destructure.
type lockAwareRetryResult struct {
	Output   []byte // first attempt + delimiter + second attempt when Retried
	ExitCode int
	RunErr   error
	Retried  bool
}

// runWithLockAwareRetry runs the suite once. If it failed AND the suite
// is cache-contended AND the output matches a lock-shaped signature, it
// sleeps a jittered 5–15s and runs once more. The returned Output
// concatenates BOTH attempts (separated by a clearly-labeled boundary) so
// evidence preserves the cache-lock signature that motivated the retry —
// without that, retried=true is unverifiable months later. Sleep is
// skipped when runFn was injected (test mode) so unit tests don't add
// wall time.
//
// Cancellability note: the time.Sleep here is uninterruptible — Ctrl-C
// during the jitter window won't fire until after the second attempt
// returns. Plumbing a context.Context through TestRecordOpts is a
// scope-adjacent change tracked separately.
func runWithLockAwareRetry(suite string, injected bool, runFn func() ([]byte, int, error)) lockAwareRetryResult {
	output, exitCode, runErr := runFn()
	if exitCode == 0 || !isCacheContendedSuite(suite) || !looksLikeCacheLockError(output) {
		return lockAwareRetryResult{Output: output, ExitCode: exitCode, RunErr: runErr, Retried: false}
	}
	// Round-3 review: surface a first-attempt runErr ONLY when it's not
	// *exec.ExitError. exec.ExitError just wraps a non-zero status which
	// is already reflected in exitCode; any OTHER error type (chdir
	// failure, binary-not-found, injected non-exec err from a test) is
	// information the operator would otherwise lose when the retry path
	// concatenates outputs.
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			fmt.Fprintf(os.Stderr,
				"I-802: %s first attempt returned non-ExitError runErr alongside lock-shaped output: %v\n",
				suite, runErr)
		}
	}
	if !injected {
		jitter := time.Duration(5000+rand.Intn(10001)) * time.Millisecond
		fmt.Fprintf(os.Stderr, "I-802: %s hit cache-lock signature; retrying after %s\n", suite, jitter)
		time.Sleep(jitter)
	}
	output2, exitCode2, runErr2 := runFn()
	// Concatenate so evidence shows BOTH attempts. Delimiter is fixed so
	// downstream tooling can grep for it.
	var combined bytes.Buffer
	combined.Write(output)
	combined.WriteString("\n--- I-802 retry boundary (above: first attempt, lock-shaped; below: retry) ---\n")
	combined.Write(output2)
	return lockAwareRetryResult{Output: combined.Bytes(), ExitCode: exitCode2, RunErr: runErr2, Retried: true}
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

// preflightEvidenceWrite verifies the configured evidence backend can
// accept a 1-byte write before the test command runs. Returns 0 on
// success (proceed with test); non-zero on failure (caller should
// return that exit code).
//
// Why hard-fail rather than warn: the prior best-effort behavior
// silently dropped audit logs whenever the AWS profile was misconfigured,
// and the warning scrolled past in pages of test output. A 200-ms probe
// surfaces the misconfiguration up-front with a clear runbook.
func preflightEvidenceWrite(cfg *config.Config, opts TestRecordOpts) int {
	backend := opts.Backend
	if backend == nil {
		var err error
		backend, err = evidence.New(evidenceConfigFromCfg(cfg))
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"evidence preflight FAILED: backend init error: %v\n"+
					"  set AS_SKIP_EVIDENCE_PREFLIGHT=1 to record without uploading\n",
				err)
			return 2
		}
	}

	// Unique key per probe so concurrent invocations from different
	// agents don't race. Use unix-nano + pid which is sufficient
	// uniqueness without pulling in a uuid dependency.
	probeKey := fmt.Sprintf("__health/preflight-%d-%d.txt", time.Now().UnixNano(), os.Getpid())
	uri, err := backend.Upload(probeKey, strings.NewReader("ok"))
	if err != nil {
		backendName := "local"
		if cfg.Evidence != nil && cfg.Evidence.Backend != "" {
			backendName = cfg.Evidence.Backend
		}
		fmt.Fprintf(os.Stderr,
			"evidence write preflight FAILED:\n"+
				"  backend: %s\n"+
				"  error:   %v\n"+
				"  runbook: source theraprac-infra/scripts/agent-aws-auth.sh --name %s\n"+
				"           (or fix the configured S3 profile / set AS_SKIP_EVIDENCE_PREFLIGHT=1 to record without uploading)\n",
			backendName, err, cfg.AgentID())
		return 2
	}
	// Best-effort cleanup so health probes don't accumulate. Don't
	// fail the run on cleanup errors — the probe upload IS the
	// success signal.
	if err := backend.Delete(probeKey); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: preflight probe at %s could not be deleted: %v)\n", uri, err)
	}
	return 0
}
