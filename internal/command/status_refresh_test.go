package command

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// I-380: status banner + reload behavior. Driven via applyRefreshResult so
// each outcome is exercised directly without spinning up a real git remote.
// The end-to-end RefreshWorkspace path is covered in
// internal/store/git_test.go.

func TestStatusRefreshBannerPulled(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	got := applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshPulled, PulledCount: 3}, &buf)
	if !strings.Contains(buf.String(), "pulled 3 commit(s) from origin") {
		t.Errorf("banner missing pulled-count phrase:\n%s", buf.String())
	}
	if got == nil {
		t.Fatal("returned store is nil")
	}
}

func TestStatusRefreshBannerOffline(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshOffline}, &buf)
	if !strings.Contains(buf.String(), "offline") || !strings.Contains(buf.String(), "last-known-good") {
		t.Errorf("offline banner missing expected phrases:\n%s", buf.String())
	}
}

func TestStatusRefreshBannerDiverged(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshDiverged}, &buf)
	if !strings.Contains(buf.String(), "diverged") {
		t.Errorf("diverged banner missing:\n%s", buf.String())
	}
}

func TestStatusRefreshBannerBlocked(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshBlocked}, &buf)
	if !strings.Contains(buf.String(), "blocked") || !strings.Contains(buf.String(), "commit or stash") {
		t.Errorf("blocked banner missing expected phrases:\n%s", buf.String())
	}
}

// I-430: pure-ahead workspace (local commits not yet pushed) gets its
// own banner separate from RefreshDiverged. RefreshDiverged is the
// scary "you have a real conflict to resolve" surface; RefreshAhead is
// the friendly "you forgot to push, run st sync" nudge.
func TestStatusAheadBanner(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshAhead, AheadCount: 2}, &buf)
	out := buf.String()
	for _, want := range []string{"2 unpushed commit(s)", "st sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("ahead banner missing %q. Got:\n%s", want, out)
		}
	}
	// Should NOT use the alarming "diverged" copy — that's a different state.
	if strings.Contains(out, "diverged") {
		t.Errorf("ahead banner shouldn't mention 'diverged'. Got:\n%s", out)
	}
}

func TestStatusAheadBannerSilentOnZero(t *testing.T) {
	// Defensive: AheadCount = 0 with RefreshAhead is technically malformed
	// (the producer would send RefreshUpToDate instead), but the renderer
	// should still print a sensible (or no) line — not a "0 unpushed" lie.
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshAhead, AheadCount: 0}, &buf)
	if strings.Contains(buf.String(), "0 unpushed") {
		t.Errorf("renderer printed misleading '0 unpushed' on zero AheadCount. Got:\n%s", buf.String())
	}
}

func TestStatusRefreshBannerSilentOnUpToDate(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshUpToDate}, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on up-to-date, got: %q", buf.String())
	}
}

func TestStatusRefreshBannerSilentOnDisabled(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshDisabled}, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected silent on disabled, got: %q", buf.String())
	}
}

func TestStatusNoRefreshFlag(t *testing.T) {
	// With NoRefresh=true, refreshAndReload must return immediately:
	// no banner, no store mutation, regardless of underlying repo state.
	s, cfg := setupTestEnv(t)
	var buf bytes.Buffer
	got := refreshAndReload(s, cfg, true /*skip*/, &buf)
	if buf.Len() != 0 {
		t.Errorf("--no-refresh should suppress banner, got: %q", buf.String())
	}
	if got != s {
		t.Errorf("expected unchanged store pointer when refresh skipped")
	}
}

// Verifies the close-the-loop guarantee: after a Pulled outcome, the
// returned store reflects newly-arrived items on disk. Without the reload,
// the original parsed-from-stale store would render the same.
func TestStatusRefreshReloadsStoreAfterPull(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Sanity: the new item we'll add via "pull" doesn't exist yet.
	if _, ok := s.Get("T-PULL"); ok {
		t.Fatalf("T-PULL unexpectedly present in initial store")
	}

	// Simulate origin advancing: drop a new item file under the cfg root.
	root := cfg.Root()
	if err := os.WriteFile(filepath.Join(root, "tasks", "T-PULL-new.md"), []byte(`id: T-PULL
type: task
status: queued
created: 2026-04-26T20:00:00-06:00
last_touched: 2026-04-26T20:00:00-06:00

completed: null

title: Pulled in by refresh

depends_on:
- []
`), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	got := applyRefreshResult(s, cfg, store.RefreshResult{Outcome: store.RefreshPulled, PulledCount: 1}, &buf)

	if !strings.Contains(buf.String(), "pulled 1 commit") {
		t.Errorf("banner missing:\n%s", buf.String())
	}
	if got == s {
		t.Errorf("store pointer not replaced after RefreshPulled — caller would render stale state")
	}
	if _, ok := got.Get("T-PULL"); !ok {
		t.Errorf("reloaded store missing T-PULL — store.New(cfg) was not called or did not pick up the file")
	}
}

// Sanity: --no-refresh wiring round-trip — a status invocation with the
// flag set should not error out and should NOT print any refresh banner
// even if the underlying refresh would have produced one.
func TestStatusNoRefreshFlagEndToEnd(t *testing.T) {
	// Build a real git-backed config so RefreshWorkspace would actually try
	// to run; --no-refresh must short-circuit before that.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: true}

	// Initialize git but point origin nowhere — refresh would hit Offline.
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "t@t.t"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", filepath.Join(t.TempDir(), "nope.git")},
	} {
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	var buf bytes.Buffer
	refreshAndReload(s, cfg, true /*NoRefresh*/, &buf)
	if buf.Len() != 0 {
		t.Errorf("--no-refresh end-to-end: expected no banner output, got: %q", buf.String())
	}
}
