package command

import (
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/testutil"
)

// seedTokens sets a minimal non-zero token total on an item (legacy reg field).
func seedTokens(t *testing.T, env *testutil.Env, id string) {
	t.Helper()
	if err := env.S.Mutate(id, func(it *model.Item) error {
		it.SetNested("time_tracking", "reg_input_tokens", "1000")
		it.SetNested("time_tracking", "reg_output_tokens", "200")
		return nil
	}); err != nil {
		t.Fatalf("seedTokens(%s): %v", id, err)
	}
}

// seedWorkTime sets a measured active-time value on an item.
func seedWorkTime(t *testing.T, env *testutil.Env, id string) {
	t.Helper()
	if err := env.S.Mutate(id, func(it *model.Item) error {
		it.SetNested("time_tracking", "accumulated_seconds", "120")
		return nil
	}); err != nil {
		t.Fatalf("seedWorkTime(%s): %v", id, err)
	}
}

// I-1614: helper unit table.
// I-1614 review [0]: loaded items store legacy token / seconds fields as NUMERIC
// YAML scalars in the typed TimeTracking map; the Doc-only readers miss them.
// captureComplete must recognize numeric legacy capture.
func TestCaptureComplete_NumericLegacyFields(t *testing.T) {
	now := time.Now()
	it := &model.Item{TimeTracking: map[string]interface{}{
		"reg_input_tokens":    3562, // int scalar (as parsed from YAML)
		"accumulated_seconds": 60.0, // float scalar
	}}
	timeOK, tokOK := captureComplete(it, now)
	if !timeOK || !tokOK {
		t.Errorf("numeric legacy fields not recognized: time=%v tok=%v (gate would falsely reject loaded items)", timeOK, tokOK)
	}
}

func TestCaptureComplete(t *testing.T) {
	now := time.Now()
	mk := func(seed func(it *model.Item)) *model.Item {
		it := &model.Item{}
		seed(it)
		return it
	}
	cases := []struct {
		name             string
		item             *model.Item
		wantTime, wantTok bool
	}{
		{"both present", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "accumulated_seconds", "60")
			it.SetNested("time_tracking", "reg_input_tokens", "10")
		}), true, true},
		{"tokens only", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "reg_input_tokens", "10")
		}), false, true},
		{"time only (accumulated)", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "accumulated_seconds", "60")
		}), true, false},
		{"time via work_duration", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "work_duration_seconds", "5")
		}), true, false},
		{"time via live session_started_at", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "session_started_at", now.Add(-time.Minute).Format(time.RFC3339))
		}), true, false},
		{"legacy output tokens count", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "reg_output_tokens", "42")
		}), false, true},
		{"neither", mk(func(it *model.Item) {}), false, false},
		{"zero values don't count", mk(func(it *model.Item) {
			it.SetNested("time_tracking", "accumulated_seconds", "0")
			it.SetNested("time_tracking", "reg_input_tokens", "0")
		}), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTime, gotTok := captureComplete(c.item, now)
			if gotTime != c.wantTime || gotTok != c.wantTok {
				t.Errorf("captureComplete = (time=%v,tok=%v), want (time=%v,tok=%v)", gotTime, gotTok, c.wantTime, c.wantTok)
			}
		})
	}
}

func TestClose_CaptureGateBlocksMissingTokens(t *testing.T) {
	env := testutil.NewEnv(t)
	seedWorkTime(t, env, "T-003") // time but no tokens
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true}); code == 0 {
		t.Fatalf("close should be blocked when tokens are missing, got 0")
	}
	it, _ := env.S.Get("T-003")
	if it.Status == "done" {
		t.Errorf("item closed despite missing tokens (status=%q)", it.Status)
	}
}

func TestClose_CaptureGateBlocksMissingTime(t *testing.T) {
	env := testutil.NewEnv(t)
	seedTokens(t, env, "T-003") // tokens but no time
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true}); code == 0 {
		t.Fatalf("close should be blocked when work time is missing, got 0")
	}
	it, _ := env.S.Get("T-003")
	if it.Status == "done" {
		t.Errorf("item closed despite missing work time (status=%q)", it.Status)
	}
}

