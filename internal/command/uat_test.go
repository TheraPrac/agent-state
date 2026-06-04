package command

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

func setupUATTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupPRTestEnv(t) // active T-003, testing config

	// Give T-003 test evidence and manifest
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "api_unit", "pass abc123 2026-03-27T10:00:00-06:00 evidence:s3://bucket/log.txt")
		it.SetNested("testing_evidence", "api_lint", "pass abc123 2026-03-27T10:00:00-06:00")
		it.SetNested("manifest", "prs", "api#42")
		it.SetNested("delivery", "stage", "pr_open")
		it.AcceptanceCriteria = []string{
			"API unit tests pass",
			"PR manifest recorded",
			"cmd:echo hello",
		}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	return s, cfg
}

func TestUATBasicReport(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte("hello\n"), 0, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if code != 0 {
		t.Fatalf("UAT returned %d, want 0\noutput: %s", code, output)
	}

	// Should contain report sections
	if !strings.Contains(output, "UAT Report") {
		t.Error("missing UAT Report header")
	}
	if !strings.Contains(output, "AUTOMATED CHECKS") {
		t.Error("missing AUTOMATED CHECKS section")
	}
	if !strings.Contains(output, "ACCEPTANCE CRITERIA") {
		t.Error("missing ACCEPTANCE CRITERIA section")
	}
	if !strings.Contains(output, "SUMMARY") {
		t.Error("missing SUMMARY section")
	}
}

func TestUATAutoChecksPass(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d, want 0 (all auto checks should pass)", code)
	}
}

func TestUATMissingEvidence(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	// Remove test evidence
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "api_unit", "null")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("UAT returned %d, want 1 (missing evidence should fail)", code)
	}
}

func TestUATCmdCriterionPass(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			if strings.Contains(cmd, "echo hello") {
				return []byte("hello\n"), 0, nil
			}
			return nil, 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d", code)
	}
}

func TestUATCmdCriterionFail(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	// Change cmd criterion to something that fails
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.AcceptanceCriteria = []string{"cmd:exit 1"}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd: func(cmd string) ([]byte, int, error) {
			return []byte("failed\n"), 1, nil
		},
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	code := UAT(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("UAT returned %d, want 1 (cmd failed)", code)
	}
}

func TestUATManualCriteria(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.AcceptanceCriteria = []string{"User can see the modal"}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return nil, 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// Prose ACs without cmd: prefix are pending (manual review), not auto-fail.
	// They don't block the pipeline — they show as warnings.
	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d, want 0 (manual ACs are pending, not blocking)", code)
	}
}

func TestUATItemNotFound(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	opts := UATOpts{Backend: &evidence.LocalBackend{Dir: t.TempDir()}}
	code := UAT(s, cfg, "T-999", opts)
	if code != 1 {
		t.Errorf("returned %d, want 1", code)
	}
}

func TestUATNoAcceptanceCriteria(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.AcceptanceCriteria = nil
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return nil, 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	// No ACs — cross-cutting checks still run
	code := UAT(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("UAT returned %d", code)
	}
}

// Ensure imports used
var _ config.Config
var _ evidence.LocalBackend

func TestValidateACsyntax(t *testing.T) {
	// Valid commands
	valid := []string{
		"- cmd: grep -q 'foo' file.txt",
		"- cmd: go test ./internal/handlers/ -run TestClaimsAging -v -count=1",
		"- cmd: cd ../theraprac-api && go test ./internal/handlers/ -run TestFoo -v -count=1",
		"- cmd: test -f file.go",
	}
	errors := ValidateACsyntax(valid)
	if len(errors) != 0 {
		t.Errorf("expected 0 errors for valid ACs, got %d: %v", len(errors), errors)
	}

	// Invalid commands — unmatched quotes
	invalid := []string{
		"- cmd: grep -q 'foo file.txt",                                                  // unmatched '
		"- cmd: awk '/pattern/,/^}/' file | grep -q 'text",                              // unmatched ' at end
		"- cmd: echo ok",                                                                  // valid
		"- cmd: ! grep -q 'haproxy ../theraprac-infra/ansible/nat-prod/playbook.yml",    // unmatched '
	}
	errors = ValidateACsyntax(invalid)
	if len(errors) != 3 {
		t.Errorf("expected 3 syntax errors, got %d: %v", len(errors), errors)
	}

	// Empty command
	empty := []string{"- cmd: "}
	errors = ValidateACsyntax(empty)
	if len(errors) != 1 {
		t.Errorf("expected 1 error for empty command, got %d", len(errors))
	}
}

