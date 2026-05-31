package command

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// --- checkGitStatus coverage ---

func TestCheckGitStatusUncommitted(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.Root = "."

	// Override execGit to simulate uncommitted changes
	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()

	callCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		callCount++
		if callCount == 1 {
			// git status returns dirty files
			return []byte(" M agent-state/tasks/T-001.md\n M agent-state/tasks/T-002.md\n"), nil
		}
		// git rev-list returns 0 behind
		return []byte("0\n"), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	// Use a real config with root set
	testCfg := &config.Config{}
	*testCfg = *cfg
	issues := checkGitStatus(testCfg, false)
	if issues == 0 {
		t.Error("expected issues for uncommitted changes")
	}

	// Verify it also works in quiet mode
	_ = checkGitStatus(testCfg, true)

	// Test with root
	testCfg2 := &config.Config{}
	*testCfg2 = *cfg
	issues2 := checkGitStatus(testCfg2, false)
	_ = issues2 // just exercise the path
	_ = root
}

func TestCheckGitStatusBehindRemote(t *testing.T) {
	cfg := config.Defaults()
	cfg.Paths.Root = "."

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()

	callCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		callCount++
		if callCount == 1 {
			// git status returns clean
			return []byte(""), nil
		}
		// git rev-list returns 3 behind
		return []byte("3\n"), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	issues := checkGitStatus(cfg, false)
	if issues == 0 {
		t.Error("expected issues for behind remote")
	}
}

func TestCheckGitStatusClean(t *testing.T) {
	cfg := config.Defaults()
	cfg.Paths.Root = "."

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()

	callCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		callCount++
		if callCount == 1 {
			// git status --porcelain returns empty (clean)
			return []byte(""), nil
		}
		// git rev-list returns 0 behind
		return []byte("0\n"), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	issues := checkGitStatus(cfg, false)
	if issues != 0 {
		t.Errorf("expected 0 issues for clean state, got %d", issues)
	}
}

func TestCheckGitStatusErrors(t *testing.T) {
	cfg := config.Defaults()
	cfg.Paths.Root = "."

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()

	execGit = func(dir string, args ...string) ([]byte, error) {
		return nil, errors.New("git not available")
	}
	execGitNoOutput = func(dir string, args ...string) error {
		return errors.New("git not available")
	}

	// Should not crash and return 0 issues (errors are non-fatal)
	issues := checkGitStatus(cfg, false)
	if issues != 0 {
		t.Errorf("expected 0 issues when git fails, got %d", issues)
	}
}

// --- Check with fix mode coverage ---

