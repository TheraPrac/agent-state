package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// buildScopeCheckCfg creates a minimal config with two scope suites:
//
//	api_integration (repo_trigger: theraprac-api)
//	web_e2e        (triggers: [src/app/**, src/components/**])
func buildScopeCheckCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte(`paths:
  root: .

testing:
  enabled: true
  scope_suites:
    api_integration:
      command: cd ../theraprac-api && make integration-local
      repo_trigger: theraprac-api
    web_e2e:
      command: cd ../theraprac-web && scripts/e2e-local.sh run
      triggers: [src/app/**, src/components/**]

worktree:
  enabled: true
  base_dir: worktrees
  parent_dir: ..
  repos: [theraprac-api, theraprac-web]
`), 0644)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// buildScopeItem builds a minimal started item with the given testing_evidence entries.
func buildScopeItem(id string, evidence map[string]string) *model.Item {
	item := &model.Item{
		ID:     id,
		Type:   "task",
		Status: "active",
	}
	if len(evidence) > 0 {
		item.TestingEvidence = make(map[string]interface{})
		for k, v := range evidence {
			item.TestingEvidence[k] = v
		}
	}
	return item
}

// stubbedOpts returns CloseScopeCheckOpts with injected git and worktree fakes.
//
// repoFiles maps repo → slice of changed files returned by git diff.
// If a repo is absent from the map, the git diff returns empty (no changes).
// resolvedRepos lists which repos have a "present" worktree.
func stubbedOpts(repoFiles map[string][]string, resolvedRepos []string) CloseScopeCheckOpts {
	resolved := map[string]bool{}
	for _, r := range resolvedRepos {
		resolved[r] = true
	}
	return CloseScopeCheckOpts{
		ResolveWorktree: func(_ *config.Config, _, repo string) string {
			if resolved[repo] {
				return "/fake/worktree/" + repo
			}
			return ""
		},
		RunGit: func(dir string, args ...string) (string, error) {
			// Extract repo name from fake dir path.
			repo := filepath.Base(dir)
			files := repoFiles[repo]
			return strings.Join(files, "\n"), nil
		},
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestCloseScopeSuiteCheck_NoChanges(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", nil)
	// No worktrees present → no changed files → pass
	opts := stubbedOpts(nil, nil)
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty message (no changes), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_ApiChange_SuitePasses(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", map[string]string{
		"api_integration": "pass 2026-06-14T10:00:00-06:00",
	})
	opts := stubbedOpts(
		map[string][]string{"theraprac-api": {"internal/auth/handler.go"}},
		[]string{"theraprac-api"},
	)
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty message (suite passed), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_ApiChange_SuiteMissing(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", nil) // no evidence recorded
	opts := stubbedOpts(
		map[string][]string{"theraprac-api": {"internal/auth/handler.go"}},
		[]string{"theraprac-api"},
	)
	msg := closeScopeSuiteCheck(item, cfg, opts)
	if msg == "" {
		t.Error("expected error message (api_integration missing), got empty")
	}
	if !strings.Contains(msg, "api_integration") {
		t.Errorf("expected message to name api_integration, got: %s", msg)
	}
	if !strings.Contains(msg, "T-001") {
		t.Errorf("expected message to include item ID T-001, got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_WebE2ETrigger_GlobMatch(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	// web_e2e not recorded; web file matches src/app/** trigger
	item := buildScopeItem("T-001", nil)
	opts := stubbedOpts(
		map[string][]string{"theraprac-web": {"src/app/page.tsx"}},
		[]string{"theraprac-web"},
	)
	msg := closeScopeSuiteCheck(item, cfg, opts)
	if msg == "" {
		t.Error("expected error message (web_e2e missing), got empty")
	}
	if !strings.Contains(msg, "web_e2e") {
		t.Errorf("expected message to name web_e2e, got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_WebE2ETrigger_NoMatch(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	// web file does NOT match src/app/** or src/components/** triggers
	// No repo_trigger on web_e2e, so it's not applicable here
	item := buildScopeItem("T-001", nil)
	opts := stubbedOpts(
		map[string][]string{"theraprac-web": {"src/lib/utils.ts"}},
		[]string{"theraprac-web"},
	)
	// theraprac-web has no repo_trigger (only api_integration does),
	// and src/lib/utils.ts doesn't match web_e2e triggers → pass
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty (no applicable suite), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_SkipFlag(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	// Even with missing evidence, Skip=true bypasses all checks
	item := buildScopeItem("T-001", nil)
	opts := stubbedOpts(
		map[string][]string{"theraprac-api": {"internal/auth/handler.go"}},
		[]string{"theraprac-api"},
	)
	opts.Skip = true
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty (skip=true), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_SuiteSkipped(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", map[string]string{
		"api_integration": "skip: no API changes",
	})
	opts := stubbedOpts(
		map[string][]string{"theraprac-api": {"internal/auth/handler.go"}},
		[]string{"theraprac-api"},
	)
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty (suite skipped), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_AutoSkip(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", map[string]string{
		"api_integration": "auto-skip: no files changed in theraprac-api",
	})
	opts := stubbedOpts(
		map[string][]string{"theraprac-api": {"internal/auth/handler.go"}},
		[]string{"theraprac-api"},
	)
	if msg := closeScopeSuiteCheck(item, cfg, opts); msg != "" {
		t.Errorf("expected empty (auto-skip), got: %s", msg)
	}
}

func TestCloseScopeSuiteCheck_BothSuitesMissing(t *testing.T) {
	cfg := buildScopeCheckCfg(t)
	item := buildScopeItem("T-001", nil)
	opts := stubbedOpts(
		map[string][]string{
			"theraprac-api": {"internal/auth/handler.go"},
			"theraprac-web": {"src/app/page.tsx"},
		},
		[]string{"theraprac-api", "theraprac-web"},
	)
	msg := closeScopeSuiteCheck(item, cfg, opts)
	if msg == "" {
		t.Error("expected error message (both suites missing), got empty")
	}
	if !strings.Contains(msg, "api_integration") || !strings.Contains(msg, "web_e2e") {
		t.Errorf("expected both suites named, got: %s", msg)
	}
}

// ── matchScopeGlob unit tests ─────────────────────────────────────────────────

func TestMatchScopeGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"src/app/**", "src/app/page.tsx", true},
		{"src/app/**", "src/app/deep/nested/file.ts", true},
		{"src/app/**", "src/lib/utils.ts", false},
		{"src/components/**", "src/components/Button.tsx", true},
		{"src/components/**", "src/app/page.tsx", false},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"src/app/**", "other/src/app/page.tsx", false},
	}
	for _, c := range cases {
		got := matchScopeGlob(c.pattern, c.path)
		if got != c.want {
			t.Errorf("matchScopeGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
