package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// --- autoScopeRepo ---

func TestAutoScopeRepo(t *testing.T) {
	tests := []struct {
		suite string
		want  string
	}{
		{"api_unit", "theraprac-api"},
		{"api_lint", "theraprac-api"},
		{"api_integration", "theraprac-api"},
		{"web_typecheck", "theraprac-web"},
		{"web_unit", "theraprac-web"},
		{"web_integration", "theraprac-web"},
		{"web_e2e", "theraprac-web"},
		{"infra_validate", "theraprac-infra"},
		{"workspace_test", ""}, // I-1597: workspace_* unmapped (hooks suite, not the as repo)
		{"as_unit", "as"},
		{"live_acceptance", ""}, // no prefix match
		{"unknown_suite", ""},
	}
	for _, tt := range tests {
		got := autoScopeRepo(tt.suite)
		if got != tt.want {
			t.Errorf("autoScopeRepo(%q) = %q, want %q", tt.suite, got, tt.want)
		}
	}
}

// --- autoGlobMatch ---

func TestAutoGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Simple patterns (no **)
		{"*.go", "foo.go", true},
		{"*.go", "foo.ts", false},
		{"internal/foo.go", "internal/foo.go", true},
		// ** prefix pattern: "src/app/**" matches anything under src/app/
		{"src/app/**", "src/app/page.tsx", true},
		{"src/app/**", "src/app/dashboard/page.tsx", true},
		{"src/app/**", "src/components/Button.tsx", false},
		// ** prefix with subdirectory
		{"src/lib/hooks/**", "src/lib/hooks/useFoo.ts", true},
		{"src/lib/hooks/**", "src/lib/store/useFoo.ts", false},
		// ** suffix pattern: "**/*.go"
		{"**/*.go", "internal/foo.go", true},
		{"**/*.go", "cmd/as/main.go", true},
		{"**/*.go", "internal/foo.ts", false},
		// Pure ** matches everything
		{"**", "anything/at/all.ts", true},
		// Multi-segment suffix: src/**/hooks/useFoo.ts should NOT match src/other-hooks/useFoo.ts
		{"src/**/hooks/useFoo.ts", "src/lib/hooks/useFoo.ts", true},
		{"src/**/hooks/useFoo.ts", "src/other-hooks/useFoo.ts", false},
	}
	for _, tt := range tests {
		got := autoGlobMatch(tt.pattern, tt.name)
		if got != tt.want {
			t.Errorf("autoGlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// --- autoMatchTriggers ---

func TestAutoMatchTriggers(t *testing.T) {
	files := []string{
		"src/app/page.tsx",
		"src/app/dashboard/page.tsx",
		"src/lib/api/client.ts",
	}
	triggers := []string{"src/app/**", "src/components/**"}

	if !autoMatchTriggers(files, triggers) {
		t.Error("expected match for src/app/** but got false")
	}

	noMatch := autoMatchTriggers([]string{"src/lib/api/client.ts"}, triggers)
	if noMatch {
		t.Error("expected no match for src/lib/api/client.ts against src/app/** and src/components/**")
	}
}

// --- selectAutoSuites ---

func makeSuiteConfig(command string) config.SuiteConfig {
	return config.SuiteConfig{Command: command}
}

func makeScopeSuiteConfig(command string, triggers ...string) config.ScopeSuiteConfig {
	return config.ScopeSuiteConfig{Command: command, Triggers: triggers}
}

func TestSelectAutoSuites_ApiOnly(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": makeSuiteConfig("make test-unit"),
			"api_lint": makeSuiteConfig("make lint"),
			"web_typecheck": makeSuiteConfig("make type-check"),
			"web_unit": makeSuiteConfig("make test-unit"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": makeScopeSuiteConfig("make integration-local"),
			"web_integration": makeScopeSuiteConfig("make test-integration"),
			"live_acceptance": makeScopeSuiteConfig("true"),
		},
	}

	item := &model.Item{}
	touched := map[string][]string{
		"theraprac-api": {"internal/billing/client.go", "internal/billing/client_test.go"},
	}

	tier1, tier2 := selectAutoSuites(cfg, item, touched)

	// Tier 1: only api suites (web suites filtered out)
	for _, s := range tier1 {
		if s == "web_typecheck" || s == "web_unit" {
			t.Errorf("tier1 should not include web suite %q when only api changed", s)
		}
	}
	wantTier1 := map[string]bool{"api_unit": false, "api_lint": false}
	for _, s := range tier1 {
		wantTier1[s] = true
	}
	for suite, found := range wantTier1 {
		if !found {
			t.Errorf("tier1 missing expected suite %q", suite)
		}
	}

	// Tier 2: api_integration expected, web_integration not, live_acceptance never
	wantTier2 := map[string]bool{"api_integration": false}
	for _, s := range tier2 {
		if s == "web_integration" || s == "live_acceptance" {
			t.Errorf("tier2 should not include %q when only api changed", s)
		}
		wantTier2[s] = true
	}
	for suite, found := range wantTier2 {
		if !found {
			t.Errorf("tier2 missing expected suite %q", suite)
		}
	}
}

func TestSelectAutoSuites_WebOnlyNoE2ETrigger(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      makeSuiteConfig("make test-unit"),
			"api_lint":      makeSuiteConfig("make lint"),
			"web_typecheck": makeSuiteConfig("make type-check"),
			"web_unit":      makeSuiteConfig("make test-unit"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_integration": makeScopeSuiteConfig("make test-integration"),
			"web_e2e":         makeScopeSuiteConfig("scripts/e2e-local.sh run", "src/app/**", "src/components/**"),
			"live_acceptance": makeScopeSuiteConfig("true"),
		},
	}

	item := &model.Item{}
	// Change only touches non-trigger paths — web_e2e should NOT fire
	touched := map[string][]string{
		"theraprac-web": {"src/lib/api/client.ts", "src/lib/utils/format.ts"},
	}

	tier1, tier2 := selectAutoSuites(cfg, item, touched)

	for _, s := range tier1 {
		if s == "api_unit" || s == "api_lint" {
			t.Errorf("tier1 should not include api suite %q when only web changed", s)
		}
	}

	for _, s := range tier2 {
		if s == "web_e2e" {
			t.Errorf("web_e2e should not fire when no files match its triggers")
		}
		if s == "live_acceptance" {
			t.Errorf("live_acceptance should never be auto-selected")
		}
	}

	hasWebIntegration := false
	for _, s := range tier2 {
		if s == "web_integration" {
			hasWebIntegration = true
		}
	}
	if !hasWebIntegration {
		t.Error("tier2 should include web_integration for web-only changes")
	}
}

func TestSelectAutoSuites_WebE2ETriggerFires(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"web_typecheck": makeSuiteConfig("make type-check"),
			"web_unit":      makeSuiteConfig("make test-unit"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_e2e": makeScopeSuiteConfig("scripts/e2e-local.sh run", "src/app/**", "src/components/**"),
		},
	}

	item := &model.Item{}
	touched := map[string][]string{
		"theraprac-web": {"src/app/patients/page.tsx"},
	}

	_, tier2 := selectAutoSuites(cfg, item, touched)

	hasE2E := false
	for _, s := range tier2 {
		if s == "web_e2e" {
			hasE2E = true
		}
	}
	if !hasE2E {
		t.Error("web_e2e should fire when a file matches its triggers")
	}
}

// I-1597 (Inv 5): class-required suites are impact-scoped, not run-regardless.
// A class suite whose mapped repo is touched runs; a mapped+untouched suite is
// omitted (autoRecordSkips records it auto-skip); an unmapped suite always runs.
func TestSelectAutoSuites_ScopeClassImpactScoped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      makeSuiteConfig("make test-unit"),
			"web_typecheck": makeSuiteConfig("make type-check"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"agent-state": {
				RequiredSuites: map[string]config.SuiteConfig{
					// as_test → prefix "as" → repo "as" (mapped, impact-scopable).
					"as_test": makeSuiteConfig("cd ../as && go test ./..."),
					// hook_test → prefix "hook" → unmapped (autoScopeRepo "").
					"hook_test": makeSuiteConfig("bash run-changed-hook-tests.sh"),
				},
			},
		},
	}

	item := &model.Item{ScopeClass: "agent-state"}

	// Case 1: as repo touched → as_test runs; hook_test (unmapped) always runs.
	tier1, tier2 := selectAutoSuites(cfg, item, map[string][]string{
		"as": {"internal/command/test_auto.go"},
	})
	if len(tier2) != 0 {
		t.Errorf("scope suites should be empty for class items, got %v", tier2)
	}
	if len(tier1) != 2 || tier1[0] != "as_test" || tier1[1] != "hook_test" {
		t.Errorf("as touched: tier1 = %v, want [as_test hook_test]", tier1)
	}

	// Case 2: as repo NOT touched → as_test omitted (auto-skipped elsewhere);
	// hook_test still runs because it can't be impact-scoped.
	tier1, _ = selectAutoSuites(cfg, item, map[string][]string{
		"theraprac-api": {"internal/billing/client.go"},
	})
	if len(tier1) != 1 || tier1[0] != "hook_test" {
		t.Errorf("as untouched: tier1 = %v, want [hook_test] only", tier1)
	}
}

func TestSelectAutoSuites_NoChanges(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": makeSuiteConfig("make test-unit"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{},
	}

	item := &model.Item{}
	tier1, tier2 := selectAutoSuites(cfg, item, map[string][]string{})

	if len(tier1)+len(tier2) != 0 {
		t.Errorf("expected no suites for empty touched map, got tier1=%v tier2=%v", tier1, tier2)
	}
}

// TestAutoGlobMatch_MultiStarFalsePositive ensures the HasSuffix fallback does
// not false-positive on a path that only shares a suffix segment with the pattern.
func TestAutoGlobMatch_MultiStarFalsePositive(t *testing.T) {
	// "src/other-hooks/useFoo.ts" should NOT match "src/**/hooks/useFoo.ts"
	if autoGlobMatch("src/**/hooks/useFoo.ts", "src/other-hooks/useFoo.ts") {
		t.Error("false positive: src/other-hooks/useFoo.ts should not match src/**/hooks/useFoo.ts")
	}
	// But "src/lib/hooks/useFoo.ts" SHOULD match
	if !autoGlobMatch("src/**/hooks/useFoo.ts", "src/lib/hooks/useFoo.ts") {
		t.Error("false negative: src/lib/hooks/useFoo.ts should match src/**/hooks/useFoo.ts")
	}
}

// --- autoRecordSkips scope-suite coverage (I-1465) ---

func makeAutoRecordSkipsConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"as_unit":         {Command: "go test ./..."},
			"live_acceptance": {Command: "true"},
		},
	}
	return cfg
}

