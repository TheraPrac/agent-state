package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
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
	startedAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "started_at", startedAt)
		return nil
	}); err != nil {
		t.Fatalf("seeding started_at: %v", err)
	}
	item, _ := env.S.Get("T-003")
	_ = item

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

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{
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

// TestClose_DurationMetricsAgree verifies I-514: all four duration fields
// emitted by Close (total_duration_seconds, work_duration_seconds,
// wall_time_hours, total_wall_time) are computed from the same wallDur and
// agree to the second / rounding, regardless of how much queue dwell sat
// between created and started_at.
func TestClose_DurationMetricsAgree(t *testing.T) {
	env := testutil.NewEnv(t)

	// Seed T-003 with started_at much later than created so the previous
	// split-anchor code would have produced wildly divergent values.
	startedAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "started_at", startedAt)
		return nil
	}); err != nil {
		t.Fatalf("seeding started_at: %v", err)
	}

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	env.Reload(t)
	closed, ok := env.S.Get("T-003")
	if !ok {
		t.Fatal("T-003 missing after close")
	}

	totalDur := readIntField(closed, "time_tracking", "total_duration_seconds")
	workDur := readIntField(closed, "time_tracking", "work_duration_seconds")
	if totalDur == 0 || workDur == 0 {
		t.Fatalf("duration fields not written: total=%d work=%d", totalDur, workDur)
	}

	// 1. total_duration_seconds == work_duration_seconds (same anchor).
	if totalDur != workDur {
		t.Errorf("total_duration_seconds (%d) != work_duration_seconds (%d)", totalDur, workDur)
	}

	// 2. wall_time_hours * 3600 agrees with total_duration_seconds within
	//    60s (the "%.1f" rounding window is 0.05h = 180s; 60s is well inside).
	hoursStr, _ := getNestedField(closed, "time_tracking", "wall_time_hours")
	var hours float64
	fmt.Sscanf(hoursStr, "%f", &hours)
	hoursAsSecs := int(hours * 3600)
	diff := hoursAsSecs - totalDur
	if diff < 0 {
		diff = -diff
	}
	if diff > 180 {
		t.Errorf("wall_time_hours (%s = %ds) disagrees with total_duration_seconds (%d) by %ds (>180s rounding window)",
			hoursStr, hoursAsSecs, totalDur, diff)
	}

	// 3. total_wall_time formats back to formatDuration(totalDur seconds).
	wallStr, _ := getNestedField(closed, "time_tracking", "total_wall_time")
	wantStr := formatDuration(time.Duration(totalDur) * time.Second)
	if wallStr != wantStr {
		t.Errorf("total_wall_time = %q, want %q (from total_duration_seconds=%d)", wallStr, wantStr, totalDur)
	}
}

// TestClose_NoWorktreesWritesZeros verifies that closing an infra-only or
// worktree-less item still succeeds and writes zero LOC fields rather than
// failing the close.
func TestClose_NoWorktreesWritesZeros(t *testing.T) {
	env := testutil.NewEnv(t)
	// No worktree config at all — configuredRepos returns nil → zero totals
	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true})
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

// TestClose_DurationAccumulatedPlusElapsed verifies that st close uses
// accumulated_seconds + elapsed(session_started_at) when session fields exist.
func TestClose_DurationAccumulatedPlusElapsed(t *testing.T) {
	env := testutil.NewEnv(t)

	// 300 accumulated seconds from a previous session + 60s live segment
	sessStart := time.Now().Add(-60 * time.Second).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "started_at", time.Now().Add(-2*time.Hour).Format(time.RFC3339))
		it.SetNested("time_tracking", "accumulated_seconds", "300")
		it.SetNested("time_tracking", "session_started_at", sessStart)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	env.Reload(t)
	closed, _ := env.S.Get("T-003")
	total := readIntField(closed, "time_tracking", "total_duration_seconds")
	// Expect ~360s (300 + 60); allow ±5s for clock jitter
	if total < 355 || total > 400 {
		t.Errorf("total_duration_seconds expected ~360 (300+60), got %d", total)
	}
}

// TestClose_DurationMigrationFallback verifies that items with only started_at
// (no session fields) still get a best-effort wall-clock duration, NOT zero.
func TestClose_DurationMigrationFallback(t *testing.T) {
	env := testutil.NewEnv(t)

	startedAt := time.Now().Add(-90 * time.Minute).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "started_at", startedAt)
		// deliberately NOT setting accumulated_seconds or session_started_at
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	env.Reload(t)
	closed, _ := env.S.Get("T-003")
	total := readIntField(closed, "time_tracking", "total_duration_seconds")
	// Migration fallback should produce ~90 minutes, not zero
	if total < 60*60 || total > 2*60*60 {
		t.Errorf("migration fallback: total_duration_seconds expected ~5400 (90m), got %d", total)
	}
}

// TestClose_ClearsSessionStartedAt verifies that st close zeroes session_started_at.
func TestClose_ClearsSessionStartedAt(t *testing.T) {
	env := testutil.NewEnv(t)

	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "session_started_at", time.Now().Format(time.RFC3339))
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true})
	if code != 0 {
		t.Fatalf("Close exit=%d", code)
	}

	env.Reload(t)
	closed, _ := env.S.Get("T-003")
	sessStart, _ := getNestedField(closed, "time_tracking", "session_started_at")
	if sessStart != "" {
		t.Errorf("session_started_at should be cleared after close, got %q", sessStart)
	}
}
