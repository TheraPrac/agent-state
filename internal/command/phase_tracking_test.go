package command

import (
	"strings"
	"testing"
)

// TestPhaseStartDoneStatus verifies the basic phase lifecycle on an item.
func TestPhaseStartDoneStatus(t *testing.T) {
	s, cfg := setupTestEnv(t)

	t.Run("invalid phase name → rc=1", func(t *testing.T) {
		var rc int
		stderr := captureStderrStr(t, func() {
			rc = PhaseStart(s, cfg, "T-001", "invalid-phase")
		})
		if rc != 1 {
			t.Errorf("PhaseStart invalid = %d, want 1", rc)
		}
		if !strings.Contains(stderr, "unknown phase") {
			t.Errorf("stderr should say 'unknown phase', got:\n%s", stderr)
		}
	})

	t.Run("PhaseStart sets active_phase and seeds by_phase", func(t *testing.T) {
		if rc := PhaseStart(s, cfg, "T-001", "code"); rc != 0 {
			t.Fatalf("PhaseStart = %d, want 0", rc)
		}
		item, _ := s.Get("T-001")
		if got := activePhase(item); got != "code" {
			t.Errorf("activePhase = %q, want %q", got, "code")
		}
		agg := readByPhase(item, "code")
		if agg.Phase != "code" {
			t.Errorf("readByPhase phase = %q, want %q", agg.Phase, "code")
		}
		if agg.StartedAt == "" {
			t.Error("by_phase.code.started_at should be set after PhaseStart")
		}
	})

	t.Run("PhaseStatus shows active phase", func(t *testing.T) {
		out := captureStdout(t, func() {
			PhaseStatus(s, cfg, "T-001")
		})
		if !strings.Contains(out, "code") {
			t.Errorf("PhaseStatus output should contain 'code', got:\n%s", out)
		}
	})

	t.Run("PhaseDone clears active_phase and stamps ended_at", func(t *testing.T) {
		if rc := PhaseDone(s, cfg, "T-001"); rc != 0 {
			t.Fatalf("PhaseDone = %d, want 0", rc)
		}
		item, _ := s.Get("T-001")
		if got := activePhase(item); got != "" {
			t.Errorf("activePhase after done = %q, want empty", got)
		}
		agg := readByPhase(item, "code")
		if agg.EndedAt == "" {
			t.Error("by_phase.code.ended_at should be set after PhaseDone")
		}
	})

	t.Run("PhaseDone with no active phase → rc=1", func(t *testing.T) {
		var rc int
		stderr := captureStderrStr(t, func() {
			rc = PhaseDone(s, cfg, "T-001")
		})
		if rc != 1 {
			t.Errorf("PhaseDone with no active phase = %d, want 1", rc)
		}
		if !strings.Contains(stderr, "no active phase") {
			t.Errorf("stderr should mention 'no active phase', got:\n%s", stderr)
		}
	})
}

// TestRunPipelinePhaseTracking verifies that multiple phases accumulate correctly.
func TestRunPipelinePhaseTracking(t *testing.T) {
	s, cfg := setupTestEnv(t)

	for _, phase := range []string{"plan", "code", "test"} {
		if rc := PhaseStart(s, cfg, "T-001", phase); rc != 0 {
			t.Fatalf("PhaseStart(%q) = %d", phase, rc)
		}
		_ = SessionLog(s, cfg, SessionLogPayload{
			ItemID:          "T-001",
			Model:           "claude-sonnet-4-6",
			RegInputTokens:  100,
			RegOutputTokens: 50,
			ProcessMs:       1000,
		})
		if rc := PhaseDone(s, cfg, "T-001"); rc != 0 {
			t.Fatalf("PhaseDone(%q) = %d", phase, rc)
		}
	}

	item, _ := s.Get("T-001")
	for _, phase := range []string{"plan", "code", "test"} {
		agg := readByPhase(item, phase)
		if agg.Phase != phase {
			t.Errorf("phase %q not found in by_phase", phase)
		}
		if agg.StartedAt == "" || agg.EndedAt == "" {
			t.Errorf("phase %q missing started_at or ended_at", phase)
		}
		if agg.Turns < 1 {
			t.Errorf("phase %q turns = %d, want >= 1", phase, agg.Turns)
		}
	}
}

