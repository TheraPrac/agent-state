package command

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/testutil"
)

// setupCheckGitEnv creates a git-backed test env for Check tests.
// It writes an item with legacy status "open" (alias for "queued") so
// Fix() triggers fixLegacyAliases and actually mutates the working tree,
// giving GitSync something to commit.
func setupCheckGitEnv(t *testing.T) (*testutil.Env, func(...string)) {
	t.Helper()
	env := testutil.NewGitEnv(t)
	root := env.Cfg.Root()

	// Write an item with a legacy status that Fix will rewrite.
	testutil.WriteItem(t, filepath.Join(root, "tasks", "T-999-legacy.md"), `id: T-999
type: task
status: open
created: 2026-01-01T00:00:00Z
last_touched: 2026-01-01T00:00:00Z
title: Legacy status item
`)
	env.Reload(t)

	// Commit the item so the next commit from GitSync is distinguishable.
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-m", "add T-999 (legacy status)")

	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	return env, run
}

func TestCheckFixGitSyncsAfterMutate(t *testing.T) {
	env, _ := setupCheckGitEnv(t)
	root := env.Cfg.Root()

	// Check may return 1 for validation issues unrelated to GitSync — that's OK.
	// The contract here is that GitSync fired and created a commit.
	Check(env.S, env.Cfg, false, true)

	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "st check --fix") {
		t.Errorf("HEAD commit = %q, want prefix 'st check --fix'", strings.TrimSpace(string(out)))
	}
	if trackedDirty(t, root) {
		t.Error("tracked files dirty after Check — GitSync must commit all modifications")
	}
}

func TestCheckQuietDoesNotSync(t *testing.T) {
	env, _ := setupCheckGitEnv(t)
	root := env.Cfg.Root()

	// Record HEAD before quiet check.
	shaBefore, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()

	Check(env.S, env.Cfg, true, true)

	shaAfter, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(shaBefore)) != strings.TrimSpace(string(shaAfter)) {
		t.Errorf("quiet Check must not create a new commit: before=%s after=%s",
			strings.TrimSpace(string(shaBefore)), strings.TrimSpace(string(shaAfter)))
	}
}

func TestCheckNoMutationDoesNotSync(t *testing.T) {
	// After all fixes have been applied, a second non-quiet Check must not
	// produce a new commit. The guard `fixed > 0 || dupFixed > 0` must
	// short-circuit GitSync when nothing was mutated.
	env := testutil.NewGitEnv(t)
	env.Cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	root := env.Cfg.Root()

	// First pass: apply all pending fixes and commit them.
	Check(env.S, env.Cfg, false, false)
	env.Reload(t)

	shaBefore, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()

	// Second pass on the already-fixed workspace: no mutations → no new commit.
	Check(env.S, env.Cfg, false, false)

	shaAfter, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(shaBefore)) != strings.TrimSpace(string(shaAfter)) {
		t.Errorf("already-fixed Check must not create a new commit: before=%s after=%s",
			strings.TrimSpace(string(shaBefore)), strings.TrimSpace(string(shaAfter)))
	}
}
