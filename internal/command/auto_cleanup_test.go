package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// --- removeFromStackSilently ---

// TestRemoveFromStackSilentlyMidStack confirms the helper drops a
// mid-stack entry, not just the top. Symmetric with
// removeFromQueueSilently. I-232.
func TestRemoveFromStackSilentlyMidStack(t *testing.T) {
	_, cfg := setupTestEnv(t)
	if err := SaveStack(cfg, []StackEntry{
		{ID: "T-001"},
		{ID: "T-002"}, // <-- target (mid-stack)
		{ID: "T-003"},
	}); err != nil {
		t.Fatalf("seed stack: %v", err)
	}

	removed, err := removeFromStackSilently(cfg, "T-002")
	if err != nil {
		t.Fatalf("removeFromStackSilently: %v", err)
	}
	if !removed {
		t.Errorf("expected removed=true for present mid-stack entry")
	}
	got := LoadStack(cfg)
	if len(got) != 2 || got[0].ID != "T-001" || got[1].ID != "T-003" {
		t.Errorf("post-remove stack = %+v, want [T-001, T-003]", got)
	}
}

// --- Close hooks (I-232) ---

func TestCloseRemovesGhostStackEntry(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-001 is mid-stack (not top). T-002 stays on top.
	if err := SaveStack(cfg, []StackEntry{
		{ID: "T-001"}, // <-- closing this one; not on top
		{ID: "T-002"},
	}); err != nil {
		t.Fatalf("seed stack: %v", err)
	}

	if code := Close(s, cfg, "T-001", "done", CloseOpts{AllowMissingCapture: "test: capture gate not under test", Force: true}); code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	got := LoadStack(cfg)
	if len(got) != 1 || got[0].ID != "T-002" {
		t.Errorf("post-close stack = %+v, want [T-002]", got)
	}
}

func TestCloseClearsStaleBranchField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Configure worktree with a real (empty) git repo. branchExists/
	// remoteBranchExists will both miss the seeded branch, so
	// branchExistsAnywhere returns false and the field gets cleared.
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "worktrees",
		ParentDir: cfg.Root(),
		Repos:     []string{"repo-a"},
	}
	repoDir := filepath.Join(cfg.Root(), "repo-a")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "init", "-q")
	mustGit(t, repoDir, "commit", "-q", "--allow-empty", "-m", "init")

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("work_tracking", "branch", "feat/dead-branch")
		it.SetNested("work_tracking", "worktree", "worktrees/T-001")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if code := Close(s, cfg, "T-001", "done", CloseOpts{AllowMissingCapture: "test: capture gate not under test", Force: true}); code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	// Reload from disk so we read the persisted state.
	freshStore := newStoreOrFail(t, cfg)
	closed, _ := freshStore.Get("T-001")
	if v, _ := getNestedField(closed, "work_tracking", "branch"); v != "" {
		t.Errorf("work_tracking.branch should be cleared, got %q", v)
	}
	if v, _ := getNestedField(closed, "work_tracking", "worktree"); v != "" {
		t.Errorf("work_tracking.worktree should be cleared, got %q", v)
	}
}

func TestClosePreservesActiveBranchField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No worktree config → branchExistsAnywhere returns true (conservative),
	// so the branch field must stay.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("work_tracking", "branch", "feat/still-live")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if code := Close(s, cfg, "T-001", "done", CloseOpts{AllowMissingCapture: "test: capture gate not under test", Force: true}); code != 0 {
		t.Fatalf("Close exit=%d", code)
	}
	freshStore := newStoreOrFail(t, cfg)
	closed, _ := freshStore.Get("T-001")
	if v, _ := getNestedField(closed, "work_tracking", "branch"); v != "feat/still-live" {
		t.Errorf("work_tracking.branch was cleared unexpectedly, got %q", v)
	}
}

// --- Finish hooks (I-232) ---

func TestFinishClearsBranchField(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}

	// Create an empty worktree dir for T-001 so Finish has something to
	// "clean up" without needing a real git invocation.
	wtDir := filepath.Join(cfg.WorktreeBase(), "T-001")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("work_tracking", "branch", "feat/finished")
		it.SetNested("work_tracking", "worktree", "worktrees/T-001")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if code := Finish(s, cfg, "T-001", FinishOpts{}); code != 0 {
		t.Fatalf("Finish exit=%d", code)
	}

	freshStore := newStoreOrFail(t, cfg)
	finished, _ := freshStore.Get("T-001")
	if v, _ := getNestedField(finished, "work_tracking", "branch"); v != "" {
		t.Errorf("work_tracking.branch should be cleared, got %q", v)
	}
}

func TestFinishResetsActiveStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}
	wtDir := filepath.Join(cfg.WorktreeBase(), "T-003")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}

	// T-003 is already active in setupTestEnv. After finish, it should
	// reset to "queued" (the type's start status).
	if code := Finish(s, cfg, "T-003", FinishOpts{}); code != 0 {
		t.Fatalf("Finish exit=%d", code)
	}

	freshStore := newStoreOrFail(t, cfg)
	finished, _ := freshStore.Get("T-003")
	if finished.Status != "queued" {
		t.Errorf("active item should reset to queued, got %q", finished.Status)
	}
}

func TestFinishPreservesTerminalStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"repo-a"},
	}
	wtDir := filepath.Join(cfg.WorktreeBase(), "T-001")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "done"
		it.Doc.SetField("status", "done")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if code := Finish(s, cfg, "T-001", FinishOpts{}); code != 0 {
		t.Fatalf("Finish exit=%d", code)
	}

	freshStore := newStoreOrFail(t, cfg)
	finished, _ := freshStore.Get("T-001")
	if finished.Status != "done" {
		t.Errorf("terminal status should be preserved, got %q", finished.Status)
	}
}

// --- Reconcile cleanup phases (I-232) ---

func TestReconcileQueueCleanupDropsTerminal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "done"
		it.Doc.SetField("status", "done")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveQueue(cfg, []QueueEntry{
		{ID: "T-001"}, // terminal — should drop
		{ID: "T-002"}, // queued — should stay
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	freshStore := newStoreOrFail(t, cfg)

	opts := ReconcileOpts{}
	n := reconcileQueueCleanup(freshStore, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 drop, got %d", n)
	}
	got := LoadQueue(cfg)
	if len(got) != 1 || got[0].ID != "T-002" {
		t.Errorf("post-cleanup queue = %+v, want [T-002]", got)
	}
}

func TestReconcileStackCleanupDropsTerminal(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "done"
		it.Doc.SetField("status", "done")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveStack(cfg, []StackEntry{
		{ID: "T-002"}, // active — keep
		{ID: "T-001"}, // terminal — drop
	}); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	freshStore := newStoreOrFail(t, cfg)

	n := reconcileStackCleanup(freshStore, cfg, ReconcileOpts{})
	if n != 1 {
		t.Errorf("expected 1 drop, got %d", n)
	}
	got := LoadStack(cfg)
	if len(got) != 1 || got[0].ID != "T-002" {
		t.Errorf("post-cleanup stack = %+v, want [T-002]", got)
	}
}

func TestReconcileStaleActiveReleasesIdleItem(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// Set last_touched directly on disk — Mutate stamps last_touched to
	// "now" automatically, so we can't go through it for backdating.
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)

	freshStore := newStoreOrFail(t, cfg)
	opts := ReconcileOpts{PRFetch: func(_ *config.Config, _ string) (string, []string) { return "", nil }}
	n := reconcileStaleActive(freshStore, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 release, got %d", n)
	}

	// After release, T-003 should be back at queued.
	finalStore := newStoreOrFail(t, cfg)
	released, _ := finalStore.Get("T-003")
	if released.Status != "queued" {
		t.Errorf("released item should be queued, got %q", released.Status)
	}
}

func TestReconcileStaleActiveSkipsRecentlyTouched(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// T-003 active with a recent touch (1 hour ago) — keep.
	staleLastTouched(t, cfg, "T-003", time.Hour)

	freshStore := newStoreOrFail(t, cfg)
	opts := ReconcileOpts{PRFetch: func(_ *config.Config, _ string) (string, []string) { return "", nil }}
	n := reconcileStaleActive(freshStore, cfg, opts)
	if n != 0 {
		t.Errorf("recent touch should NOT release, got %d releases", n)
	}
}

// I-1485: a CLEAN worktree from a non-live owner is just a husk and must be released
// (previously any worktree-on-disk pinned the item active forever — the I-1470 case).
func TestReconcileStaleActiveReleasesCleanWorktreeHusk(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(cfg.WorktreeBase(), "T-003"), 0755); err != nil {
		t.Fatal(err)
	}

	freshStore := newStoreOrFail(t, cfg)
	opts := ReconcileOpts{
		PRFetch:         func(_ *config.Config, _ string) (string, []string) { return "", nil },
		WorktreeUnsaved: func(_ *config.Config, _ string) bool { return false }, // clean husk
	}
	n := reconcileStaleActive(freshStore, cfg, opts)
	if n != 1 {
		t.Errorf("clean worktree husk from a non-live owner should release, got %d releases", n)
	}
}

