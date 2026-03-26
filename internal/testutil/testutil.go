// Package testutil provides shared test helpers for the as CLI.
// It reduces duplication across test files and provides a temp git repo
// helper that unlocks integration tests for git-dependent operations.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Env bundles a temp directory with a configured store and config.
type Env struct {
	Root string
	Cfg  *config.Config
	S    *store.Store
}

// NewEnv creates a temp directory with standard structure, sample items,
// and a loaded store. The directory is cleaned up when the test ends.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".changelog"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}

	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)

	writeItems(t, root)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	return &Env{Root: root, Cfg: cfg, S: s}
}

// NewGitEnv creates a NewEnv inside an initialized git repo with an
// initial commit. This unlocks tests for GitSync, finish, and other
// git-dependent operations.
func NewGitEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t)

	// Initialize git repo
	git(t, env.Root, "init")
	git(t, env.Root, "config", "user.email", "test@test.com")
	git(t, env.Root, "config", "user.name", "Test")
	git(t, env.Root, "add", "-A")
	git(t, env.Root, "commit", "-m", "initial")

	return env
}

// WriteItem writes a markdown file at the given path.
func WriteItem(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Reload re-scans the store from disk. Use after writing files directly
// (bypassing s.Write) to pick up changes.
func (e *Env) Reload(t *testing.T) {
	t.Helper()
	s, err := store.New(e.Cfg)
	if err != nil {
		t.Fatalf("store.New on reload: %v", err)
	}
	e.S = s
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-03-25T10:00:00-06:00",
		"GIT_COMMITTER_DATE=2026-03-25T10:00:00-06:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeItems(t *testing.T, root string) {
	t.Helper()

	WriteItem(t, filepath.Join(root, "tasks", "T-001-first.md"), `id: T-001
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

	WriteItem(t, filepath.Join(root, "tasks", "T-002-second.md"), `id: T-002
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

	WriteItem(t, filepath.Join(root, "tasks", "T-003-active.md"), `id: T-003
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

	WriteItem(t, filepath.Join(root, "issues", "I-001-bug.md"), `id: I-001
type: issue
status: open
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: A bug
severity: high
`)

	WriteItem(t, filepath.Join(root, "archive", "T-004-done.md"), `id: T-004
type: task
status: completed
created: 2026-03-20T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: 2026-03-25T10:00:00-06:00

title: Done task
`)
}
