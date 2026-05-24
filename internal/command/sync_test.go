package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupSyncWorkspace creates the nested workspace + git repo needed to exercise
// the non-state gate via command.Sync. Returns (workspace, store).
func setupSyncWorkspace(t *testing.T) (workspace string, s *store.Store) {
	t.Helper()
	workspace = t.TempDir()
	itemDir := filepath.Join(workspace, "agent-state")
	for _, dir := range []string{"tasks", "issues", "archive"} {
		if err := os.MkdirAll(filepath.Join(itemDir, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	asDir := filepath.Join(workspace, ".as")
	if err := os.MkdirAll(asDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644); err != nil {
		t.Fatal(err)
	}
	taskBody := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Sync test task

depends_on:
- []
`
	if err := os.WriteFile(filepath.Join(itemDir, "tasks", "T-001-sync-test.md"), []byte(taskBody), 0644); err != nil {
		t.Fatal(err)
	}

	// Pre-commit a tracked non-state file so later edits show up as tracked-
	// modified (XY = " M") rather than untracked (??), which the gate skips.
	nonStateDir := filepath.Join(workspace, "claude-config", "hooks")
	if err := os.MkdirAll(nonStateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonStateDir, "foo.sh"), []byte("#!/bin/sh\necho original\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// .gitignore mirrors production so the lock file isn't committed.
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("**/.st-git.lock\n"), 0644); err != nil {
		t.Fatal(err)
	}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		env := os.Environ()
		filtered := make([]string, 0, len(env)+2)
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_DIR=") ||
				strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
				strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
				continue
			}
			filtered = append(filtered, kv)
		}
		filtered = append(filtered,
			"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
			"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
		)
		cmd.Env = filtered
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	git("add", "-A")
	git("commit", "-m", "initial")
	git("branch", "-M", "main")

	cfg, err := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	st, err := store.New(cfg)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return workspace, st
}

// TestSync_AllowNonStateFlag_BypassesGate verifies that Sync(..., allowNonState=true)
// bypasses the I-765 non-state gate on a feature branch with a dirty tracked
// non-state file, returning exit code 0. Also verifies the env var is cleaned
// up after the call (defer Unsetenv worked).
func TestSync_AllowNonStateFlag_BypassesGate(t *testing.T) {
	workspace, s := setupSyncWorkspace(t)

	// Switch to a feature branch.
	cmd := exec.Command("git", "checkout", "-b", "fix/I-765-command-test")
	cmd.Dir = workspace
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}

	// Dirty the tracked non-state file.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified-by-command-test\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// Precondition: gate must refuse without the flag.
	if code := Sync(s, "pre-check", false); code == 0 {
		t.Fatal("precondition failed: Sync without allowNonState must refuse on dirty feature branch")
	}

	// With allowNonState=true the gate must be bypassed.
	if code := Sync(s, "as: sync agent-state (test)", true); code != 0 {
		t.Fatalf("Sync(..., allowNonState=true) must return 0; got %d", code)
	}

	// Env var must be cleaned up after the call.
	if v := os.Getenv("ST_SYNC_ALLOW_NON_STATE"); v != "" {
		t.Errorf("ST_SYNC_ALLOW_NON_STATE must be unset after Sync returns; got %q", v)
	}
}
