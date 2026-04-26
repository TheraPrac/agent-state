package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

// TestClose_FreezesDurationsAndLOC verifies that st close writes:
//   - time_tracking.total_duration_seconds (closed_at - created_at)
//   - time_tracking.work_duration_seconds (closed_at - started_at)
//   - time_tracking.lines_added / lines_removed / lines_net / files_changed_count
//   - time_tracking.by_repo (one line per configured repo)
//   - work_tracking.files_changed (per-file detail)
//
// Uses fake git so the test is deterministic.
func TestClose_FreezesDurationsAndLOC(t *testing.T) {
	env := testutil.NewEnv(t)

	// Bootstrap T-003 with a started_at roughly 2 hours ago and an older created_at.
	// Created is already set by writeItems to 2026-03-25T12:00:00; we just need started_at.
	item, _ := env.S.Get("T-003")
	startedAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	item.SetNested("time_tracking", "started_at", startedAt)
	if err := env.S.Write(item); err != nil {
		t.Fatalf("seeding started_at: %v", err)
	}

	// Worktree config + fake git returning a known 3-file diff in one repo.
	tmp := t.TempDir()
	apiDir := filepath.Join(tmp, "T-003", "api")
	if err := os.MkdirAll(filepath.Join(apiDir, ".git"), 0755); err != nil {
		t.Fatalf("fake git dir: %v", err)
	}
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api"},
	}

	resolve := func(cfg *config.Config, id, repo string) string { return apiDir }
	runG := func(dir string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(key, "merge-base"):
			return "abc123\n", nil
		case strings.HasPrefix(key, "diff --name-status"):
			return "M\tinternal/foo.go\nA\tinternal/bar_test.go\nD\tinternal/gone.go\n", nil
		case strings.HasPrefix(key, "diff --numstat"):
			return "45\t12\tinternal/foo.go\n30\t0\tinternal/bar_test.go\n0\t87\tinternal/gone.go\n", nil
		}
		return "", fmt.Errorf("unexpected git call: %s", key)
	}

	code := Close(env.S, env.Cfg, "T-003", "completed", CloseOpts{
		Force: true, // bypass gates
		FilesOpts: FilesOpts{
			ResolveRepo: resolve,
			RunGit:      runG,
		},
	})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	// Reload from disk so we read what was persisted, not just in-memory state
	env.Reload(t)
	closed, ok := env.S.Get("T-003")
	if !ok {
		t.Fatal("T-003 missing after close")
	}

	// Duration fields present
	totalDur := readIntField(closed, "time_tracking", "total_duration_seconds")
	if totalDur <= 0 {
		t.Errorf("total_duration_seconds should be > 0, got %d", totalDur)
	}
	workDur := readIntField(closed, "time_tracking", "work_duration_seconds")
	if workDur < 60*60 || workDur > 3*60*60 {
		t.Errorf("work_duration_seconds expected ~2h, got %ds", workDur)
	}

	// LOC aggregates
	assertInt(t, closed, "time_tracking", "lines_added", 75)   // 45 + 30 + 0
	assertInt(t, closed, "time_tracking", "lines_removed", 99) // 12 + 0 + 87
	assertInt(t, closed, "time_tracking", "files_changed_count", 3)

	// by_repo list entry
	raw, err := locateItemFile(env.Root, "T-003")
	if err != nil {
		t.Fatalf("locate item file: %v", err)
	}
	data, _ := os.ReadFile(raw)
	s := string(data)
	if !strings.Contains(s, "by_repo:") {
		t.Errorf("expected by_repo block in item file:\n%s", s)
	}
	if !strings.Contains(s, "api: files=3 added=75 removed=99 net=-24") {
		t.Errorf("by_repo line missing or wrong:\n%s", s)
	}

	// Per-file detail
	if !strings.Contains(s, "files_changed:") {
		t.Errorf("expected files_changed block")
	}
	for _, needle := range []string{
		"api M internal/foo.go +45 -12 (+33) [app]",
		"api A internal/bar_test.go +30 -0 (+30) [test]",
		"api D internal/gone.go +0 -87 (-87) [app]",
	} {
		if !strings.Contains(s, needle) {
			t.Errorf("files_changed missing %q", needle)
		}
	}
}

// TestClose_NoWorktreesWritesZeros verifies that closing an infra-only or
// worktree-less item still succeeds and writes zero LOC fields rather than
// failing the close.
func TestClose_NoWorktreesWritesZeros(t *testing.T) {
	env := testutil.NewEnv(t)
	// No worktree config at all — configuredRepos returns nil → zero totals
	code := Close(env.S, env.Cfg, "T-003", "completed", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("close exit=%d", code)
	}
	env.Reload(t)
	closed, _ := env.S.Get("T-003")
	assertInt(t, closed, "time_tracking", "lines_added", 0)
	assertInt(t, closed, "time_tracking", "files_changed_count", 0)
}

// locateItemFile finds the item's markdown file under root, searching both
// tasks/ and archive/ (post-close).
func locateItemFile(root, id string) (string, error) {
	for _, sub := range []string{"tasks", "issues", "archive"} {
		entries, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), id+"-") || e.Name() == id+".md" {
				return filepath.Join(root, sub, e.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("not found: %s", id)
}
