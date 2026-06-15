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

	// Modify and stage the tracked non-state file to trigger the gate.
	// I-1472: gate fires only on staged (index-dirty) entries.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho modified\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "claude-config/hooks/foo.sh").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
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

// setupPushWorkspace creates a bare remote, clones it, and sets up the
// agent-state item directory inside the clone. Returns (bareDir, cloneDir, store, cfg).
// cfg has AutoCommit=true, AutoPush=true.
func setupPushWorkspace(t *testing.T) (bareDir, cloneDir string, s *store.Store, cfg *config.Config) {
	t.Helper()
	base := t.TempDir()

	bareDir = filepath.Join(base, "origin.git")
	cloneDir = filepath.Join(base, "clone")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		env := make([]string, 0, len(os.Environ())+3)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "GIT_DIR=") ||
				strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
				strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env,
			"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
			"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
		)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}

	// Init bare origin.
	git(base, "init", "--bare", bareDir)
	git(bareDir, "config", "receive.denyNonFastForwards", "false")

	// Seed the bare repo via a temp clone — bare repos can't commit directly.
	seedDir := filepath.Join(base, "seed")
	git(base, "clone", bareDir, seedDir)
	for _, dir := range []string{"agent-state/tasks", "agent-state/issues", "agent-state/archive"} {
		if err := os.MkdirAll(filepath.Join(seedDir, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(seedDir, ".as"), 0755); err != nil {
		t.Fatalf("mkdir .as: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, ".as", "config.yaml"), []byte("paths:\n  root: agent-state\n"), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	itemBody := `id: T-001
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Push test task

depends_on:
- []

sbar:
  situation: Push test fixture.
  background: Used to test push-error scenarios in autoSync.
  assessment: Task exists to be modified by test scenarios.
  recommendation: Keep fixture stable.
`
	if err := os.WriteFile(filepath.Join(seedDir, "agent-state/tasks", "T-001-push-test.md"), []byte(itemBody), 0644); err != nil {
		t.Fatalf("write T-001: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, ".gitignore"), []byte("**/.st-git.lock\n"), 0644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	git(seedDir, "config", "user.email", "test@test.com")
	git(seedDir, "config", "user.name", "Test")
	git(seedDir, "add", "-A")
	git(seedDir, "commit", "-m", "initial")
	git(seedDir, "branch", "-M", "main")
	git(seedDir, "push", "-u", "origin", "main")

	// Clone for our agent.
	git(base, "clone", bareDir, cloneDir)
	git(cloneDir, "config", "user.email", "agent@test.com")
	git(cloneDir, "config", "user.name", "Agent")

	cfg, err := config.LoadFrom(filepath.Join(cloneDir, ".as", "config.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}
	s, err = store.New(cfg)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return bareDir, cloneDir, s, cfg
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

// TestAutoSyncPushRejectedByServerGateMessage verifies that autoSync emits the
// actionable "push blocked by a server-side gate" message (not generic
// "run `st sync` manually") when ErrPushRejectedButOriginUnchanged is returned.
func TestAutoSyncPushRejectedByServerGateMessage(t *testing.T) {
	bareDir, cloneDir, s, _ := setupPushWorkspace(t)

	// Install a pre-receive hook in the bare origin that rejects every push.
	hooksDir := filepath.Join(bareDir, "hooks")
	os.MkdirAll(hooksDir, 0755)
	hook := filepath.Join(hooksDir, "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0755)

	// Dirty a tracked file so GitSync has something to commit and push.
	itemPath := filepath.Join(cloneDir, "agent-state/tasks", "T-001-push-test.md")
	existing, _ := os.ReadFile(itemPath)
	os.WriteFile(itemPath, append(existing, []byte("# modified\n")...), 0644)

	var returned error
	stderr := captureStderr(t, func() int {
		returned = autoSync(s, "test: push rejected")
		return 0
	})

	if returned != nil {
		t.Errorf("autoSync must return nil for ErrPushRejectedButOriginUnchanged; got: %v", returned)
	}
	if !strings.Contains(stderr, "push blocked by a server-side gate") {
		t.Errorf("expected server-gate message; stderr: %q", stderr)
	}
	if strings.Contains(stderr, "run `st sync` manually") {
		t.Errorf("generic fallthrough message must not appear for server-gate error; stderr: %q", stderr)
	}
}

// TestAutoSyncPushDivergedMessage verifies that autoSync emits the actionable
// "push diverged — a peer changed the same file(s)" message when ErrPushDiverged
// is returned (peer and agent both modified the same file).
func TestAutoSyncPushDivergedMessage(t *testing.T) {
	bareDir, cloneDir, s, _ := setupPushWorkspace(t)
	base := filepath.Dir(bareDir)
	peerDir := filepath.Join(base, "peer")

	// gitAt runs a git command in the given directory.
	gitAt := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		env := make([]string, 0, len(os.Environ())+2)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "GIT_DIR=") ||
				strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
				strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, "GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00", "GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}

	// Peer clone: clone from bare, modify the same file, push to origin first.
	gitAt(base, "clone", bareDir, peerDir)
	gitAt(peerDir, "config", "user.email", "peer@test.com")
	gitAt(peerDir, "config", "user.name", "Peer")
	peerItem := filepath.Join(peerDir, "agent-state/tasks", "T-001-push-test.md")
	existing, _ := os.ReadFile(peerItem)
	os.WriteFile(peerItem, append(existing, []byte("# peer edit\n")...), 0644)
	gitAt(peerDir, "add", "-u")
	gitAt(peerDir, "commit", "-m", "peer commit")
	gitAt(peerDir, "push", "origin", "main")

	// Agent clone: also modify the SAME file (conflict with peer's commit).
	agentItem := filepath.Join(cloneDir, "agent-state/tasks", "T-001-push-test.md")
	agentExisting, _ := os.ReadFile(agentItem)
	os.WriteFile(agentItem, append(agentExisting, []byte("# agent edit\n")...), 0644)

	var returned error
	stderr := captureStderr(t, func() int {
		returned = autoSync(s, "test: push diverged")
		return 0
	})

	if returned != nil {
		t.Errorf("autoSync must return nil for ErrPushDiverged; got: %v", returned)
	}
	if !strings.Contains(stderr, "push diverged") {
		t.Errorf("expected push-diverged message; stderr: %q", stderr)
	}
	if strings.Contains(stderr, "run `st sync` manually") {
		t.Errorf("generic fallthrough message must not appear for push-diverged error; stderr: %q", stderr)
	}
}