// Scope suite for an unchanged repo must be auto-skipped so UAT doesn't block.
func TestAutoRecordSkips_SkipsScopeSuiteForUnchangedRepo(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Testing = makeAutoRecordSkipsConfig().Testing

	// as repo has no changes — as_unit should be auto-skipped.
	touched := map[string][]string{
		"theraprac-api": {"internal/billing/client.go"},
	}

	if code := autoRecordSkips(s, cfg, "T-001", &model.Item{}, touched); code != 0 {
		t.Fatalf("autoRecordSkips returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	ev, ok := getNestedField(item, "testing_evidence", "as_unit")
	if !ok || !strings.HasPrefix(ev, "auto-skip: no files changed in as") {
		t.Errorf("as_unit testing_evidence = %q, want auto-skip prefix", ev)
	}
	// live_acceptance must never be auto-skipped.
	if _, ok := getNestedField(item, "testing_evidence", "live_acceptance"); ok {
		t.Error("live_acceptance must not be auto-skipped")
	}
}

// Scope suite for a changed repo must NOT be auto-skipped — it should run.
func TestAutoRecordSkips_DoesNotSkipScopeSuiteForChangedRepo(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Testing = makeAutoRecordSkipsConfig().Testing

	// as repo changed — as_unit must run, not be skipped.
	touched := map[string][]string{
		"as": {"internal/command/test_auto.go"},
	}

	if code := autoRecordSkips(s, cfg, "T-001", &model.Item{}, touched); code != 0 {
		t.Fatalf("autoRecordSkips returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if _, ok := getNestedField(item, "testing_evidence", "as_unit"); ok {
		t.Error("as_unit must not be auto-skipped when as repo has changes")
	}
	// api_unit (required, api unchanged) must still be skipped.
	ev, ok := getNestedField(item, "testing_evidence", "api_unit")
	if !ok || !strings.HasPrefix(ev, "auto-skip:") {
		t.Errorf("api_unit testing_evidence = %q, want auto-skip (required, api unchanged)", ev)
	}
}

// I-1597 regression: workspace_test (workspace-config class) verifies
// claude-config hooks, not the `as` repo. It must NOT be auto-skipped or
// impact-scoped to `as` — otherwise a workspace-config item whose hooks changed
// (but `as` did not) would auto-skip its only suite and pass the gate green with
// zero suites run. autoScopeRepo leaves `workspace_*` unmapped so it always runs.
func TestWorkspaceTest_NotAutoSkipped_AlwaysRuns(t *testing.T) {
	if r := autoScopeRepo("workspace_test"); r != "" {
		t.Fatalf("autoScopeRepo(workspace_test) = %q, want \"\" (unmapped, self-scoping)", r)
	}

	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{},
		ScopeSuites:    map[string]config.ScopeSuiteConfig{},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {Command: "bash claude-config/hooks/run-changed-hook-tests.sh"},
				},
			},
		},
	}
	item := &model.Item{ScopeClass: "workspace-config"}

	// No `as` changes (and theraprac-workspace isn't even a scoped repo) →
	// workspace_test must still be selected to run.
	tier1, _ := selectAutoSuites(cfg, item, map[string][]string{})
	if len(tier1) != 1 || tier1[0] != "workspace_test" {
		t.Errorf("tier1 = %v, want [workspace_test] (always runs)", tier1)
	}

	// And it must NOT be recorded auto-skip.
	s, scfg := setupTestEnvWithChangelog(t)
	scfg.Testing = cfg.Testing
	if code := autoRecordSkips(s, scfg, "T-001", item, map[string][]string{}); code != 0 {
		t.Fatalf("autoRecordSkips returned %d, want 0", code)
	}
	stored, _ := s.Get("T-001")
	if _, ok := getNestedField(stored, "testing_evidence", "workspace_test"); ok {
		t.Error("workspace_test must not be auto-skipped — it always runs")
	}
}