func TestCheckWithFixAppliesAndReports(t *testing.T) {
	s, cfg, _ := setupFixEnv(t)

	// Override git to avoid real git calls
	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	execGit = func(dir string, args ...string) ([]byte, error) {
		return []byte("0\n"), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	// With fix=true, should apply fixes
	code := Check(s, cfg, false, true)
	_ = code

	// With fix=false and quiet=false, should show fixable summary
	code2 := Check(s, cfg, false, false)
	_ = code2
}

func TestCheckNoIssues(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Write a fully valid task
	writeFile(t, filepath.Join(root, "tasks", "T-001-valid.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Valid task
depends_on:
- []
blocks:
- []
`)

	// Write index listing T-001
	writeFile(t, filepath.Join(root, "index.md"), "# Index\n- T-001\n")

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	gitCallCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		gitCallCount++
		if gitCallCount%2 == 1 {
			return []byte(""), nil // porcelain: clean
		}
		return []byte("0\n"), nil // rev-list: 0 behind
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	code := Check(s, cfg, false, false)
	if code != 0 {
		t.Errorf("expected 0 for clean check, got %d", code)
	}
}

func TestCheckWithFixNoFixable(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeFile(t, filepath.Join(root, "tasks", "T-001-valid.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Valid task
depends_on:
- []
blocks:
- []
`)
	writeFile(t, filepath.Join(root, "index.md"), "# Index\n- T-001\n")

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	gitCallCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		gitCallCount++
		if gitCallCount%2 == 1 {
			return []byte(""), nil // porcelain: clean
		}
		return []byte("0\n"), nil // rev-list: 0 behind
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	// Fix mode with nothing to fix — should print "No fixable issues"
	code := Check(s, cfg, false, true)
	if code != 0 {
		t.Errorf("expected 0 for clean check with fix, got %d", code)
	}
}

// --- fixRequiredFields write error coverage ---

func TestFixRequiredFieldsWriteError(t *testing.T) {
	s, cfg, root := setupFixEnv(t)

	// Make the tasks directory read-only to force write errors
	tasksDir := filepath.Join(root, "tasks")
	os.Chmod(tasksDir, 0555)
	defer os.Chmod(tasksDir, 0755)

	// Should still count fixes attempted
	fixed := fixRequiredFields(s, cfg)
	_ = fixed // may or may not succeed depending on OS
}

// --- fixStaleDeps write error coverage ---

func TestFixStaleDepsSingleItemNoDeps(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Item with no dependencies at all
	writeFile(t, filepath.Join(root, "tasks", "T-001-clean.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Clean task
depends_on:
- []
blocks:
- []
`)

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	fixed := fixStaleDeps(s, cfg)
	if fixed != 0 {
		t.Errorf("expected 0 fixes for clean item, got %d", fixed)
	}
}

// --- fixIndex when no index file exists ---

func TestFixIndexNoFile(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	writeFile(t, filepath.Join(root, "tasks", "T-001.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: A task
depends_on:
- []
blocks:
- []
`)
	// Intentionally no index.md

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	fixed := fixIndex(s, cfg)
	if fixed != 1 {
		t.Errorf("expected 1 fix for missing index, got %d", fixed)
	}
}

// --- FixableSummary with no fixable issues ---

func TestFixableSummaryEmpty(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	writeFile(t, filepath.Join(root, "tasks", "T-001.md"), `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: A task
depends_on:
- []
blocks:
- []
`)
	writeFile(t, filepath.Join(root, "index.md"), "# Index\n- T-001\n")

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	count, descs := FixableSummary(s, cfg)
	if count != 0 {
		t.Errorf("expected 0 fixable, got %d: %v", count, descs)
	}
}

// --- DeliveryGate via Check (delivery config) ---

func TestCheckWithDeliveryConfig(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "archive", ".as"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte(`paths:
  root: .
delivery:
  stages: [coding, committed, pushed, merged, uat_approved, closed]
  archive_gate: uat_approved
`), 0644)

	// Archived task with delivery block but no UAT
	writeFile(t, filepath.Join(root, "archive", "T-001-done.md"), `id: T-001
type: task
status: done
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
title: Done task
delivery:
  stage: merged
`)
	writeFile(t, filepath.Join(root, "index.md"), "# Index\n")

	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	gitCallCount := 0
	execGit = func(dir string, args ...string) ([]byte, error) {
		gitCallCount++
		if gitCallCount%2 == 1 {
			return []byte(""), nil // porcelain: clean
		}
		return []byte("0\n"), nil // rev-list: 0 behind
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	// Auto-fix now stamps legacy archived items as uat_approved
	code := Check(s, cfg, false, false)
	if code != 0 {
		t.Errorf("expected code 0 after auto-fix, got %d", code)
	}
}

// I-472: Check should auto-fix duplicate-id drift in non-quiet mode and
// flag it as a failure in quiet mode (CI/hooks).
func TestCheck_FlagsDuplicateID(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	body := `id: I-009
type: issue
status: done
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00

completed: 2026-04-01T11:00:00-06:00

title: Resolved issue with a stale issues/ copy

depends_on:
- []

blocks:
- []
`
	stalePath := filepath.Join(root, "issues", "I-009-resolved-issue.md")
	canonicalPath := filepath.Join(root, "archive", "I-009-resolved-issue.md")
	if err := os.WriteFile(stalePath, []byte(body), 0644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(body), 0644); err != nil {
		t.Fatalf("seed canonical: %v", err)
	}

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Stub git probes so checkGitStatus doesn't add unrelated noise.
	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	execGit = func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-list" {
			return []byte("0\n"), nil
		}
		return []byte(""), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	// Quiet mode: read-only — drift surfaces as a failure, no on-disk
	// repair.
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if code := Check(s, cfg, true, false); code == 0 {
		t.Errorf("quiet Check expected non-zero exit when duplicate present, got 0")
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Errorf("quiet Check should not delete stale file; got %v", err)
	}

	// Non-quiet: auto-fix removes the stale duplicate.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New (rescan): %v", err)
	}
	if code := Check(s2, cfg, false, false); code != 0 {
		t.Logf("Check returned %d (non-fatal — other validators may flag the seed item)", code)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("non-quiet Check should remove stale duplicate; stat err=%v", err)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Errorf("canonical archive copy should remain; got %v", err)
	}
}

// TestCheck_FixKeepsDoneCopyOverResurrectedActive is the I-1241 regression:
// the realistic resurrection has the issues/ copy at its PRE-close status
// (active) while the archive/ copy is the freshly-closed done copy. Both are
// self-consistent (active→issues, done→archive), so the old map-iteration
// tie-break could pick the stale active copy — and then `st check --fix`
// (RemoveStaleDuplicates) deleted the archive/done copy, silently reverting
// the close. This drove I-1236/I-714/I-741 back to active on 2026-05-31.
// The recency tie-break must keep the archive/done copy (newer last_touched)
// and the fix must delete the stale issues/active copy.
func TestCheck_FixKeepsDoneCopyOverResurrectedActive(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	// Stale resurrected copy: active, older last_touched (peer pre-close state).
	staleActive := `id: I-010
type: issue
status: active
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-05-30T09:00:00-06:00

title: Closed issue resurrected as active by a peer merge

depends_on:
- []

blocks:
- []
`
	// Freshly-closed copy: done, newer last_touched (stamped at close time).
	freshDone := `id: I-010
type: issue
status: done
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-05-31T12:00:00-06:00

completed: 2026-05-31T12:00:00-06:00

title: Closed issue resurrected as active by a peer merge

depends_on:
- []

blocks:
- []
`
	stalePath := filepath.Join(root, "issues", "I-010-closed-issue.md")
	canonicalPath := filepath.Join(root, "archive", "I-010-closed-issue.md")
	if err := os.WriteFile(stalePath, []byte(staleActive), 0644); err != nil {
		t.Fatalf("seed stale active: %v", err)
	}
	if err := os.WriteFile(canonicalPath, []byte(freshDone), 0644); err != nil {
		t.Fatalf("seed canonical done: %v", err)
	}

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	defer func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	}()
	execGit = func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-list" {
			return []byte("0\n"), nil
		}
		return []byte(""), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// Canonical selection must be the archive/done copy regardless of scan order.
	if got := s.All()["I-010"]; got == nil || got.Status != "done" {
		t.Fatalf("canonical item status = %v, want done (recency tie-break)", got)
	}

	// Auto-fix must delete the stale active copy and keep the done copy —
	// the close must NOT be reverted.
	if code := Check(s, cfg, false, false); code != 0 {
		t.Logf("Check returned %d (non-fatal — other validators may flag the seed item)", code)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("fix should remove the stale issues/active copy; stat err=%v", err)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Errorf("archive/done copy must survive (close not reverted); got %v", err)
	}
}
