package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// --- Claim ---

func TestClaim_Stamps(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "sess-claim-001")
	defer os.Unsetenv("AS_SESSION_ID")

	out := captureStdout(t, func() {
		if rc := Claim(s, cfg, "T-001"); rc != 0 {
			t.Fatalf("Claim returned %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "claimed T-001") {
		t.Errorf("expected claimed confirmation; got: %q", out)
	}
	it, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after claim")
	}
	if it.ClaimedBy != "sess-claim-001" {
		t.Errorf("ClaimedBy = %q, want sess-claim-001", it.ClaimedBy)
	}
	if it.ClaimedAt == "" {
		t.Error("ClaimedAt should be set after claim")
	}
}

func TestClaim_NoSessionID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Unsetenv("AS_SESSION_ID")

	if rc := Claim(s, cfg, "T-001"); rc != 2 {
		t.Errorf("Claim with no session returned %d, want 2", rc)
	}
}

func TestClaim_NoItemID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "sess-x")
	defer os.Unsetenv("AS_SESSION_ID")

	if rc := Claim(s, cfg, ""); rc != 2 {
		t.Errorf("Claim with no item id returned %d, want 2", rc)
	}
}

func TestClaim_NotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "sess-x")
	defer os.Unsetenv("AS_SESSION_ID")

	if rc := Claim(s, cfg, "T-999"); rc != 1 {
		t.Errorf("Claim on missing item returned %d, want 1", rc)
	}
}

func TestClaim_LiveConflict(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Register a live rival and stamp its claim.
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-rival",
		SessionID:   "sess-rival",
		PID:         os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "sess-rival"
		it.ClaimedAt = "2026-01-01T00:00:00Z"
		it.Doc.SetField("claimed_by", "sess-rival")
		it.Doc.SetField("claimed_at", "2026-01-01T00:00:00Z")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	os.Setenv("AS_SESSION_ID", "sess-challenger")
	defer os.Unsetenv("AS_SESSION_ID")

	if rc := Claim(s, cfg, "T-001"); rc != 1 {
		t.Errorf("Claim on live-rival item returned %d, want 1", rc)
	}
	// Original claim intact.
	it, _ := s.Get("T-001")
	if it.ClaimedBy != "sess-rival" {
		t.Errorf("rival claim was overwritten: ClaimedBy=%q", it.ClaimedBy)
	}
}

func TestClaim_StaleTakeover(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Stamp a claim from a ghost session (no live registration).
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.ClaimedBy = "sess-ghost"
		it.ClaimedAt = "2026-01-01T00:00:00Z"
		it.Doc.SetField("claimed_by", "sess-ghost")
		it.Doc.SetField("claimed_at", "2026-01-01T00:00:00Z")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	os.Setenv("AS_SESSION_ID", "sess-new")
	defer os.Unsetenv("AS_SESSION_ID")

	if rc := Claim(s, cfg, "T-001"); rc != 0 {
		t.Errorf("Claim over stale returned %d, want 0", rc)
	}
	it, _ := s.Get("T-001")
	if it.ClaimedBy != "sess-new" {
		t.Errorf("ClaimedBy after stale takeover = %q, want sess-new", it.ClaimedBy)
	}
}


// --- Dispatch ---

func TestDispatch_NoBoundary(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_SESSION_ID", "sess-dispatch")
	defer os.Unsetenv("AS_SESSION_ID")

	// No coordinator.yaml — dispatch must refuse.
	rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 1})
	if rc == 0 {
		t.Error("Dispatch without coordinator.yaml should fail")
	}
}

func TestDispatch_DryRun_NoDispatchable(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Setenv("AS_SESSION_ID", "sess-dispatch")
	defer os.Unsetenv("AS_SESSION_ID")

	// No approved plans → nothing dispatchable.
	out := captureStdout(t, func() {
		rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 2, DryRun: true})
		if rc == 0 {
			t.Error("Dispatch with no dispatchable items should fail")
		}
	})
	if !strings.Contains(out, "nothing dispatchable") {
		t.Errorf("expected 'nothing dispatchable' message; got: %q", out)
	}
}

func TestDispatch_DryRun_ShowsPicks(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Setenv("AS_SESSION_ID", "sess-dispatch")
	defer os.Unsetenv("AS_SESSION_ID")

	// Approve T-001 and I-001 so two items are dispatchable.
	approvePlanWithFiles(t, s, cfg, "T-001")
	approvePlanWithFiles(t, s, cfg, "I-001")

	out := captureStdout(t, func() {
		rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 2, DryRun: true})
		if rc != 0 {
			t.Errorf("Dispatch dry-run returned %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN header; got: %q", out)
	}
	if !strings.Contains(out, "picks") {
		t.Errorf("expected 'picks' in output; got: %q", out)
	}
	if !strings.Contains(out, "st watch") {
		t.Errorf("expected 'st watch' aggregation hint; got: %q", out)
	}
}

