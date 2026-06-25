package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupPrepStandaloneEnv builds a fixture with a single sprintless item
// (T-100) plus the same on-disk shape setupPrepWriteOnlyEnv uses, so
// the canned-plan/canned-report engine can be reused. No sprint, no
// epic, no registry entries — exactly the situation PrepStandalone
// must handle. I-571.
func setupPrepStandaloneEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".plans"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .

run:
  permission_mode: dangerously-skip-permissions
  default_model: sonnet
  max_parallelism: 1
  default_budget_usd: 2.00
  step_order: [implement, close]
  steps:
    implement:
      type: claude
    close:
      type: close
      resolution: completed
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Empty registry — no sprint/epic anywhere.
	reg := &registry.Registry{}
	if err := reg.Save(filepath.Join(root, ".as", "epics.yaml")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "tasks", "T-100-standalone.md"), `id: T-100
type: task
status: queued
created: 2026-05-09T10:00:00-06:00
last_touched: 2026-05-09T10:00:00-06:00

completed: null

title: Standalone task

summary: Standalone summary

acceptance_criteria:
- cmd: echo standalone

depends_on:
- []
`)

	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func TestPrepStandaloneItemWithoutSprintWritesPlan(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)
	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		// I-933: report + review are opt-in now.
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true, Review: true}, engine)
	})

	if exit != 0 {
		t.Fatalf("PrepStandalone exit=%d, want 0", exit)
	}
	if !plan.Exists(cfg.PlansDir(), "T-100") {
		t.Errorf("plan sidecar missing for T-100")
	}
	if !plan.ReportExists(cfg.PlansDir(), "T-100") {
		t.Errorf("report sidecar missing for T-100")
	}
	p, _ := plan.Load(cfg.PlansDir(), "T-100")
	if p == nil || p.Approved {
		t.Errorf("expected draft plan (Approved=false), got %+v", p)
	}
	if prepCalls.get() != 1 || reviewCalls.get() != 1 {
		t.Errorf("expected exactly one prep + one review claude call, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
}

func TestPrepStandaloneRespectsApprovedSidecar(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)

	// Pre-seed an approved plan sidecar. plan.Save enforces completeness
	// (scope_repos + ACs must be present), so populate the minimum.
	approved := &plan.Plan{
		Approach:   "Already approved",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: echo standalone"},
		Approved:   true,
		ApprovedAt: plan.Now(),
	}
	if err := plan.Save(cfg.PlansDir(), "T-100", approved); err != nil {
		t.Fatalf("seed approved plan: %v", err)
	}

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true}, engine)
	})

	if exit != 0 {
		t.Fatalf("PrepStandalone exit=%d, want 0 (no-op on approved)", exit)
	}
	if prepCalls.get() != 0 || reviewCalls.get() != 0 {
		t.Errorf("expected no claude calls for approved item, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
	// Plan still approved + untouched.
	p, _ := plan.Load(cfg.PlansDir(), "T-100")
	if p == nil || !p.Approved || p.Approach != "Already approved" {
		t.Errorf("approved plan was mutated: %+v", p)
	}
}

func TestPrepStandaloneRespectsRejectedSidecar(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)

	rejected := &plan.Plan{Approach: "Rejected draft", Rejected: true, RejectedAt: plan.Now()}
	if err := plan.Save(cfg.PlansDir(), "T-100", rejected); err != nil {
		t.Fatalf("seed rejected plan: %v", err)
	}

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true}, engine)
	})

	if exit != 0 {
		t.Fatalf("PrepStandalone exit=%d, want 0", exit)
	}
	if prepCalls.get() != 0 || reviewCalls.get() != 0 {
		t.Errorf("expected no claude calls when rejected and --include-rejected not set, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
}

func TestPrepStandaloneIncludeRejectedReprocesses(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)

	rejected := &plan.Plan{Approach: "Rejected draft", Rejected: true, RejectedAt: plan.Now()}
	if err := plan.Save(cfg.PlansDir(), "T-100", rejected); err != nil {
		t.Fatalf("seed rejected plan: %v", err)
	}

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		// With IncludeRejected, the rejected sidecar should NOT short-circuit.
		// In write-only mode, prepItemWriteOnly resumes from the existing
		// draft (so it may not need a fresh prep call), but the review call
		// always runs — that's enough to prove the standalone path didn't
		// hit the "rejected ⇒ skip" early return.
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true, IncludeRejected: true, Review: true}, engine)
	})

	if exit != 0 {
		t.Fatalf("PrepStandalone exit=%d, want 0", exit)
	}
	if reviewCalls.get() != 1 {
		t.Errorf("expected --include-rejected to re-run plan review (got review=%d, prep=%d)",
			reviewCalls.get(), prepCalls.get())
	}
}

func TestPrepStandaloneItemNotFound(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)
	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		exit = PrepStandalone(s, cfg, "T-999", PrepOpts{WriteOnly: true}, engine)
	})

	if exit != 1 {
		t.Errorf("unknown item should return 1, got %d", exit)
	}
	if prepCalls.get() != 0 || reviewCalls.get() != 0 {
		t.Errorf("expected no claude calls for unknown item, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
}

func TestPrepStandaloneSkipsTerminalStatus(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)
	// Mark the item terminal via a status flip + reload.
	itemPath := filepath.Join(cfg.Root(), "tasks", "T-100-standalone.md")
	contents, _ := os.ReadFile(itemPath)
	updated := strings.Replace(string(contents), "status: queued", "status: done", 1)
	os.WriteFile(itemPath, []byte(updated), 0644)
	s, _ = store.New(cfg)

	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)
	var exit int
	suppressStdout(t, func() {
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true}, engine)
	})

	if exit != 0 {
		t.Errorf("terminal item should exit 0, got %d", exit)
	}
	if prepCalls.get() != 0 || reviewCalls.get() != 0 {
		t.Errorf("expected no claude calls for terminal item, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
}

func TestPrepStandaloneNeverCallsSprintAutoApprove(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)

	menuRec := &recorder{}
	confirmRec := &recorder{}
	engine, _, _ := makeWriteOnlyEngine(nil, menuRec, confirmRec, 0)

	suppressStdout(t, func() {
		// Run twice to ensure no interactive sprint-approval gate could
		// fire even after a successful first pass (which is when
		// maybeAutoApproveSprintPlan would normally engage in Prep).
		PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true}, engine)
		PrepStandalone(s, cfg, "T-100", PrepOpts{WriteOnly: true}, engine)
	})

	if menuRec.get() != 0 {
		t.Errorf("expected zero SelectMenu calls in standalone path, got %d", menuRec.get())
	}
	if confirmRec.get() != 0 {
		t.Errorf("expected zero ConfirmPrompt calls in standalone path, got %d", confirmRec.get())
	}
}

func TestPrepStandaloneDryRunDoesNotCallClaude(t *testing.T) {
	s, cfg := setupPrepStandaloneEnv(t)
	engine, prepCalls, reviewCalls := makeWriteOnlyEngine(nil, nil, nil, 0)

	var exit int
	suppressStdout(t, func() {
		exit = PrepStandalone(s, cfg, "T-100", PrepOpts{DryRun: true}, engine)
	})

	if exit != 0 {
		t.Fatalf("dry-run exit=%d, want 0", exit)
	}
	if prepCalls.get() != 0 || reviewCalls.get() != 0 {
		t.Errorf("dry-run should not call claude, got prep=%d review=%d",
			prepCalls.get(), reviewCalls.get())
	}
	if plan.Exists(cfg.PlansDir(), "T-100") {
		t.Errorf("dry-run should not write a plan sidecar")
	}
}

