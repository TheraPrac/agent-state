package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupTestEnvWithChangelog creates a standard test env with a .changelog directory.
func setupTestEnvWithChangelog(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	return s, cfg
}

// === Tag ===

func TestTagAddHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "add", "security")
	if code != 0 {
		t.Errorf("Tag add returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 1 || item.Tags[0] != "security" {
		t.Errorf("Tags = %v, want [security]", item.Tags)
	}

	// Verify changelog
	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "tag_add" && e.NewValue == "security" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for tag_add")
	}
}

func TestTagAddDuplicate(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "alpha")
	code := Tag(s, cfg, "T-001", "add", "alpha")
	if code != 1 {
		t.Errorf("Tag add duplicate returned %d, want 1", code)
	}
}

func TestTagRmHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "alpha")
	Tag(s, cfg, "T-001", "add", "beta")

	code := Tag(s, cfg, "T-001", "rm", "alpha")
	if code != 0 {
		t.Errorf("Tag rm returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 1 || item.Tags[0] != "beta" {
		t.Errorf("Tags = %v, want [beta]", item.Tags)
	}
}

func TestTagRmNotPresent(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "rm", "nonexistent")
	if code != 1 {
		t.Errorf("Tag rm nonexistent returned %d, want 1", code)
	}
}

func TestTagRmLastTag(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Tag(s, cfg, "T-001", "add", "only-tag")

	code := Tag(s, cfg, "T-001", "rm", "only-tag")
	if code != 0 {
		t.Errorf("Tag rm last returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	if len(item.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", item.Tags)
	}
}

func TestTagNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-999", "add", "foo")
	if code != 1 {
		t.Errorf("Tag not found returned %d, want 1", code)
	}
}

func TestTagBadAction(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Tag(s, cfg, "T-001", "flip", "foo")
	if code != 2 {
		t.Errorf("Tag bad action returned %d, want 2", code)
	}
}

func TestTagAddRoundtrip(t *testing.T) {
	// Regression test: tag add must survive write→re-parse cycle.
	// Previously, updateTagsInDoc wrote inline "[x, y]" format that the
	// parser couldn't read back, silently dropping tags.
	s, cfg := setupTestEnvWithChangelog(t)

	Tag(s, cfg, "T-001", "add", "security")
	Tag(s, cfg, "T-001", "add", "billing")

	// Force re-parse by creating a new store from the same directory
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}

	item, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after re-parse")
	}
	if len(item.Tags) != 2 {
		t.Fatalf("Tags after roundtrip = %v, want [security billing]", item.Tags)
	}
	if item.Tags[0] != "security" || item.Tags[1] != "billing" {
		t.Errorf("Tags = %v, want [security billing]", item.Tags)
	}

	// Verify the file content uses multi-line format
	path, _ := s2.Path("T-001")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "- security") || !strings.Contains(content, "- billing") {
		t.Errorf("file should use multi-line format, got:\n%s", content)
	}
}

// === Dep Add/Rm ===

func TestDepAddHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-001", "T-003")
	if code != 0 {
		t.Errorf("DepAdd returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	found := false
	for _, d := range item.DependsOn {
		if d == "T-003" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-001 DependsOn = %v, want to contain T-003", item.DependsOn)
	}

	dep, _ := s.Get("T-003")
	found = false
	for _, b := range dep.Blocks {
		if b == "T-001" {
			found = true
		}
	}
	if !found {
		t.Errorf("T-003 Blocks = %v, want to contain T-001", dep.Blocks)
	}
}

func TestDepAddDuplicate(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	DepAdd(s, cfg, "T-001", "T-003")
	code := DepAdd(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepAdd duplicate returned %d, want 1", code)
	}
}

func TestDepAddSelf(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-001", "T-001")
	if code != 2 {
		t.Errorf("DepAdd self returned %d, want 2", code)
	}
}

func TestDepAddNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepAdd(s, cfg, "T-999", "T-001")
	if code != 1 {
		t.Errorf("DepAdd missing id returned %d, want 1", code)
	}
	code = DepAdd(s, cfg, "T-001", "T-999")
	if code != 1 {
		t.Errorf("DepAdd missing dep returned %d, want 1", code)
	}
}

func TestDepRmHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// First add the dependency
	DepAdd(s, cfg, "T-001", "T-003")

	// Then remove it
	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 0 {
		t.Errorf("DepRm returned %d, want 0", code)
	}

	item, _ := s.Get("T-001")
	for _, d := range item.DependsOn {
		if d == "T-003" {
			t.Error("T-003 should be removed from depends_on")
		}
	}

	dep, _ := s.Get("T-003")
	for _, b := range dep.Blocks {
		if b == "T-001" {
			t.Error("T-001 should be removed from blocks")
		}
	}
}

func TestDepRmNotDependency(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("DepRm non-dependency returned %d, want 1", code)
	}
}

func TestDepRmNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := DepRm(s, cfg, "T-999", "T-001")
	if code != 1 {
		t.Errorf("DepRm missing id returned %d, want 1", code)
	}
}

// === Log ===

func TestLogSingleHappy(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	// Create some changelog entries
	changelog.Append(cfg, "T-001", changelog.Entry{
		Timestamp: "2026-03-25T10:00:00-06:00", Op: "create", NewValue: "queued",
	})
	changelog.Append(cfg, "T-001", changelog.Entry{
		Timestamp: "2026-03-25T11:00:00-06:00", Op: "start", OldValue: "queued", NewValue: "active",
	})

	code := Log(s, cfg, "T-001", LogOpts{})
	if code != 0 {
		t.Errorf("Log T-001 returned %d, want 0", code)
	}
}

func TestLogSingleNoEntries(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "T-001", LogOpts{})
	if code != 0 {
		t.Errorf("Log empty returned %d, want 0", code)
	}
}

func TestLogSingleNotFound(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "T-999", LogOpts{})
	if code != 1 {
		t.Errorf("Log not found returned %d, want 1", code)
	}
}

func TestLogAll(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	changelog.Append(cfg, "T-001", changelog.Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})
	changelog.Append(cfg, "T-002", changelog.Entry{Op: "create", Timestamp: "2026-03-25T10:00:00-06:00"})

	code := Log(s, cfg, "", LogOpts{})
	if code != 0 {
		t.Errorf("Log all returned %d, want 0", code)
	}
}

func TestLogAllEmpty(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Log(s, cfg, "", LogOpts{})
	if code != 0 {
		t.Errorf("Log all empty returned %d, want 0", code)
	}
}

func TestLogWithLimit(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	for i := 0; i < 10; i++ {
		changelog.Append(cfg, "T-001", changelog.Entry{Op: "update", Timestamp: "2026-03-25T10:00:00-06:00"})
	}

	code := Log(s, cfg, "T-001", LogOpts{Limit: 3})
	if code != 0 {
		t.Errorf("Log with limit returned %d, want 0", code)
	}
}

// === Create with priority (I-406) ===

// I-406: severity is dead. The CLI rejects --severity at the entry point;
// callers use --priority (0-4) instead.
func TestCreateIssueRejectsSeverity(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Create(s, cfg, "issue", "Critical bug", CreateOpts{Priority: 0, Severity: "critical"})
	if code != 2 {
		t.Errorf("Create issue with --severity should exit 2 (deprecated), got %d", code)
	}
}

func TestCreateIssueWithPriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Create(s, cfg, "issue", "Critical bug", CreateOpts{Priority: 0})
	if code != 0 {
		t.Errorf("Create issue with priority=0 returned %d, want 0", code)
	}
	item, ok := s.Get("I-002")
	if !ok {
		t.Fatal("I-002 should exist after create")
	}
	if item.Priority == nil || *item.Priority != 0 {
		t.Errorf("priority = %v, want 0", item.Priority)
	}
	path, _ := s.Path("I-002")
	content, _ := os.ReadFile(path)
	if !containsStr(string(content), "priority: 0") {
		t.Error("file should contain 'priority: 0'")
	}
}

// I-406: `st update <id> severity <anything>` must exit non-zero with
// the migration pointer rather than silently writing a deprecated field.
func TestUpdateRejectsSeverity(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	code := Update(s, cfg, "I-001", "severity", "high", UpdateModeValue)
	if code != 2 {
		t.Errorf("Update severity should exit 2 (deprecated), got %d", code)
	}
}

