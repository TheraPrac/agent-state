package command

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
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
		{"workspace_test", "as"},
		{"live_acceptance", ""},  // no prefix match
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

func TestSelectAutoSuites_ScopeClassRunsAllClassSuites(t *testing.T) {
	cfg := &config.Config{}
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      makeSuiteConfig("make test-unit"),
			"web_typecheck": makeSuiteConfig("make type-check"),
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": makeSuiteConfig("bash run-changed-hook-tests.sh"),
				},
			},
		},
	}

	item := &model.Item{ScopeClass: "workspace-config"}
	// Even if no "theraprac-api" or "theraprac-web" touched, class suites run
	touched := map[string][]string{
		"as": {"internal/command/test_auto.go"},
	}

	tier1, tier2 := selectAutoSuites(cfg, item, touched)

	if len(tier2) != 0 {
		t.Errorf("scope suites should be empty for workspace-config items, got %v", tier2)
	}
	if len(tier1) != 1 || tier1[0] != "workspace_test" {
		t.Errorf("tier1 should be [workspace_test], got %v", tier1)
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
