package command

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/model"
)

// TestStart_DispatchLine — default path emits DISPATCH line with sonnet
// (model-rec falls back to sonnet when no engine wired and no tier stamped).
func TestStart_DispatchLine(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true}); code != 0 {
			t.Errorf("Start returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "DISPATCH: launch session with model=") {
		t.Errorf("expected DISPATCH line in output; got:\n%s", out)
	}
	if !strings.Contains(out, "model=sonnet") {
		t.Errorf("expected model=sonnet in DISPATCH line (fallback); got:\n%s", out)
	}
}

// TestStart_DispatchLineWithModelTier — item with model_tier: opus emits opus.
func TestStart_DispatchLineWithModelTier(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("model_tier", "opus")
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true}); code != 0 {
			t.Errorf("Start returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "model=opus") {
		t.Errorf("expected model=opus from model_tier field; got:\n%s", out)
	}
}

// TestStart_DispatchLineWithEscalate — --escalate overrides the resolved tier.
func TestStart_DispatchLineWithEscalate(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true, Escalate: "opus"}); code != 0 {
			t.Errorf("Start returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "model=opus") {
		t.Errorf("expected model=opus after --escalate opus; got:\n%s", out)
	}
}

// TestStart_EscalateChangelogEntry — --escalate writes a start_escalate changelog entry.
func TestStart_EscalateChangelogEntry(t *testing.T) {
	s, cfg := setupTestEnv(t)
	captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true, Escalate: "opus"}); code != 0 {
			t.Fatalf("Start returned %d, want 0", code)
		}
	})
	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "start_escalate" {
			found = true
			if e.NewValue != "opus" {
				t.Errorf("start_escalate NewValue = %q, want opus", e.NewValue)
			}
			if e.OldValue == "" {
				t.Error("start_escalate OldValue should be non-empty (original resolved tier)")
			}
			break
		}
	}
	if !found {
		t.Error("expected start_escalate entry in changelog after --escalate")
	}
}

// TestStart_EscalateInvalidTier — invalid --escalate tier is warned and ignored.
func TestStart_EscalateInvalidTier(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true, Escalate: "turbo"}); code != 0 {
			t.Errorf("Start returned %d, want 0 (invalid escalate should warn, not fail)", code)
		}
	})
	// Should fall back to resolved tier (sonnet), not "turbo".
	if strings.Contains(out, "model=turbo") {
		t.Errorf("invalid escalate tier should not appear in DISPATCH line; got:\n%s", out)
	}
	if !strings.Contains(out, "DISPATCH:") {
		t.Errorf("DISPATCH line should still be emitted after invalid escalate; got:\n%s", out)
	}
}

// TestStart_InlineFlag — --inline is a no-op; DISPATCH line is still printed.
func TestStart_InlineFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true, Inline: true}); code != 0 {
			t.Errorf("Start returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "DISPATCH: launch session with model=") {
		t.Errorf("DISPATCH line should be present with --inline; got:\n%s", out)
	}
}

// TestStart_NoPushDispatchStillPrinted — --no-push does not suppress the DISPATCH line.
func TestStart_NoPushDispatchStillPrinted(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if code := Start(s, cfg, "T-001", StartOpts{NoPush: true}); code != 0 {
			t.Errorf("Start returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "DISPATCH: launch session with model=") {
		t.Errorf("DISPATCH line must appear even with --no-push; got:\n%s", out)
	}
}