// TestSessionLogRoutesToActivePhase verifies that SessionLog credits the active phase.
func TestSessionLogRoutesToActivePhase(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := PhaseStart(s, cfg, "T-001", "code"); rc != 0 {
		t.Fatalf("PhaseStart = %d", rc)
	}

	if rc := SessionLog(s, cfg, SessionLogPayload{
		ItemID:          "T-001",
		Model:           "claude-sonnet-4-6",
		RegInputTokens:  200,
		RegOutputTokens: 80,
		ProcessMs:       2000,
	}); rc != 0 {
		t.Fatalf("SessionLog = %d", rc)
	}

	item, _ := s.Get("T-001")
	agg := readByPhase(item, "code")
	if agg.Turns != 1 {
		t.Errorf("by_phase.code.turns = %d, want 1", agg.Turns)
	}
	if agg.Tokens.Input != 200 {
		t.Errorf("by_phase.code.tokens.input = %d, want 200", agg.Tokens.Input)
	}
}

// TestPhaseStartAutoClosesPrior verifies that starting a new phase while another
// is active stamps the prior phase's ended_at (rather than leaving it orphaned)
// and moves active_phase to the new phase. Guards the phase.go auto-close path.
func TestPhaseStartAutoClosesPrior(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := PhaseStart(s, cfg, "T-001", "plan"); rc != 0 {
		t.Fatalf("PhaseStart(plan) = %d", rc)
	}
	// Start "code" without an intervening PhaseDone — the prior "plan" phase
	// must be auto-closed.
	if rc := PhaseStart(s, cfg, "T-001", "code"); rc != 0 {
		t.Fatalf("PhaseStart(code) = %d", rc)
	}

	item, _ := s.Get("T-001")
	if got := activePhase(item); got != "code" {
		t.Errorf("activePhase = %q, want %q", got, "code")
	}
	prior := readByPhase(item, "plan")
	if prior.EndedAt == "" {
		t.Error("prior phase 'plan' should have ended_at stamped after auto-close")
	}
	cur := readByPhase(item, "code")
	if cur.StartedAt == "" {
		t.Error("new phase 'code' should have started_at set")
	}
	if cur.EndedAt != "" {
		t.Errorf("new phase 'code' should not be closed yet, ended_at = %q", cur.EndedAt)
	}
}

// TestPhaseParityInteractiveVsRun verifies that by_phase entries have identical schema
// regardless of execution path. Both items go through PhaseStart + SessionLog + PhaseDone.
func TestPhaseParityInteractiveVsRun(t *testing.T) {
	s, cfg := setupTestEnv(t)

	for _, id := range []string{"T-001", "T-002"} {
		PhaseStart(s, cfg, id, "code")
		_ = SessionLog(s, cfg, SessionLogPayload{
			ItemID:          id,
			Model:           "claude-sonnet-4-6",
			RegInputTokens:  100,
			RegOutputTokens: 40,
			ProcessMs:       1500,
		})
		PhaseDone(s, cfg, id)
	}

	item1, _ := s.Get("T-001")
	item2, _ := s.Get("T-002")

	for _, pair := range []struct {
		name string
		agg  byPhaseAggregate
	}{
		{"T-001", readByPhase(item1, "code")},
		{"T-002", readByPhase(item2, "code")},
	} {
		if pair.agg.Phase != "code" {
			t.Errorf("%s: phase = %q, want 'code'", pair.name, pair.agg.Phase)
		}
		if pair.agg.StartedAt == "" {
			t.Errorf("%s: started_at empty", pair.name)
		}
		if pair.agg.EndedAt == "" {
			t.Errorf("%s: ended_at empty", pair.name)
		}
		if pair.agg.Turns != 1 {
			t.Errorf("%s: turns = %d, want 1", pair.name, pair.agg.Turns)
		}
	}
}
