package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// writeTestWorkinfo writes a .workinfo file into workDir for test use.
func writeTestWorkinfo(t *testing.T, workDir, id, branch string, repos []string) {
	t.Helper()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", workDir, err)
	}
	var b strings.Builder
	b.WriteString("# Worktree metadata\n")
	b.WriteString("name: " + id + "\n")
	b.WriteString("branch: " + branch + "\n")
	b.WriteString("repos:\n")
	for _, r := range repos {
		b.WriteString("  - " + r + "\n")
	}
	path := filepath.Join(workDir, ".workinfo")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// ---- readWorkinfo tests ---------------------------------------------------

// TestReadWorkinfo_Happy — nominal case: name, branch, repos all present.
func TestReadWorkinfo_Happy(t *testing.T) {
	dir := t.TempDir()
	writeTestWorkinfo(t, dir, "T-001", "feat/T-001-foo", []string{"repo-a", "repo-b"})

	wi, err := readWorkinfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wi.ID != "T-001" {
		t.Errorf("ID = %q, want T-001", wi.ID)
	}
	if wi.Branch != "feat/T-001-foo" {
		t.Errorf("Branch = %q, want feat/T-001-foo", wi.Branch)
	}
	if len(wi.Repos) != 2 || wi.Repos[0] != "repo-a" || wi.Repos[1] != "repo-b" {
		t.Errorf("Repos = %v, want [repo-a repo-b]", wi.Repos)
	}
}

// TestReadWorkinfo_MissingFile — missing .workinfo returns an error.
func TestReadWorkinfo_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := readWorkinfo(dir)
	if err == nil {
		t.Fatal("expected error for missing .workinfo, got nil")
	}
}

// TestReadWorkinfo_MissingBranch — file present but no branch: line is an error.
func TestReadWorkinfo_MissingBranch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".workinfo")
	os.WriteFile(path, []byte("name: T-001\nrepos:\n  - repo-a\n"), 0o644)

	_, err := readWorkinfo(dir)
	if err == nil {
		t.Fatal("expected error for missing branch field, got nil")
	}
	if !strings.Contains(err.Error(), "branch") {
		t.Errorf("error should mention 'branch': %v", err)
	}
}

// TestReadWorkinfo_Comments — lines starting with # are ignored.
func TestReadWorkinfo_Comments(t *testing.T) {
	dir := t.TempDir()
	content := "# this is a comment\nname: T-001\nbranch: main\n# another comment\nrepos:\n  - repo-a\n"
	os.WriteFile(filepath.Join(dir, ".workinfo"), []byte(content), 0o644)

	wi, err := readWorkinfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wi.Branch != "main" {
		t.Errorf("Branch = %q, want main", wi.Branch)
	}
	if len(wi.Repos) != 1 || wi.Repos[0] != "repo-a" {
		t.Errorf("Repos = %v, want [repo-a]", wi.Repos)
	}
}

// TestReadWorkinfo_EmptyRepos — repos: section with no entries.
func TestReadWorkinfo_EmptyRepos(t *testing.T) {
	dir := t.TempDir()
	content := "name: T-001\nbranch: feat/x\nrepos:\n"
	os.WriteFile(filepath.Join(dir, ".workinfo"), []byte(content), 0o644)

	wi, err := readWorkinfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wi.Repos) != 0 {
		t.Errorf("Repos = %v, want empty", wi.Repos)
	}
}

// ---- WorktreeAdd guard tests ----------------------------------------------

// TestWorktreeAdd_ItemNotFound — unknown ID returns exit code 1.
func TestWorktreeAdd_ItemNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}

	code := WorktreeAdd(s, cfg, "T-999", "repo-b")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (item not found)", code)
	}
}

// TestWorktreeAdd_ItemNotActive — queued item returns exit code 1.
func TestWorktreeAdd_ItemNotActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}

	// T-001 starts as queued in setupTestEnv
	code := WorktreeAdd(s, cfg, "T-001", "repo-b")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (item not active)", code)
	}
}

// TestWorktreeAdd_WorktreeDisabled — worktree integration off returns exit code 1.
func TestWorktreeAdd_WorktreeDisabled(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// cfg.Worktree is nil — worktree not enabled

	// Start T-001 to make it active first (NoPush to avoid stack side-effects)
	_ = Start(s, cfg, "T-001", StartOpts{NoPush: true})

	code := WorktreeAdd(s, cfg, "T-001", "repo-b")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (worktree disabled)", code)
	}
}

// TestWorktreeAdd_MissingWorkinfo — active item but no .workinfo file returns exit code 1.
func TestWorktreeAdd_MissingWorkinfo(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Start without worktree config so --slug is not required.
	_ = Start(s, cfg, "T-001", StartOpts{NoPush: true})

	// Now enable worktree config; .workinfo won't exist in wtDir.
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}

	code := WorktreeAdd(s, cfg, "T-001", "repo-b")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (missing .workinfo)", code)
	}
}

// TestWorktreeAdd_Idempotent — re-adding an already-registered repo succeeds silently.
func TestWorktreeAdd_Idempotent(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Start without worktree config so --slug is not required.
	_ = Start(s, cfg, "T-001", StartOpts{NoPush: true})

	// Enable worktree config and pre-write .workinfo listing repo-a.
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a"}}
	wtDir := filepath.Join(cfg.WorktreeBase(), "T-001")
	writeTestWorkinfo(t, wtDir, "T-001", "feat/T-001-foo", []string{"repo-a"})

	out := captureStdout(t, func() {
		code := WorktreeAdd(s, cfg, "T-001", "repo-a")
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (idempotent)", code)
		}
	})
	if !strings.Contains(out, "already registered") {
		t.Errorf("expected 'already registered' in output; got:\n%s", out)
	}
}

// TestWorktreeAdd_UpdatesWorkinfo — after a successful add, .workinfo includes the new repo.
// This test doesn't actually provision a git worktree; it stubs provisionSingleRepoWorktree
// indirectly by pre-creating the repo directory so the git commands fail silently and
// the test focuses on the .workinfo update path. Since provisionSingleRepoWorktree returns
// an error when git is unavailable, we test the .workinfo write path via readWorkinfo alone.
func TestWorktreeAdd_WorkinfoParsesNewRepo(t *testing.T) {
	dir := t.TempDir()
	// Simulate a .workinfo with one repo; manually append a second and re-parse.
	writeTestWorkinfo(t, dir, "T-001", "feat/T-001-foo", []string{"repo-a"})

	wi, err := readWorkinfo(dir)
	if err != nil {
		t.Fatalf("initial readWorkinfo: %v", err)
	}

	// Simulate what WorktreeAdd does after provisioning: append + rewrite.
	wi.Repos = append(wi.Repos, "repo-b")
	writeWorkinfo(dir, wi.ID, wi.Branch, wi.Repos)

	wi2, err := readWorkinfo(dir)
	if err != nil {
		t.Fatalf("re-readWorkinfo after update: %v", err)
	}
	if len(wi2.Repos) != 2 {
		t.Errorf("Repos length = %d, want 2; repos=%v", len(wi2.Repos), wi2.Repos)
	}
	if wi2.Repos[1] != "repo-b" {
		t.Errorf("Repos[1] = %q, want repo-b", wi2.Repos[1])
	}
}