func TestClose_CaptureGateAllowsBothPresent(t *testing.T) {
	env := testutil.NewEnv(t)
	seedWorkTime(t, env, "T-003")
	seedTokens(t, env, "T-003")
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true}); code != 0 {
		t.Fatalf("close should succeed when both dimensions present, got %d", code)
	}
	it, _ := env.S.Get("T-003")
	if it.Status != "done" {
		t.Errorf("status = %q, want done", it.Status)
	}
}

func TestClose_ForceDoesNotBypassCaptureGate(t *testing.T) {
	env := testutil.NewEnv(t)
	// No capture seeded; --force must NOT bypass the capture gate.
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true}); code == 0 {
		t.Fatalf("--force wrongly bypassed the capture gate (got 0)")
	}
	it, _ := env.S.Get("T-003")
	if it.Status == "done" {
		t.Errorf("item closed under --force despite no capture (status=%q)", it.Status)
	}
}

func TestClose_AllowMissingCaptureOverrideLogged(t *testing.T) {
	env := testutil.NewEnv(t)
	// No capture; explicit override closes and records an audit entry.
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true, AllowMissingCapture: "completed outside a tracked session"}); code != 0 {
		t.Fatalf("close with --allow-missing-capture should succeed, got %d", code)
	}
	it, _ := env.S.Get("T-003")
	if it.Status != "done" {
		t.Errorf("status = %q, want done", it.Status)
	}
	entries, err := changelog.Read(env.Cfg, "T-003")
	if err != nil {
		t.Fatalf("changelog.Read: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "close_allow_missing_capture" && strings.Contains(e.Reason, "completed outside a tracked session") {
			found = true
		}
	}
	if !found {
		t.Errorf("no close_allow_missing_capture audit entry recorded")
	}
}

// I-1614 review: tokens recorded ONLY in the canonical real_tokens blob (what
// the Stop hook and `reconcile-tokens apply` write) must satisfy the gate.
func TestClose_RealTokensBlobSatisfiesGate(t *testing.T) {
	env := testutil.NewEnv(t)
	seedWorkTime(t, env, "T-003")
	if err := env.S.Mutate("T-003", func(it *model.Item) error {
		it.SetNested("time_tracking", "real_tokens", "input=500 output=120 cache_read=0 cache_creation_5m=0 cache_creation_1h=0")
		return nil
	}); err != nil {
		t.Fatalf("seed real_tokens: %v", err)
	}
	if code := Close(env.S, env.Cfg, "T-003", "done", CloseOpts{Force: true}); code != 0 {
		t.Fatalf("close should pass with real_tokens blob present, got %d", code)
	}
	it, _ := env.S.Get("T-003")
	if it.Status != "done" {
		t.Errorf("status = %q, want done", it.Status)
	}
}

// I-1614 review: archived (administrative terminal) must NOT require capture.
func TestClose_ArchivedSkipsCaptureGate(t *testing.T) {
	env := testutil.NewEnv(t)
	// no capture seeded
	if code := Close(env.S, env.Cfg, "T-003", "archived", CloseOpts{Force: true}); code != 0 {
		t.Fatalf("archived close should skip the capture gate, got %d", code)
	}
	it, _ := env.S.Get("T-003")
	if it.Status != "archived" {
		t.Errorf("status = %q, want archived", it.Status)
	}
}

func TestClose_AbandonSkipsCaptureGate(t *testing.T) {
	env := testutil.NewEnv(t)
	// abandoned with a valid drop reason closes despite empty capture.
	if code := Close(env.S, env.Cfg, "T-003", "abandoned", CloseOpts{Reason: "superseded"}); code != 0 {
		t.Fatalf("abandon should skip the capture gate, got %d", code)
	}
	it, _ := env.S.Get("T-003")
	if it.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned", it.Status)
	}
}
