package command

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

// seedSessionStartedAt writes session_started_at on item id, t seconds in the past.
func seedSessionStartedAt(t *testing.T, env *testutil.Env, id string, secsAgo int) {
	t.Helper()
	ts := time.Now().Add(-time.Duration(secsAgo) * time.Second).Format(time.RFC3339)
	if err := env.S.Mutate(id, func(it *model.Item) error {
		it.SetNested("time_tracking", "session_started_at", ts)
		return nil
	}); err != nil {
		t.Fatalf("seed session_started_at on %s: %v", id, err)
	}
}

// readTTField reads a string value from time_tracking on item id. Returns "".
func readTTField(t *testing.T, env *testutil.Env, id, key string) string {
	t.Helper()
	env.Reload(t)
	item, ok := env.S.Get(id)
	if !ok {
		t.Fatalf("item %s not found", id)
	}
	v, _ := getNestedField(item, "time_tracking", key)
	return v
}

func TestTimerPauseAllFlushesElapsed(t *testing.T) {
	env := testutil.NewEnv(t)
	// T-003 is active and assigned to agent-a in testutil
	seedSessionStartedAt(t, env, "T-003", 120) // 2 minutes ago

	n, err := TimerPauseAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerPauseAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item paused, got %d", n)
	}

	acc := readTTField(t, env, "T-003", "accumulated_seconds")
	if acc == "" {
		t.Fatal("accumulated_seconds not written")
	}
	secs, _ := strconv.Atoi(acc)
	if secs < 100 || secs > 200 {
		t.Errorf("accumulated_seconds expected ~120, got %d", secs)
	}

	sessStart := readTTField(t, env, "T-003", "session_started_at")
	if sessStart != "" {
		t.Errorf("session_started_at should be cleared, got %q", sessStart)
	}
}

func TestTimerPauseAllNoActiveItems(t *testing.T) {
	env := testutil.NewEnv(t)
	// No session_started_at set on T-003 — timer is already paused

	n, err := TimerPauseAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerPauseAll: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 items paused (no live timer), got %d", n)
	}
}

func TestTimerPauseAllOnlyOwnAgent(t *testing.T) {
	env := testutil.NewEnv(t)
	// Assign T-003 to a different agent
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.AssignedTo = "agent-b"
		it.Doc.SetField("assigned_to", "agent-b")
		return nil
	}); err != nil {
		t.Fatalf("reassign T-003: %v", err)
	}
	seedSessionStartedAt(t, env, "T-003", 60)

	n, err := TimerPauseAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerPauseAll: %v", err)
	}
	if n != 0 {
		t.Errorf("should not touch peer agent's item, got n=%d", n)
	}
	// session_started_at should still be set on T-003
	sessStart := readTTField(t, env, "T-003", "session_started_at")
	if sessStart == "" {
		t.Error("peer agent's session_started_at was incorrectly cleared")
	}
}

func TestTimerResumeAllSetsSessionStart(t *testing.T) {
	env := testutil.NewEnv(t)
	// T-003 active, assigned agent-a, no session_started_at

	n, err := TimerResumeAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerResumeAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item resumed, got %d", n)
	}

	sessStart := readTTField(t, env, "T-003", "session_started_at")
	if sessStart == "" {
		t.Fatal("session_started_at should be set after resume")
	}
	// Should parse as a valid RFC3339 time
	if _, err := time.Parse(time.RFC3339, sessStart); err != nil {
		t.Errorf("session_started_at %q is not valid RFC3339: %v", sessStart, err)
	}
}

func TestTimerResumeAllIdempotent(t *testing.T) {
	env := testutil.NewEnv(t)
	seedSessionStartedAt(t, env, "T-003", 300) // 5 minutes ago

	firstVal := readTTField(t, env, "T-003", "session_started_at")

	n, err := TimerResumeAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerResumeAll: %v", err)
	}
	if n != 0 {
		t.Errorf("already-running timer should not be touched, got n=%d", n)
	}

	// session_started_at must be unchanged
	afterVal := readTTField(t, env, "T-003", "session_started_at")
	if afterVal != firstVal {
		t.Errorf("session_started_at changed from %q to %q — double-count risk", firstVal, afterVal)
	}
}

func TestTimerPauseClockSkewGuard(t *testing.T) {
	env := testutil.NewEnv(t)
	// Set session_started_at 60 seconds in the FUTURE (clock skew)
	futureTS := time.Now().Add(60 * time.Second).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "session_started_at", futureTS)
		return nil
	}); err != nil {
		t.Fatalf("seed future session_started_at: %v", err)
	}

	n, err := TimerPauseAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerPauseAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item processed, got %d", n)
	}

	acc := readTTField(t, env, "T-003", "accumulated_seconds")
	secs := 0
	if acc != "" {
		fmt.Sscanf(acc, "%d", &secs)
	}
	if secs < 0 {
		t.Errorf("accumulated_seconds went negative (%d) — clock-skew guard failed", secs)
	}
	// Should be 0 (clamp) not negative
	if secs > 5 {
		t.Errorf("accumulated_seconds should be ~0 for future start, got %d", secs)
	}
}