// TestUATScopeSuiteSkipped verifies I-540: scope suites marked
// `skip: <reason>` via `st test --skip` render as informational
// ⊘ skipped, not ✗ auto-fail, and the UAT exit code stays 0.
func TestUATScopeSuiteSkipped(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "web_e2e", "skip: workspace-only change")
		it.SetNested("testing_evidence", "api_integration", "skip: infra-only change")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("hello\n"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if code != 0 {
		t.Fatalf("UAT returned %d, want 0 (skipped scope suites must not fail)\noutput: %s", code, output)
	}
	if !strings.Contains(output, "skipped") {
		t.Errorf("expected SUMMARY to mention 'skipped', got:\n%s", output)
	}
	if !strings.Contains(output, "skip: workspace-only change") {
		t.Errorf("expected skip reason rendered, got:\n%s", output)
	}
	// The auto-fail counter must NOT be inflated by the two skipped rows.
	if !strings.Contains(output, "0 auto-fail") {
		t.Errorf("expected 0 auto-fail in summary, got:\n%s", output)
	}
}

// TestUATScopeSuiteSkippedSummaryCount pins the new summary counter so a
// future refactor can't quietly drop "N skipped" from the SUMMARY line.
func TestUATScopeSuiteSkippedSummaryCount(t *testing.T) {
	s, cfg := setupUATTestEnv(t)
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("testing_evidence", "web_e2e", "skip: workspace-only change")
		it.SetNested("testing_evidence", "api_integration", "skip: infra-only change")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("hello\n"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "2 skipped") {
		t.Errorf("expected '2 skipped' in summary, got:\n%s", output)
	}
	if !strings.Contains(output, "0 auto-fail") {
		t.Errorf("expected '0 auto-fail' in summary, got:\n%s", output)
	}
}

// I-776: workspace-config items show the class's required suite in the
// AUTOMATED CHECKS block, NOT the default api/web Tier 1 — UAT must agree
// with the gate, which iterates the class-scoped set.
func TestUATScopeClassChecksClassSuites(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

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
		it.SetNested("testing_evidence", "workspace_test", "pass abc123 2026-05-23T08:00:00-06:00")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "workspace_test") {
		t.Errorf("expected workspace_test in AUTOMATED CHECKS for workspace-config item:\n%s", output)
	}
	// Default api/web suites must NOT appear — they're not required for this item.
	for _, defaultSuite := range []string{"api_unit", "api_lint", "web_typecheck", "web_unit"} {
		if strings.Contains(output, defaultSuite) {
			t.Errorf("expected NO %s in AUTOMATED CHECKS for workspace-config item:\n%s", defaultSuite, output)
		}
	}
}

