package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// TestBaseline records the set of Go test names that fail on the main branch
// at a given commit SHA. Used by `st test --run` to distinguish pre-existing
// failures from regressions introduced by the feature branch. I-1474.
type TestBaseline struct {
	Suite        string   `json:"suite"`
	RecordedAt   string   `json:"recorded_at"`
	SHA          string   `json:"sha"`
	FailingTests []string `json:"failing_tests"`
}

func baselinePath(cfg *config.Config, suite string) string {
	return filepath.Join(cfg.Root(), ".as", "baselines", suite, "baseline.json")
}

// LoadBaseline reads the baseline for suite. Returns nil, nil if no baseline exists.
func LoadBaseline(cfg *config.Config, suite string) (*TestBaseline, error) {
	data, err := os.ReadFile(baselinePath(cfg, suite))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b TestBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// SaveBaseline writes the baseline for b.Suite atomically (write-then-rename).
func SaveBaseline(cfg *config.Config, b *TestBaseline) error {
	path := baselinePath(cfg, b.Suite)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseFailingTests extracts test names from Go test output.
// Matches lines of the form "--- FAIL: TestName (0.00s)".
// Returns nil when no such lines are present (non-Go suite or clean output).
func parseFailingTests(output []byte) []string {
	const prefix = "--- FAIL: "
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		// rest is "TestName (0.00s)" — extract the name before the first space
		if idx := strings.Index(rest, " "); idx >= 0 {
			name := rest[:idx]
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return names
}

// filterNewFailures partitions current failing tests into (new, preExisting)
// by comparing against the baseline set. Both returned slices are sorted.
func filterNewFailures(current, baselineTests []string) (newFails, preExisting []string) {
	set := make(map[string]bool, len(baselineTests))
	for _, t := range baselineTests {
		set[t] = true
	}
	for _, t := range current {
		if set[t] {
			preExisting = append(preExisting, t)
		} else {
			newFails = append(newFails, t)
		}
	}
	sort.Strings(newFails)
	sort.Strings(preExisting)
	return newFails, preExisting
}

// applyBaselineCheck compares test output against the stored baseline.
// Returns 0 if all failures are pre-existing on main (gate passes with pass
// evidence recorded), or -1 when no baseline exists, the suite is non-Go
// (unparseable), or new failures are present (caller should use the normal
// fail path).
func applyBaselineCheck(cfg *config.Config, s *store.Store, id, suite string, output []byte, sha, logURI, retryTag, envTag string, now time.Time) int {
	b, err := LoadBaseline(cfg, suite)
	if err != nil || b == nil {
		return -1
	}
	current := parseFailingTests(output)
	if current == nil {
		return -1 // non-Go suite or no parseable failures
	}
	newFails, preExisting := filterNewFailures(current, b.FailingTests)
	if len(newFails) > 0 {
		fmt.Printf("FAIL %s: %d new failure(s) not in baseline (main @ %s):\n", suite, len(newFails), b.SHA)
		for _, f := range newFails {
			fmt.Printf("  NEW: %s\n", f)
		}
		if len(preExisting) > 0 {
			fmt.Printf("  %d pre-existing (on main @ %s): %s\n", len(preExisting), b.SHA, strings.Join(preExisting, ", "))
		}
		return -1 // new failures — fall through to existing fail path
	}
	// All failures are pre-existing — treat as gate pass.
	fmt.Printf("PASS %s (all %d failure(s) pre-existing on main @ %s)\n", suite, len(preExisting), b.SHA)
	ev := fmt.Sprintf("pass%s%s baseline:pre-existing/%d %s %s evidence:%s",
		retryTag, envTag, len(preExisting), sha, now.Format(time.RFC3339), logURI)
	_ = s.Mutate(id, func(it *model.Item) error {
		it.SetNested("testing_evidence", suite, ev)
		it.Doc.SetField("last_touched", now.Format(time.RFC3339))
		return nil
	})
	changelog.Append(cfg, id, changelog.Entry{
		Op: "test_passed", Field: "testing_evidence." + suite, NewValue: ev,
	})
	autoSync(s, fmt.Sprintf("st test pass: %s %s (baseline pre-existing)", id, suite)) //nolint:errcheck
	return 0
}

// TestBaselineRefresh runs suite on the main checkout (no worktree rewrite),
// records failing test names, and saves the baseline to .as/baselines/<suite>/baseline.json.
func TestBaselineRefresh(cfg *config.Config, suite string) int {
	if cfg.Testing == nil {
		fmt.Fprintln(os.Stderr, "no testing configuration found")
		return 1
	}
	suiteCmd := ""
	if sc, ok := cfg.Testing.RequiredSuites[suite]; ok {
		suiteCmd = sc.Command
	} else if sc, ok := cfg.Testing.ScopeSuites[suite]; ok {
		suiteCmd = sc.Command
	}
	if suiteCmd == "" {
		fmt.Fprintf(os.Stderr, "suite %q not found in config.testing\n", suite)
		return 1
	}

	fmt.Printf("[baseline] Running %s on main checkout: %s\n", suite, suiteCmd)
	// Run WITHOUT worktree rewrite — cd ../theraprac-X resolves to main checkout.
	output, exitCode, runErr := runCmdInDirStreaming(cfg.Root(), suiteCmd)
	if runErr != nil && exitCode == 0 {
		fmt.Fprintf(os.Stderr, "failed to execute %s: %v\n", suite, runErr)
		return 1
	}
	_ = exitCode // non-zero is expected when failures exist

	failing := parseFailingTests(output)

	// Determine SHA from the first repo associated with this suite.
	sha := "unknown"
	if repo := autoScopeRepo(suite); repo != "" {
		repoDir := resolveRepoDir(cfg, repo)
		if repoDir != "" {
			if s, err := runGit(repoDir, "rev-parse", "--short", "HEAD"); err == nil {
				sha = strings.TrimSpace(s)
			}
		}
	}

	b := &TestBaseline{
		Suite:        suite,
		RecordedAt:   time.Now().Format(time.RFC3339),
		SHA:          sha,
		FailingTests: failing,
	}
	if err := SaveBaseline(cfg, b); err != nil {
		fmt.Fprintf(os.Stderr, "saving baseline for %s: %v\n", suite, err)
		return 1
	}

	fmt.Printf("[baseline] %s: %d failing test(s) on main (sha=%s)\n", suite, len(failing), sha)
	return 0
}