func TestTimerScrubRemovesWallClockFallback(t *testing.T) {
	env := testutil.NewEnv(t)
	// Wall-clock-contaminated item: work_duration set, no accumulated_seconds.
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "work_duration_seconds", "187056") // 51.96h, I-925 class
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := TimerScrub(env.S, env.Cfg, false)
	if err != nil {
		t.Fatalf("TimerScrub: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item scrubbed, got %d", n)
	}
	if v := readTTField(t, env, "T-003", "work_duration_seconds"); v != "" {
		t.Errorf("work_duration_seconds should be removed, got %q", v)
	}
}

func TestTimerScrubKeepsMeasuredValues(t *testing.T) {
	env := testutil.NewEnv(t)
	// Measured item: accumulated_seconds present alongside work_duration.
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "accumulated_seconds", "9374")
		it.SetNested("time_tracking", "work_duration_seconds", "9374")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := TimerScrub(env.S, env.Cfg, false)
	if err != nil {
		t.Fatalf("TimerScrub: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 items scrubbed, got %d", n)
	}
	if v := readTTField(t, env, "T-003", "work_duration_seconds"); v != "9374" {
		t.Errorf("measured work_duration_seconds should survive scrub, got %q", v)
	}
}

func TestTimerScrubDryRunChangesNothing(t *testing.T) {
	env := testutil.NewEnv(t)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "work_duration_seconds", "187056")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := TimerScrub(env.S, env.Cfg, true)
	if err != nil {
		t.Fatalf("TimerScrub dry-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("dry-run should report 1 candidate, got %d", n)
	}
	if v := readTTField(t, env, "T-003", "work_duration_seconds"); v != "187056" {
		t.Errorf("dry-run must not modify the item, got %q", v)
	}
}

func TestTimerPauseSanityCapStaleEpoch(t *testing.T) {
	env := testutil.NewEnv(t)
	// Simulate a stale session_started_at 48 hours ago (e.g., write-race residue).
	stale := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "session_started_at", stale)
		return nil
	}); err != nil {
		t.Fatalf("seed stale session_started_at: %v", err)
	}

	_, err := TimerPauseAll(env.S, env.Cfg, "agent-a")
	if err != nil {
		t.Fatalf("TimerPauseAll: %v", err)
	}

	acc := readTTField(t, env, "T-003", "accumulated_seconds")
	var secs int
	fmt.Sscanf(acc, "%d", &secs)

	// Without the cap this would be ~172800s (48h). With the cap it must be ≤ 43200 (12h).
	const maxExpected = 12 * 3600
	if secs > maxExpected {
		t.Errorf("sanity cap failed: accumulated_seconds=%d exceeds 12h cap (%d)", secs, maxExpected)
	}
	if secs <= 0 {
		t.Errorf("accumulated_seconds should be > 0 (capped, not zeroed), got %d", secs)
	}
}

func TestTimerScrubRatioFlagsContamination(t *testing.T) {
	env := testutil.NewEnv(t)
	// Seed an item with accumulated_seconds=314710 (87.4h) and wall_time_hours=90.6 —
	// the exact I-1530 shape: ratio=96.5%, clearly contaminated.
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "accumulated_seconds", "314710")
		it.SetNested("time_tracking", "work_duration_seconds", "314710")
		it.SetNested("time_tracking", "wall_time_hours", "90.6")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Warn-only (autoNull=false) — must flag but not modify.
	n, err := TimerScrubRatio(env.S, false, false)
	if err != nil {
		t.Fatalf("TimerScrubRatio warn-only: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item flagged, got %d", n)
	}
	if v := readTTField(t, env, "T-003", "accumulated_seconds"); v != "314710" {
		t.Errorf("warn-only must not modify the item, got %q", v)
	}

	// Auto-null (autoNull=true) — must remove both fields.
	n, err = TimerScrubRatio(env.S, false, true)
	if err != nil {
		t.Fatalf("TimerScrubRatio auto-null: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item flagged in auto-null run, got %d", n)
	}
	if v := readTTField(t, env, "T-003", "accumulated_seconds"); v != "" {
		t.Errorf("auto-null should remove accumulated_seconds, got %q", v)
	}
}

func TestTimerScrubRatioKeepsLegitimate(t *testing.T) {
	env := testutil.NewEnv(t)
	// I-1538 shape: 16170s (4.5h) out of 75.9h wall time — ratio=5.9%, legitimate.
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "accumulated_seconds", "16170")
		it.SetNested("time_tracking", "work_duration_seconds", "16170")
		it.SetNested("time_tracking", "wall_time_hours", "75.9")
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := TimerScrubRatio(env.S, false, false)
	if err != nil {
		t.Fatalf("TimerScrubRatio: %v", err)
	}
	if n != 0 {
		t.Errorf("legitimate item should not be flagged, got n=%d", n)
	}
}