// I-1485: a worktree holding unsaved work (uncommitted or unpushed) must NOT be reset —
// keep the item active for operator review so nothing is silently stranded.
func TestReconcileStaleActiveKeepsDirtyWorktree(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(cfg.WorktreeBase(), "T-003"), 0755); err != nil {
		t.Fatal(err)
	}

	freshStore := newStoreOrFail(t, cfg)
	opts := ReconcileOpts{
		PRFetch:         func(_ *config.Config, _ string) (string, []string) { return "", nil },
		WorktreeUnsaved: func(_ *config.Config, _ string) bool { return true }, // unsaved work present
	}
	n := reconcileStaleActive(freshStore, cfg, opts)
	if n != 0 {
		t.Errorf("worktree with unsaved work should NOT release, got %d releases", n)
	}
}

func TestReconcileStaleActiveSkipsOpenPR(t *testing.T) {
	s, cfg := setupTestEnv(t)
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("work_tracking", "branch", "feat/open-pr")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Mutate above overwrote last_touched to now — re-backdate.
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)

	freshStore := newStoreOrFail(t, cfg)
	opts := ReconcileOpts{PRFetch: func(_ *config.Config, branch string) (string, []string) {
		if branch == "feat/open-pr" {
			return "OPEN", []string{"https://github.com/org/repo/pull/1"}
		}
		return "", nil
	}}
	n := reconcileStaleActive(freshStore, cfg, opts)
	if n != 0 {
		t.Errorf("open PR should NOT release, got %d releases", n)
	}
}

func TestReconcileDryRunSkipsCleanupWrites(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Status = "done"
		it.Doc.SetField("status", "done")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveQueue(cfg, []QueueEntry{{ID: "T-001"}}); err != nil {
		t.Fatal(err)
	}

	freshStore := newStoreOrFail(t, cfg)
	n := reconcileQueueCleanup(freshStore, cfg, ReconcileOpts{DryRun: true})
	if n != 1 {
		t.Errorf("dry-run should still report 1 drop, got %d", n)
	}
	// Queue file should be unchanged.
	got := LoadQueue(cfg)
	if len(got) != 1 || got[0].ID != "T-001" {
		t.Errorf("dry-run should NOT mutate queue file, got %+v", got)
	}
}

// --- Release hook (I-232) ---

func TestReleaseRemovesFromStack(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-003 is active + assigned. Push it on the stack, then release.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.AssignedTo = "agent-x"
		it.Doc.SetField("assigned_to", "agent-x")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveStack(cfg, []StackEntry{{ID: "T-003"}}); err != nil {
		t.Fatal(err)
	}

	if code := Release(s, cfg, "T-003"); code != 0 {
		t.Fatalf("Release exit=%d", code)
	}

	got := LoadStack(cfg)
	if len(got) != 0 {
		t.Errorf("post-release stack = %+v, want []", got)
	}
}

// --- helpers ---

func newStoreOrFail(t *testing.T, cfg *config.Config) *store.Store {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// staleLastTouched rewrites the item file's last_touched field to
// (now - age) directly on disk, bypassing Mutate (which always stamps
// last_touched to now via store/mutate.go:284). Used by stale-active
// reconcile tests to backdate items.
func staleLastTouched(t *testing.T, cfg *config.Config, id string, age time.Duration) {
	t.Helper()
	// Look in tasks/, issues/, archive/.
	var path string
	for _, sub := range []string{"tasks", "issues", "archive"} {
		dir := filepath.Join(cfg.Root(), sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name() == id+".md" || filepath.Ext(e.Name()) == ".md" && len(e.Name()) > len(id) && e.Name()[:len(id)+1] == id+"-" {
				path = filepath.Join(dir, e.Name())
				break
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		t.Fatalf("could not locate file for %s", id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	target := time.Now().Add(-age).Format(time.RFC3339)
	var out []byte
	saw := false
	for _, line := range splitFileLines(string(data)) {
		if !saw && len(line) > 14 && line[:14] == "last_touched: " {
			out = append(out, []byte("last_touched: "+target+"\n")...)
			saw = true
			continue
		}
		out = append(out, []byte(line+"\n")...)
	}
	if !saw {
		// No last_touched line — append one in the header area.
		out = append([]byte("last_touched: "+target+"\n"), out...)
	}
	if err := os.WriteFile(path, out, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func splitFileLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// mustGit runs git with the given args in dir, t.Fatal on failure.
// Used by close-stale-branch tests to set up a real (but minimal) git
// repo so branchExists / remoteBranchExists can run.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