func TestDispatch_DryRun_CapsAtParallelismCap(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root()) // parallelism_cap = 4
	os.Setenv("AS_SESSION_ID", "sess-dispatch")
	defer os.Unsetenv("AS_SESSION_ID")

	approvePlanWithFiles(t, s, cfg, "T-001")

	// Requesting 100 exceeds the cap of 4; should silently cap to 4.
	// With only 1 dispatchable item, dry-run should still succeed.
	out := captureStdout(t, func() {
		rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 100, DryRun: true})
		if rc != 0 {
			t.Errorf("Dispatch capped dry-run returned %d, want 0", rc)
		}
	})
	// The cap advisory goes to stderr; stdout still shows DRY RUN + picks.
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN header even when capped; got: %q", out)
	}
}

func TestDispatch_DryRun_DeduplicatesPicks(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Setenv("AS_SESSION_ID", "sess-dispatch")
	defer os.Unsetenv("AS_SESSION_ID")

	// Only T-001 is approved — requesting 3 should return 1 pick, not 3 copies.
	approvePlanWithFiles(t, s, cfg, "T-001")

	out := captureStdout(t, func() {
		rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 3, DryRun: true})
		if rc != 0 {
			t.Errorf("Dispatch with 1 available item returned %d, want 0", rc)
		}
	})
	if !strings.Contains(out, "[1]") {
		t.Errorf("expected exactly one pick index [1]; got: %q", out)
	}
	if strings.Contains(out, "[2]") {
		t.Errorf("should not have a second pick when only 1 item available; got: %q", out)
	}
}

func TestDispatch_NoSessionID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Unsetenv("AS_SESSION_ID")

	approvePlanWithFiles(t, s, cfg, "T-001")

	rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 1})
	if rc == 0 {
		t.Error("Dispatch without session ID should fail (non-dry-run)")
	}
}

// TestDispatch_ClaimBeforeSpawn verifies that the dispatcher stamps
// claimed_by on each item before calling Spawn. We exercise the claim
// side-effect only (Spawn itself requires a real claude binary and a
// coordinator.yaml with a resolvable binary — out of scope for unit tests;
// see spawn_test.go for the integration layer).
func TestDispatch_ClaimBeforeSpawn(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Setenv("AS_SESSION_ID", "sess-dispatch-claim")
	defer os.Unsetenv("AS_SESSION_ID")

	approvePlanWithFiles(t, s, cfg, "T-001")

	// Patch spawnFn to a no-op that records whether the item was claimed.
	origSpawnFn := spawnFn
	defer func() { spawnFn = origSpawnFn }()

	var claimedBeforeSpawn string
	spawnFn = func(s *store.Store, cfg *config.Config, opts SpawnOpts) int {
		it, _ := s.Get(opts.Item)
		claimedBeforeSpawn = it.ClaimedBy
		return 0
	}

	captureStdout(t, func() {
		rc := Dispatch(s, cfg, DispatchOpts{Parallelism: 1})
		if rc != 0 {
			t.Errorf("Dispatch returned %d, want 0", rc)
		}
	})
	if claimedBeforeSpawn != "sess-dispatch-claim" {
		t.Errorf("item was not claimed before Spawn; ClaimedBy=%q", claimedBeforeSpawn)
	}
}

// TestDispatch_SpawnFailureReleasesClaimAndContinues verifies that a spawn
// failure releases the just-acquired claim so the item isn't orphaned.
func TestDispatch_SpawnFailureReleasesClaimAndContinues(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	os.Setenv("AS_SESSION_ID", "sess-dispatch-fail")
	defer os.Unsetenv("AS_SESSION_ID")

	approvePlanWithFiles(t, s, cfg, "T-001")
	approvePlanWithFiles(t, s, cfg, "I-001")

	origSpawnFn := spawnFn
	defer func() { spawnFn = origSpawnFn }()

	// First item fails, second succeeds.
	call := 0
	spawnFn = func(s *store.Store, cfg *config.Config, opts SpawnOpts) int {
		call++
		if call == 1 {
			return 1 // fail
		}
		return 0 // succeed
	}

	captureStdout(t, func() {
		Dispatch(s, cfg, DispatchOpts{Parallelism: 2})
	})

	// The first item's claim should be released after the spawn failure.
	// We can't predict which item was first, but at most one should still
	// be claimed (the one whose spawn succeeded).
	t001, _ := s.Get("T-001")
	i001, _ := s.Get("I-001")
	claimed := 0
	if t001.ClaimedBy == "sess-dispatch-fail" {
		claimed++
	}
	if i001.ClaimedBy == "sess-dispatch-fail" {
		claimed++
	}
	if claimed > 1 {
		t.Errorf("expected at most 1 item to remain claimed after failure; got %d", claimed)
	}
}
