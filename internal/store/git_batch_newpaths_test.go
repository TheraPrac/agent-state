package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// TestGitSyncBatchesNewPathsAddCalls is the I-1720 regression: GitSync's
// explicit-newPaths staging loop must issue O(1) `git add` subprocess
// invocations for a bulk batch, not one per path. reconcileArchive (st
// reconcile Phase 5) can pass dozens-to-hundreds of paths in a single
// GitSync call on a workspace with a long terminal-item backlog; before
// I-1720, each path spawned its own `git add` subprocess (fork/exec + git
// repo-open overhead per path).
//
// Verified by shimming `git` on PATH with a wrapper that counts `add`
// invocations before delegating to the real binary.
func TestGitSyncBatchesNewPathsAddCalls(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	if err := os.MkdirAll(asDir, 0755); err != nil {
		t.Fatalf("mkdir .as: %v", err)
	}
	if err := os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	initGitRepo(t, root)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git not found on PATH: %v", err)
	}
	counterPath := filepath.Join(t.TempDir(), "add-calls.count")
	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" = \"add\" ]; then echo x >> \"" + counterPath + "\"; fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const n = 150
	newPaths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(root, "issues", fmt.Sprintf("I-%03d-bulk.md", i))
		content := fmt.Sprintf("id: I-%03d\ntype: issue\nstatus: queued\ntitle: bulk\n", i)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatalf("write bulk item %d: %v", i, err)
		}
		newPaths = append(newPaths, p)
	}

	if err := s.GitSync("bulk reconcile stage", newPaths...); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	// All n files must actually land in the commit — batching must not
	// drop any path.
	out, err := gitOutput(root, "show", "--stat", "--name-only", "HEAD")
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("I-%03d-bulk.md", i)
		if !strings.Contains(out, want) {
			t.Errorf("commit missing bulk path %s", want)
		}
	}

	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read add-call counter: %v", err)
	}
	calls := strings.Count(string(data), "x")
	// GitSync issues a handful of unrelated `add` calls of its own (add -u,
	// .plans staging, canonical .as staging) regardless of newPaths size.
	// The bound here is on the ORDER of calls, not an exact count: O(1)
	// batching keeps total calls small no matter how large n is, where
	// pre-I-1720 one-per-path staging would have made this scale with n
	// (150 additional `git add` calls for newPaths alone).
	const maxExpectedAddCalls = 5
	if calls > maxExpectedAddCalls {
		t.Errorf("GitSync issued %d `git add` subprocess calls staging %d newPaths — expected O(1) batching (<=%d), got O(n) one-per-path behavior", calls, n, maxExpectedAddCalls)
	}
}
