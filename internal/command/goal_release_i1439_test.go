package command

import (
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// I-1439 defect 3: a goal's lifecycle "active" is NOT a work-item active
// claim. Neither `st release` nor reconcile's stale-active sweep may
// demote an active goal to draft (StartStatus). Goals are managed by
// `st goal activate/drop/mark-met`. Regression for the clobber that left
// every operator-activated goal silently reverted to draft on the next
// session-start reconcile.

func TestReconcileStaleActiveSkipsGoals(t *testing.T) {
	// setupTestEnvWithGoal seeds an ACTIVE goal G-TEST with an old
	// last_touched (2026-03) and no worktree/PR — i.e. exactly the
	// "stale active" shape the sweep releases for work items.
	s, cfg := setupTestEnvWithGoal(t, true)

	opts := ReconcileOpts{PRFetch: func(_ *config.Config, _ string) (string, []string) { return "", nil }}
	reconcileStaleActive(s, cfg, opts)

	final, ok := newStoreOrFail(t, cfg).Get("G-TEST")
	if !ok {
		t.Fatal("G-TEST disappeared")
	}
	if final.Status != "active" {
		t.Errorf("stale-active sweep must NOT touch goals; G-TEST status = %q, want \"active\"", final.Status)
	}
}

func TestReleaseRefusesGoal(t *testing.T) {
	s, cfg := setupTestEnvWithGoal(t, true)

	if code := Release(s, cfg, "G-TEST"); code == 0 {
		t.Error("Release on a goal must refuse (non-zero exit) — goals use st goal drop/mark-met")
	}

	final, ok := newStoreOrFail(t, cfg).Get("G-TEST")
	if !ok {
		t.Fatal("G-TEST disappeared")
	}
	if final.Status != "active" {
		t.Errorf("goal status must be unchanged after a refused release; got %q, want \"active\"", final.Status)
	}
}

func TestStartRefusesGoal(t *testing.T) {
	// Goals are not workable items: `st start` would flip a goal to active
	// and stamp a claim no goal verb can clear (and release now refuses
	// goals). Refuse up front. Use the active fixture — the guard fires
	// for any goal regardless of status, before the start-status check.
	s, cfg := setupTestEnvWithGoal(t, true)

	if code := Start(s, cfg, "G-TEST", StartOpts{}); code == 0 {
		t.Error("Start on a goal must refuse (non-zero exit) — goals use st goal activate")
	}

	final, ok := newStoreOrFail(t, cfg).Get("G-TEST")
	if !ok {
		t.Fatal("G-TEST disappeared")
	}
	if final.AssignedTo != "" || final.ClaimedBy != "" {
		t.Errorf("refused start must not stamp assignment/claim; assigned_to=%q claimed_by=%q", final.AssignedTo, final.ClaimedBy)
	}
}