// I-406: `st update <id> priority 9` must exit non-zero with a clear
// must-be-0-4 message — the new value-set check.
func TestUpdateRejectsOutOfRangePriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Update(s, cfg, "T-003", "priority", "9", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=9 should exit 2, got %d", code)
	}
	if code := Update(s, cfg, "T-003", "priority", "-1", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=-1 should exit 2, got %d", code)
	}
	if code := Update(s, cfg, "T-003", "priority", "abc", UpdateModeValue); code != 2 {
		t.Errorf("Update priority=abc should exit 2, got %d", code)
	}
}

// I-406: priority must be 0-4. Out-of-range rejected at create time.
func TestCreateRejectsOutOfRangePriority(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	if code := Create(s, cfg, "task", "x", CreateOpts{Priority: 9}); code != 2 {
		t.Errorf("Create with priority=9 should exit 2, got %d", code)
	}
	if code := Create(s, cfg, "task", "x", CreateOpts{Priority: -1}); code != 2 {
		t.Errorf("Create with priority=-1 should exit 2, got %d", code)
	}
}

func TestCreateRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Create(s, cfg, "task", "Changelog test", CreateOpts{Priority: 2})

	entries, _ := changelog.Read(cfg, "T-005")
	if len(entries) != 1 {
		t.Fatalf("expected 1 changelog entry, got %d", len(entries))
	}
	if entries[0].Op != "create" {
		t.Errorf("op = %q, want create", entries[0].Op)
	}
}

// === Finish with worktree ===

func TestFinishWithWorktreeConfig(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	// Create a worktree directory structure
	wtDir := filepath.Join(cfg.Root(), "worktrees", "T-001")
	repoDir := filepath.Join(wtDir, "repo-a")
	os.MkdirAll(repoDir, 0755)

	// Dry run
	code := Finish(s, cfg, "T-001", FinishOpts{DryRun: true})
	if code != 0 {
		t.Errorf("Finish dry-run returned %d, want 0", code)
	}
}

func TestFinishListEmpty(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	// Create the base dir but empty
	os.MkdirAll(filepath.Join(cfg.Root(), "worktrees"), 0755)

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list returned %d, want 0", code)
	}
}

func TestFinishListNonexistent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "nonexistent",
	}

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list nonexistent returned %d, want 0", code)
	}
}

func TestFinishWorktreeNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	os.MkdirAll(filepath.Join(cfg.Root(), "worktrees"), 0755)

	code := Finish(s, cfg, "T-999", FinishOpts{})
	if code != 1 {
		t.Errorf("Finish not found returned %d, want 1", code)
	}
}

func TestFinishNoIDWithWorktree(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}

	code := Finish(s, cfg, "", FinishOpts{})
	if code != 2 {
		t.Errorf("Finish no ID returned %d, want 2", code)
	}
}

// === Coverage: Start records changelog ===

func TestStartRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Start(s, cfg, "T-001", StartOpts{})

	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "start" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for start")
	}
}

// === Coverage: Close records changelog ===

func TestCloseRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Close(s, cfg, "T-003", "completed", CloseOpts{})

	entries, _ := changelog.Read(cfg, "T-003")
	found := false
	for _, e := range entries {
		if e.Op == "close" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for close")
	}
}

// === Coverage: Update records changelog ===

func TestUpdateRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	Update(s, cfg, "T-001", "title", "New title", UpdateModeValue)

	entries, _ := changelog.Read(cfg, "T-001")
	found := false
	for _, e := range entries {
		if e.Op == "update" && e.Field == "title" {
			found = true
		}
	}
	if !found {
		t.Error("expected changelog entry for update")
	}
}

// === Coverage: DepAdd/Rm record changelog ===

func TestDepAddRecordsChangelog(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	DepAdd(s, cfg, "T-001", "T-003")

	// Check both items got entries
	entries1, _ := changelog.Read(cfg, "T-001")
	entries3, _ := changelog.Read(cfg, "T-003")

	if len(entries1) == 0 {
		t.Error("T-001 should have changelog entries")
	}
	if len(entries3) == 0 {
		t.Error("T-003 should have changelog entries")
	}
}

func TestFinishListWithEntries(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
	}
	baseDir := filepath.Join(cfg.Root(), "worktrees")
	os.MkdirAll(filepath.Join(baseDir, "T-001", "repo-a"), 0755)
	os.MkdirAll(filepath.Join(baseDir, "T-002", "repo-b"), 0755)

	code := Finish(s, cfg, "", FinishOpts{ListAll: true})
	if code != 0 {
		t.Errorf("Finish --list returned %d, want 0", code)
	}
}