// I-1597: live_acceptance is a manual gate. Even if a class declares it a
// required suite, selectAutoSuites must NOT add it to tier1 (which AutoTest
// would auto-run), and autoRecordSkips must NOT auto-skip it (repo==""). The
// gate then forces an explicit operator record rather than a silent satisfy.
func TestSelectAutoSuites_LiveAcceptanceNeverAutoRun(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{},
		ScopeSuites:    map[string]config.ScopeSuiteConfig{},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"manual-gated": {
				RequiredSuites: map[string]config.SuiteConfig{
					"live_acceptance": {Command: "true"},
					"as_test":         {Command: "go test ./..."},
				},
			},
		},
	}
	item := &model.Item{ScopeClass: "manual-gated"}
	tier1, _ := selectAutoSuites(cfg, item, map[string][]string{"as": {"x.go"}})
	for _, n := range tier1 {
		if n == "live_acceptance" {
			t.Errorf("live_acceptance must never be in tier1 (auto-run); got %v", tier1)
		}
	}
	if len(tier1) != 1 || tier1[0] != "as_test" {
		t.Errorf("tier1 = %v, want [as_test] only", tier1)
	}
}

// I-1597 (Inv 5/6): a class item's mapped+untouched required suite is recorded
// auto-skip; its unmapped required suite (no repo mapping) records nothing and
// runs. Before I-1597, autoRecordSkips returned early for any class item.
func TestAutoRecordSkips_ClassItem_ImpactScoped(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{},
		ScopeSuites:    map[string]config.ScopeSuiteConfig{},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"agent-state": {
				RequiredSuites: map[string]config.SuiteConfig{
					"as_test":   {Command: "cd ../as && go test ./..."},
					"hook_test": {Command: "bash run-changed-hook-tests.sh"},
				},
			},
		},
	}

	item := &model.Item{ScopeClass: "agent-state"}
	// as repo untouched (only a hooks change, which maps to no repo prefix).
	touched := map[string][]string{
		"theraprac-api": {"internal/billing/client.go"},
	}

	if code := autoRecordSkips(s, cfg, "T-001", item, touched); code != 0 {
		t.Fatalf("autoRecordSkips returned %d, want 0", code)
	}

	stored, _ := s.Get("T-001")
	// as_test (mapped, untouched) → auto-skip recorded with its reason.
	ev, ok := getNestedField(stored, "testing_evidence", "as_test")
	if !ok || !strings.HasPrefix(ev, "auto-skip: no files changed in as") {
		t.Errorf("as_test evidence = %q, want auto-skip prefix", ev)
	}
	// hook_test (unmapped) → nothing recorded; it runs instead.
	if _, ok := getNestedField(stored, "testing_evidence", "hook_test"); ok {
		t.Error("hook_test (unmapped) must not be auto-skipped — it always runs")
	}
}

