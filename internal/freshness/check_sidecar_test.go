package freshness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// setupCheckTestEnv writes a minimal workspace + item file +
// optional plan sidecar so Check() can be exercised end-to-end
// against the (cfg, store) pair the production caller passes.
func setupCheckTestEnv(t *testing.T, withSidecar bool) (*store.Store, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", ".as", "agent-state", "agent-state/.plans"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// approvedAt is always 24h ago so the 7-day drift cutoff never triggers.
	approvedAt := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	itemBody := fmt.Sprintf(`id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Fixture task
plan_approved: true
plan_approved_at: %s`, approvedAt) + `

depends_on:
- []

next_actions:
- []

sbar:
  situation: |-
    Fixture used by freshness check_sidecar_test.
  background: |-
    Fixture content.
  assessment: |-
    Fixture content.
  recommendation: |-
    Fixture content.
`
	if err := os.WriteFile(filepath.Join(root, "tasks", "T-001-fixture.md"), []byte(itemBody), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if withSidecar {
		if err := os.MkdirAll(cfg.PlansDir(), 0755); err != nil {
			t.Fatal(err)
		}
		sidecar := "---\nscope_repos: [as]\nplan_approved: true\n---\n\n## Approach\nReal.\n\n## Acceptance Criteria\n- cmd: go test ./...\n"
		if err := os.WriteFile(filepath.Join(cfg.PlansDir(), "T-001.md"), []byte(sidecar), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return s, cfg, root
}

// TestCheckReturnsStaleOnMissingSidecar — I-716's freshness arm:
// an item with plan_approved=true but no .plans/<id>.md returns
// VerdictStale with a file-missing finding mentioning the sidecar
// path.
func TestCheckReturnsStaleOnMissingSidecar(t *testing.T) {
	s, cfg, _ := setupCheckTestEnv(t, false)

	result, err := Check(cfg, s, "T-001", CheckOpts{
		SkipCache: true,
	})
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if result.Verdict != VerdictStale {
		t.Errorf("expected VerdictStale on missing sidecar; got %s", result.Verdict)
	}
	if len(result.Findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	found := false
	for _, f := range result.Findings {
		if f.Category == CategoryFileMissing && strings.Contains(f.Message, ".plans/T-001.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a file-missing finding mentioning the sidecar path; got %v", result.Findings)
	}
}

// TestCheckReturnsFreshOnPresentSidecar — control test: same
// fixture but with the sidecar present yields Fresh (no
// findings). Anchors the missing-sidecar refusal as the specific
// trigger.
func TestCheckReturnsFreshOnPresentSidecar(t *testing.T) {
	s, cfg, _ := setupCheckTestEnv(t, true)

	result, err := Check(cfg, s, "T-001", CheckOpts{
		SkipCache: true,
		// Single scope_repo and the sidecar references no files
		// to modify, so file-existence is a no-op.
	})
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if result.Verdict != VerdictFresh {
		t.Errorf("expected VerdictFresh; got %s (findings: %v)", result.Verdict, result.Findings)
	}
}

// Ensure model import isn't dropped if I refactor the helper later.
var _ = model.Item{}