// I-776: scope-suite policy is skipped for class items in UAT, same as the
// gate. A stale `web_e2e: required` marker on a workspace-config item must
// not surface as a UAT failure.
func TestUATScopeClassSkipsScopeSuiteRequired(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

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
		it.SetNested("testing_evidence", "workspace_test", "pass abc123 2026-05-23T08:00:00-06:00")
		// Stale marker — must be ignored.
		it.SetNested("testing_evidence", "web_e2e", "required")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	opts := UATOpts{
		RunCmd:  func(cmd string) ([]byte, int, error) { return []byte("ok"), 0, nil },
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if strings.Contains(output, "web_e2e") {
		t.Errorf("class items should not surface scope_suites markers in UAT:\n%s", output)
	}
}

// TestUATCmdTimeout verifies that a cmd: AC that runs longer than the timeout
// fails with a clear timeout message rather than hanging indefinitely.
func TestUATCmdTimeout(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.AcceptanceCriteria = []string{"cmd: sleep 300"}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// Inject a 1-second timeout runCmd to keep the test fast.
	timeoutRunCmd := func(cmd string) ([]byte, int, error) {
		return runCmdInDirWithTimeout("", cmd, 1*time.Second)
	}

	opts := UATOpts{
		RunCmd:  timeoutRunCmd,
		Backend: &evidence.LocalBackend{Dir: t.TempDir()},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	exit := UAT(s, cfg, "T-003", opts)
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if exit == 0 {
		t.Errorf("expected non-zero exit for timed-out cmd: AC, got 0")
	}
	if !strings.Contains(output, "timeout") {
		t.Errorf("expected 'timeout' in UAT output, got:\n%s", output)
	}
}

// TestValidateACsyntaxAntiPattern verifies that ValidateACsyntax rejects
// cmd: ACs that use `st test --run` (re-runs full suites during UAT).
func TestValidateACsyntaxAntiPattern(t *testing.T) {
	badACs := []string{
		"- cmd: st test T-216 api_unit --run",
		"- cmd: st test T-216 api_lint --run",
		"- cmd: st test T-100 web_typecheck --run",
	}
	errs := ValidateACsyntax(badACs)
	if len(errs) != 3 {
		t.Errorf("expected 3 anti-pattern errors, got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e, "anti-pattern") {
			t.Errorf("expected 'anti-pattern' in error, got: %s", e)
		}
	}

	// These are fine: targeted go test commands, not suite re-runs.
	goodACs := []string{
		"- cmd: go test ./internal/handlers/ -run TestAgingBucketCalculation -v -count=1",
		"- cmd: st test T-216 api_unit --skip 'no changes'",
		"- cmd: st stack",
	}
	errs = ValidateACsyntax(goodACs)
	if len(errs) != 0 {
		t.Errorf("expected 0 errors for good ACs, got %d: %v", len(errs), errs)
	}
}

// TestValidateACsyntaxFullSuiteAntiPattern verifies that ValidateACsyntax rejects
// bare go test, make test-*, and npm run test suite runs (I-1119).
func TestValidateACsyntaxFullSuiteAntiPattern(t *testing.T) {
	blocked := []string{
		"- cmd: go test ./...",
		"- cmd: go test ./internal/db/...",
		"- cmd: make test-unit",
		"- cmd: cd ../theraprac-api && make test-api-lint",
		"- cmd: npm run test",
		"- cmd: npm run test -- --verbose",
	}
	errs := ValidateACsyntax(blocked)
	if len(errs) != len(blocked) {
		t.Errorf("expected %d anti-pattern errors, got %d: %v", len(blocked), len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e, "anti-pattern") {
			t.Errorf("expected 'anti-pattern' in error, got: %s", e)
		}
	}

	// These are fine: targeted invocations, not full-suite runs.
	allowed := []string{
		"- cmd: go test ./internal/handlers/ -run TestAgingBucketCalculation -v -count=1",
		"- cmd: go test -run TestFoo ./internal/db/...",
		"- cmd: go test -run=TestFoo ./internal/db/...",
		`- cmd: GOFLAGS="-run=TestClaimsAging" go test ./internal/db/...`,
		"- cmd: npm run test -- --testPathPattern=AgingReport",
		"- cmd: npm run test:unit",
		"- cmd: grep -q 'handler' ./internal/handlers/api_aging.go",
	}
	errs = ValidateACsyntax(allowed)
	if len(errs) != 0 {
		t.Errorf("expected 0 errors for allowed ACs, got %d: %v", len(errs), errs)
	}
}

// TestCleanACs verifies the st uat --clean-acs conversion job (I-1120).
func TestCleanACs(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// Seed two items: one with suite-run ACs (T-001) and one with clean ACs (T-003).
	// Use Doc.ReplaceList so ACs persist to disk (Mutate serializes via Doc.String()).
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "queued"
		acs := []string{"cmd: go test ./...", "cmd: make test-unit", "cmd: grep -q 'foo' file.go"}
		it.AcceptanceCriteria = acs
		rawACs := []string{"- cmd: go test ./...", "- cmd: make test-unit", "- cmd: grep -q 'foo' file.go"}
		it.Doc.ReplaceList("acceptance_criteria", rawACs)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}
	if err := s.Mutate("T-003", func(it *model.Item) error {
		acs := []string{"cmd: go test ./internal/handlers/ -run TestFoo -v -count=1", "cmd: grep -q 'handler' ./file.go"}
		it.AcceptanceCriteria = acs
		rawACs := []string{"- cmd: go test ./internal/handlers/ -run TestFoo -v -count=1", "- cmd: grep -q 'handler' ./file.go"}
		it.Doc.ReplaceList("acceptance_criteria", rawACs)
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// Dry run: should print summary but not modify T-001.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := CleanACs(s, cfg, CleanACsOpts{Apply: false})
	w.Close()
	os.Stdout = old
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if code != 0 {
		t.Fatalf("dry run returned %d, want 0\noutput: %s", code, out)
	}
	if !strings.Contains(out, "T-001") {
		t.Errorf("expected T-001 in dry-run output, got:\n%s", out)
	}
	if !strings.Contains(out, "Dry run") {
		t.Errorf("expected 'Dry run' in output, got:\n%s", out)
	}
	// Verify T-001 was NOT modified.
	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found")
	}
	if len(item.AcceptanceCriteria) != 3 {
		t.Errorf("dry run should not modify ACs, got %d ACs", len(item.AcceptanceCriteria))
	}

	// Apply mode: should remove suite-run ACs from T-001, leave T-003 untouched.
	code = CleanACs(s, cfg, CleanACsOpts{Apply: true})
	if code != 0 {
		t.Fatalf("apply returned %d, want 0", code)
	}
	item, ok = s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after apply")
	}
	if len(item.AcceptanceCriteria) != 1 {
		t.Errorf("expected 1 AC remaining (grep), got %d: %v", len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	if !strings.Contains(item.AcceptanceCriteria[0], "grep") {
		t.Errorf("expected grep AC to survive, got: %s", item.AcceptanceCriteria[0])
	}
	// T-003 (clean ACs) should be untouched.
	item3, ok := s.Get("T-003")
	if !ok {
		t.Fatal("T-003 not found")
	}
	if len(item3.AcceptanceCriteria) != 2 {
		t.Errorf("T-003 ACs should be untouched, got %d", len(item3.AcceptanceCriteria))
	}

	// --item filter: only scan T-001; T-003 ignored.
	// Reset T-001 to test --item.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.AcceptanceCriteria = []string{"cmd: npm run test", "cmd: test -f output.go"}
		it.Doc.ReplaceList("acceptance_criteria", []string{"- cmd: npm run test", "- cmd: test -f output.go"})
		return nil
	}); err != nil {
		t.Fatalf("reset T-001: %v", err)
	}
	code = CleanACs(s, cfg, CleanACsOpts{Apply: true, Item: "T-001"})
	if code != 0 {
		t.Fatalf("apply --item returned %d, want 0", code)
	}
	item, _ = s.Get("T-001")
	if len(item.AcceptanceCriteria) != 1 || !strings.Contains(item.AcceptanceCriteria[0], "test -f") {
		t.Errorf("expected only 'cmd: test -f' AC remaining, got: %v", item.AcceptanceCriteria)
	}

	// --item with unknown ID should return exit 1.
	code = CleanACs(s, cfg, CleanACsOpts{Item: "NONEXISTENT"})
	if code != 1 {
		t.Errorf("expected exit 1 for unknown item, got %d", code)
	}

	// --item with terminal-status item should return exit 1.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "done"
		it.Doc.SetField("status", "done")
		return nil
	}); err != nil {
		t.Fatalf("set T-001 done: %v", err)
	}
	code = CleanACs(s, cfg, CleanACsOpts{Item: "T-001"})
	if code != 1 {
		t.Errorf("expected exit 1 for terminal-status item, got %d", code)
	}
}

