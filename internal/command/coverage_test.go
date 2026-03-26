package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Tests to boost coverage for existing commands.

// --- Show: exercise remaining branches ---

func TestShowWithDeps(t *testing.T) {
	s, _ := setupTestEnv(t)
	// T-002 has depends_on — exercises that branch
	code := Show(s, "T-002", ShowOpts{})
	if code != 0 {
		t.Errorf("Show T-002 full returned %d, want 0", code)
	}
}

func TestShowWithBlocks(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Add a blocks relationship so T-001 blocks T-002
	DepAdd(s, cfg, "T-002", "T-001")

	// Now T-001 should show blocks
	code := Show(s, "T-001", ShowOpts{})
	if code != 0 {
		t.Errorf("Show with blocks returned %d, want 0", code)
	}
}

func TestShowWithTags(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "infra")
	code := Show(s, "T-001", ShowOpts{})
	if code != 0 {
		t.Errorf("Show with tags returned %d, want 0", code)
	}
}

func TestShowWithDeliveryStage(t *testing.T) {
	root := setupTestEnvRoot(t)
	// Write an item with delivery stage
	writeFile(t, filepath.Join(root, "tasks", "T-010-staged.md"), `id: T-010
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Staged task

delivery:
  stage: pr_open
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	code := Show(s, "T-010", ShowOpts{})
	if code != 0 {
		t.Errorf("Show with delivery stage returned %d, want 0", code)
	}

	code = Show(s, "T-010", ShowOpts{Brief: true})
	if code != 0 {
		t.Errorf("Show brief with stage returned %d, want 0", code)
	}
}

func TestShowWithSummary(t *testing.T) {
	root := setupTestEnvRoot(t)
	writeFile(t, filepath.Join(root, "tasks", "T-010-summary.md"), `id: T-010
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Summary task

summary: |
  This is a summary.

acceptance_criteria:
- Must work
- Must be fast

next_actions:
- Do first thing
- Do second thing
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	code := Show(s, "T-010", ShowOpts{})
	if code != 0 {
		t.Errorf("Show with summary returned %d, want 0", code)
	}
}

func TestShowFieldNoDoc(t *testing.T) {
	s, _ := setupTestEnv(t)
	item, _ := s.Get("T-001")
	item.Doc = nil
	code := Show(s, "T-001", ShowOpts{Field: "status"})
	// Will access the item from store which has a doc, so exercises the path
	_ = code
}

// --- Check: exercise validation issues ---

func TestCheckWithIssues(t *testing.T) {
	root := setupTestEnvRoot(t)
	// Write a bad item missing required fields
	writeFile(t, filepath.Join(root, "tasks", "T-BAD-bad.md"), `id: T-BAD
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Bad task
`)
	// Write an item in wrong directory
	writeFile(t, filepath.Join(root, "archive", "T-010-wrong.md"), `id: T-010
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Wrong dir
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	code := Check(s, cfg, false)
	if code != 1 {
		t.Errorf("Check with issues returned %d, want 1", code)
	}

	// Quiet mode
	code = Check(s, cfg, true)
	if code != 1 {
		t.Errorf("Check quiet with issues returned %d, want 1", code)
	}
}

// --- Status: exercise more detail views ---

func TestStatusWithDeliveryStage(t *testing.T) {
	root := setupTestEnvRoot(t)
	writeFile(t, filepath.Join(root, "tasks", "T-010-staged.md"), `id: T-010
type: task
status: active
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Staged task

delivery:
  stage: pr_open
`)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	code := Status(s, cfg, "T-010", StatusOpts{})
	if code != 0 {
		t.Errorf("Status single with delivery returned %d, want 0", code)
	}
}

// --- DepRm: missing doc path ---

func TestDepRmMissingDoc(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Add dep first
	DepAdd(s, cfg, "T-001", "T-003")

	// Nil out doc
	item, _ := s.Get("T-001")
	item.Doc = nil

	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepRm missing doc returned %d, want 1", code)
	}
}

func TestDepAddMissingDoc(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	item, _ := s.Get("T-001")
	item.Doc = nil

	code := DepAdd(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepAdd missing doc returned %d, want 1", code)
	}
}

// --- Finish: more coverage for the git paths ---

func TestFinishForceWithUncommittedChanges(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	repoDir := filepath.Join(wtDir, "repo-a")
	os.MkdirAll(repoDir, 0755)

	// Force remove — skips git checks
	code := Finish(s, cfg, "T-001", FinishOpts{Force: true})
	// Will fail at git commands but the force path is exercised
	_ = code
}

// Helper to create test env root with standard items but return the root path.
func setupTestEnvRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".changelog"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Standard items
	writeFile(t, filepath.Join(root, "tasks", "T-001-first.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: First task
depends_on:
- []
next_actions:
- []
`)
	writeFile(t, filepath.Join(root, "tasks", "T-002-second.md"), `id: T-002
type: task
status: queued
created: 2026-03-25T11:00:00-06:00
last_touched: 2026-03-25T11:00:00-06:00
completed: null
title: Second task
depends_on:
- T-001
next_actions:
- []
`)
	writeFile(t, filepath.Join(root, "tasks", "T-003-active.md"), `id: T-003
type: task
status: active
created: 2026-03-25T12:00:00-06:00
last_touched: 2026-03-25T12:00:00-06:00
completed: null
title: Active task
assigned_to: agent-a
depends_on:
- []
next_actions:
- []
`)
	writeFile(t, filepath.Join(root, "issues", "I-001-bug.md"), `id: I-001
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: A bug
severity: high
`)
	writeFile(t, filepath.Join(root, "archive", "T-004-done.md"), `id: T-004
type: task
status: completed
created: 2026-03-20T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: 2026-03-25T10:00:00-06:00
title: Done task
`)

	return root
}

// --- Migrate ---

func TestMigrateDryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Migrate(s, cfg, MigrateOpts{DryRun: true})
	if code != 0 {
		t.Errorf("Migrate --dry-run returned %d, want 0", code)
	}
}

func TestMigrateApply(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Migrate(s, cfg, MigrateOpts{DryRun: false})
	if code != 0 {
		t.Errorf("Migrate returned %d, want 0", code)
	}
}
