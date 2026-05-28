package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// setupGateWorkspace builds the workspace-with-nested-agent-state layout
// needed to exercise the I-807 non-state gate via autoSync. Returns (workspace,
// store, config). The workspace has a tracked non-state file
// (claude-config/hooks/foo.sh) pre-committed on main; modifying it triggers
// the gate.
func setupGateWorkspace(t *testing.T) (workspace string, s *store.Store, cfg *config.Config) {
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

title: Gate test task

depends_on:
- []

sbar:
  situation: |-
    Gate test fixture.
  background: |-
    Used to exercise the I-807 non-state gate.
  assessment: |-
    Gate fires when a non-state file is dirty on main.
  recommendation: |-
    autoSync propagates the gate sentinel error.
`
	if err := os.WriteFile(filepath.Join(itemDir, "tasks", "T-001-gate-test.md"), []byte(taskBody), 0644); err != nil {
		t.Fatal(err)
	}

	// Pre-commit a tracked non-state file. Modifying it later triggers the gate.
	nonStateDir := filepath.Join(workspace, "claude-config", "hooks")
	if err := os.MkdirAll(nonStateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonStateDir, "foo.sh"), []byte("#!/bin/sh\necho original\n"), 0755); err != nil {
		t.Fatal(err)
	}
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
	return workspace, st, cfg
}

// TestAutoSyncGateRefusalReturnsSentinel verifies that autoSync returns
// ErrI807MainBranchGate when the gate fires and emits the full error (not a
// "warning:" downgrade) to stderr.
func TestAutoSyncGateRefusalReturnsSentinel(t *testing.T) {
	workspace, s, _ := setupGateWorkspace(t)

	// Dirty the tracked non-state file to trigger the gate.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified\n"), 0755); err != nil {
		t.Fatal(err)
	}

	var returned error
	stderr := captureStderr(t, func() int {
		returned = autoSync(s, "test: gate sentinel")
		return 0
	})

	if !errors.Is(returned, store.ErrI807MainBranchGate) {
		t.Errorf("autoSync must return ErrI807MainBranchGate on gate refusal; got: %v", returned)
	}
	if strings.HasPrefix(stderr, "warning:") {
		t.Errorf("gate refusal must NOT be downgraded to a warning; stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "foo.sh") {
		t.Errorf("gate error must name the offending file; stderr: %q", stderr)
	}
}

// TestAutoSyncTransientErrorStaysBestEffort verifies that a non-gate GitSync
// failure keeps the best-effort behavior: autoSync returns nil and prints a
// "warning:" line.
func TestAutoSyncTransientErrorStaysBestEffort(t *testing.T) {
	// Use a nil store — autoSync short-circuits before GitSync, returning nil.
	// For a true transient error we need a store whose GitSync fails with a
	// non-sentinel. Inject a broken git dir to provoke a transient error.
	workspace := t.TempDir()
	asDir := filepath.Join(workspace, ".as")
	os.MkdirAll(filepath.Join(asDir), 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	cfg, err := config.LoadFrom(filepath.Join(asDir, "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// AutoCommit=true but no git repo → GitSync will fail with a non-gate error.
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var returned error
	stderr := captureStderr(t, func() int {
		returned = autoSync(s, "test: transient error")
		return 0
	})

	if returned != nil {
		t.Errorf("autoSync must return nil on transient GitSync failure; got: %v", returned)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Errorf("transient error must emit a warning line; stderr: %q", stderr)
	}
	if strings.Contains(stderr, fmt.Sprintf("%v", store.ErrI807MainBranchGate)) {
		t.Errorf("transient error must not contain gate sentinel text; stderr: %q", stderr)
	}
}