// I-831: when a default-class item carries goal tags that map to a scope class,
// checkTestSuites prepends a failing hint row with an actionable fix command.
func TestUATHintsScopeClassForGoalTaggedItem(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	// Inject workspace-config scope class with applies_to_goals: [st-tooling].
	if cfg.Testing.ScopeClasses == nil {
		cfg.Testing.ScopeClasses = make(map[string]config.ScopeClassConfig)
	}
	cfg.Testing.ScopeClasses["workspace-config"] = config.ScopeClassConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"workspace_test": {Command: "bash run.sh"},
		},
		AppliesToGoals: []string{"st-tooling"},
	}

	// Create a default-class item with goal:st-tooling tag and no evidence.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = ""
		it.Tags = []string{"goal:st-tooling"}
		it.TestingEvidence = map[string]interface{}{}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	item, ok := s.Get("T-003")
	if !ok {
		t.Fatal("T-003 not found")
	}

	results := checkTestSuites(item, cfg)
	if len(results) == 0 {
		t.Fatal("expected at least one check result")
	}
	hint := results[0]
	if hint.Label != "scope_class" {
		t.Errorf("first result label = %q, want scope_class", hint.Label)
	}
	if hint.Passed {
		t.Error("hint check should be failing (Passed=false)")
	}
	if !strings.Contains(hint.Detail, "workspace-config") {
		t.Errorf("hint detail should mention workspace-config, got: %s", hint.Detail)
	}
	if !strings.Contains(hint.Detail, "st update") {
		t.Errorf("hint detail should mention st update, got: %s", hint.Detail)
	}
}

// I-831: when a default-class item has no goal tags matching any scope class,
// no hint row is prepended.
func TestUATNoHintForUntaggedItem(t *testing.T) {
	s, cfg := setupUATTestEnv(t)

	if cfg.Testing.ScopeClasses == nil {
		cfg.Testing.ScopeClasses = make(map[string]config.ScopeClassConfig)
	}
	cfg.Testing.ScopeClasses["workspace-config"] = config.ScopeClassConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"workspace_test": {Command: "bash run.sh"},
		},
		AppliesToGoals: []string{"st-tooling"},
	}

	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.ScopeClass = ""
		it.Tags = []string{"some-unrelated-tag"}
		it.TestingEvidence = map[string]interface{}{}
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	item, ok := s.Get("T-003")
	if !ok {
		t.Fatal("T-003 not found")
	}

	results := checkTestSuites(item, cfg)
	for _, r := range results {
		if r.Label == "scope_class" && strings.Contains(r.Detail, "goal tags suggest") {
			t.Errorf("unexpected hint row for untagged item: %+v", r)
		}
	}
}