// --- resolveRepoDirForAuto (I-1473) ---

// gitAuto runs git -C dir with args and fatals on error. Local to auto-test
// fixtures.
func gitAuto(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoWithFeatureBranch creates a git repo in dir with one commit on main
// and (when changedFile != "") a feature branch adding that file.
func initRepoWithFeatureBranch(t *testing.T, dir string, changedFile string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	os.MkdirAll(dir, 0755)
	gitAuto(t, dir, "init", "-b", "main")
	gitAuto(t, dir, "config", "user.email", "test@test.com")
	gitAuto(t, dir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0644)
	gitAuto(t, dir, "add", ".")
	gitAuto(t, dir, "commit", "-m", "initial")
	if changedFile != "" {
		gitAuto(t, dir, "checkout", "-b", "feature")
		fullPath := filepath.Join(dir, changedFile)
		os.MkdirAll(filepath.Dir(fullPath), 0755)
		os.WriteFile(fullPath, []byte("change\n"), 0644)
		gitAuto(t, dir, "add", ".")
		gitAuto(t, dir, "commit", "-m", "add "+changedFile)
	}
}

// worktreeCfgFor returns a Config with Worktree enabled pointing at root,
// using a single repo name. The legacy worktree path (root/worktrees/<id>)
// is what WorktreeForItem resolves to in tests that use cfg.Root().
func worktreeCfgFor(t *testing.T, repo string) (*config.Config, string) {
	t.Helper()
	_, cfg := setupTestEnvWithChangelog(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{repo},
	}
	return cfg, cfg.Root()
}

// TestResolveRepoDirForAuto_NoWorktreeConfig checks the no-config fallback.
func TestResolveRepoDirForAuto_NoWorktreeConfig(t *testing.T) {
	cfg := &config.Config{}
	got := resolveRepoDirForAuto(cfg, "I-001", "theraprac-api")
	// resolveRepoDir with no ParentDir returns the bare repo name.
	if got != "theraprac-api" {
		t.Errorf("got %q, want %q (bare name fallback)", got, "theraprac-api")
	}
}

// TestResolveRepoDirForAuto_WorktreeDirMissing checks the fallback when the
// item has no worktree on disk.
func TestResolveRepoDirForAuto_WorktreeDirMissing(t *testing.T) {
	cfg, _ := worktreeCfgFor(t, "theraprac-api")
	// No worktree dir created — item has no worktree.
	got := resolveRepoDirForAuto(cfg, "I-001", "theraprac-api")
	// Should fall back to resolveRepoDir (bare name when no ParentDir).
	if got == "" {
		t.Error("expected non-empty main-checkout fallback, got empty string")
	}
	if filepath.IsAbs(got) && strings.Contains(got, "worktrees") {
		t.Errorf("got worktree path %q but no worktree exists — expected main checkout", got)
	}
}

// TestResolveRepoDirForAuto_RepoDirMissing checks that a missing repo inside
// an existing worktree returns "" (skip), not another item's clone.
func TestResolveRepoDirForAuto_RepoDirMissing(t *testing.T) {
	cfg, root := worktreeCfgFor(t, "theraprac-api")
	// Create I-001 worktree dir but NOT theraprac-api inside it.
	os.MkdirAll(filepath.Join(root, "worktrees", "I-001"), 0755)
	// Create I-002 worktree with theraprac-api — must NOT be returned for I-001.
	otherRepo := filepath.Join(root, "worktrees", "I-002", "theraprac-api")
	os.MkdirAll(otherRepo, 0755)
	os.WriteFile(filepath.Join(otherRepo, ".git"), []byte("gitdir: ../some/path\n"), 0644)

	got := resolveRepoDirForAuto(cfg, "I-001", "theraprac-api")
	if got != "" {
		t.Errorf("got %q, want empty string (repo absent → skip, no cross-item fallback)", got)
	}
}

// TestResolveRepoDirForAuto_RepoPresent checks that a repo in the item's own
// worktree is returned correctly.
func TestResolveRepoDirForAuto_RepoPresent(t *testing.T) {
	cfg, root := worktreeCfgFor(t, "theraprac-api")
	repoDir := filepath.Join(root, "worktrees", "I-001", "theraprac-api")
	os.MkdirAll(repoDir, 0755)
	os.WriteFile(filepath.Join(repoDir, ".git"), []byte("gitdir: ../some/path\n"), 0644)

	got := resolveRepoDirForAuto(cfg, "I-001", "theraprac-api")
	if got != repoDir {
		t.Errorf("got %q, want %q", got, repoDir)
	}
}

// TestDetectTouchedRepos_UsesItemWorktree confirms that detectTouchedRepos
// reads from the item's own worktree and not from a peer item's clone.
// I-1473: before the fix, Pattern-3 fallback would find I-002's clone and
// return its (empty) diff, causing I-001's suites to be incorrectly skipped.
func TestDetectTouchedRepos_UsesItemWorktree(t *testing.T) {
	cfg, root := worktreeCfgFor(t, "theraprac-api")

	// I-001's worktree has changes (feature file).
	initRepoWithFeatureBranch(t,
		filepath.Join(root, "worktrees", "I-001", "theraprac-api"),
		"internal/billing/charge.go")

	// I-002's worktree has NO changes (stays on main).
	initRepoWithFeatureBranch(t,
		filepath.Join(root, "worktrees", "I-002", "theraprac-api"),
		"" /* no feature branch */)

	touched, err := detectTouchedRepos(cfg, "I-001")
	if err != nil {
		t.Fatalf("detectTouchedRepos: %v", err)
	}
	files, ok := touched["theraprac-api"]
	if !ok || len(files) == 0 {
		t.Errorf("expected I-001's worktree changes to be detected; touched=%v", touched)
	}
	found := false
	for _, f := range files {
		if f == "internal/billing/charge.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("internal/billing/charge.go not in changed files; got %v", files)
	}
}

// TestDetectTouchedRepos_SkipsRepoNotInWorktree confirms that when the item's
// worktree dir exists but the requested repo is absent, the repo is skipped
// (not populated in the touched map), rather than falling back to another
// item's clone. I-1473.
func TestDetectTouchedRepos_SkipsRepoNotInWorktree(t *testing.T) {
	cfg, root := worktreeCfgFor(t, "theraprac-api")

	// I-001 worktree dir exists but theraprac-api is absent.
	os.MkdirAll(filepath.Join(root, "worktrees", "I-001"), 0755)

	// A peer item has theraprac-api with changes — must NOT bleed into I-001.
	initRepoWithFeatureBranch(t,
		filepath.Join(root, "worktrees", "I-002", "theraprac-api"),
		"internal/billing/charge.go")

	touched, err := detectTouchedRepos(cfg, "I-001")
	if err != nil {
		t.Fatalf("detectTouchedRepos: %v", err)
	}
	if _, ok := touched["theraprac-api"]; ok {
		t.Errorf("theraprac-api should be absent from touched when repo is not in I-001's worktree; got %v", touched)
	}
}

// TestDetectTouchedRepos_FallsBackToMainWhenNoWorktree checks that when an
// item has no worktree at all, detectTouchedRepos uses the main checkout.
func TestDetectTouchedRepos_FallsBackToMainWhenNoWorktree(t *testing.T) {
	cfg, root := worktreeCfgFor(t, "theraprac-api")

	// No worktree dir for I-001. Create a "main checkout" in the location
	// resolveRepoDir would return (bare name "theraprac-api" relative to CWD,
	// but since tests don't chdir we use WorktreeBaseLegacy parent).
	// The simplest setup: configure a ParentDir so resolveRepoDir returns an
	// absolute path, then create a git repo there with changes.
	mainRepo := filepath.Join(root, "theraprac-api")
	initRepoWithFeatureBranch(t, mainRepo, "main_change.go")
	cfg.Worktree.ParentDir = root // absolute → RepoParent() returns root directly

	touched, err := detectTouchedRepos(cfg, "I-001")
	if err != nil {
		t.Fatalf("detectTouchedRepos: %v", err)
	}
	// The main checkout has changes — they must be detected.
	if _, ok := touched["theraprac-api"]; !ok {
		t.Errorf("expected main-checkout changes detected when no worktree; touched=%v", touched)
	}
}
