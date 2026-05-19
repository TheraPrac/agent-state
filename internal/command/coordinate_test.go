package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

// boundaryFixture is a minimal-but-real coordinator.yaml written into the
// test env's .as/ so Coordinate's LoadBoundary succeeds.
const boundaryFixture = `escalation:
  respawn_limit: 3
  budget_cap_usd:
    per_item: 40
    per_objective: 150
  stuck_multiplier: 3
  parallelism_cap: 4
  tripwire_list:
    - prod_infra_apply
dedupe:
  window_minutes: 30
escalation_channel:
  default: alerts_band
  active_ping:
    - category_E
    - budget_cap
`

func writeBoundary(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".as", "coordinator.yaml"),
		[]byte(boundaryFixture), 0644); err != nil {
		t.Fatalf("write boundary fixture: %v", err)
	}
}

func TestSelectNext(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "") // user-added ⇒ approved
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	it, why := selectNext(s, cfg)
	if it == nil || it.ID != "T-001" {
		t.Fatalf("selectNext = %v (%s), want T-001", it, why)
	}

	// Claim it → no longer eligible (single in-flight invariant).
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.ClaimedBy = "some-session"
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if it, why := selectNext(s, cfg); it != nil {
		t.Errorf("claimed item must be skipped, got %s", it.ID)
	} else if why == "" {
		t.Error("a skip must come with a reason (no opaque empty)")
	}
}

func TestCoordinateBoundaryMissingHardFails(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// No coordinator.yaml written → must refuse to run (never unbounded).
	if rc := Coordinate(s, cfg, CoordinateOpts{DryRun: true}); rc == 0 {
		t.Fatal("missing boundary must hard-fail (contract §11), got rc=0")
	}
}

func TestCoordinateDryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	t.Setenv("AS_AGENT_ID", "")
	QueueAdd(s, cfg, "T-001", QueueOpts{})

	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{DryRun: true})
	})
	if rc != 0 {
		t.Fatalf("dry-run rc = %d, want 0\n%s", rc, out)
	}
	for _, want := range []string{
		"DRY RUN", "respawn_limit=3", "per_item=$40", "parallelism=4",
		"picked:      T-001", "size-class:", "st spawn T-001",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// Side-effect-free: no worker registration may have been written.
	if entries, _ := os.ReadDir(filepath.Join(cfg.Root(), ".as", "agents")); len(entries) > 0 {
		t.Errorf("dry-run must register nothing, found %d agent file(s)", len(entries))
	}
}

// TestReapDeadChildrenNoChildrenNoop pins the safety invariant: with no
// child processes, reapDeadChildren must return promptly (ECHILD), never
// hang or panic. The actual zombie-reap behaviour is an OS interaction
// proven by T-363 live-verify (a unit test cannot faithfully fork+Release
// a child the way command.Spawn does without being flaky).
func TestReapDeadChildrenNoChildrenNoop(t *testing.T) {
	done := make(chan struct{})
	go func() { reapDeadChildren(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapDeadChildren hung with no children (must ECHILD-return immediately)")
	}
}

func TestCoordinateDryRunNoEligibleItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	writeBoundary(t, cfg.Root())
	// Nothing queued → reports the reason and exits 0 (no opaque blank).
	var rc int
	out := captureStdout(t, func() {
		rc = Coordinate(s, cfg, CoordinateOpts{DryRun: true})
	})
	if rc != 0 {
		t.Fatalf("no-eligible rc = %d, want 0", rc)
	}
	if !strings.Contains(out, "no eligible item") {
		t.Errorf("must surface WHY nothing ran, got %q", out)
	}
}
