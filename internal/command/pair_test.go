package command

import (
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/session"
)

func newTestSessionMgr(cfg *config.Config) *session.Manager {
	return session.NewManager(cfg.SessionsDir(), 2*time.Hour)
}

// Bare `st pair` attaches to the stack-top item and writes the marker.
func TestPairAttachesStackTop(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""}); code != 0 {
		t.Fatalf("StackPush returned %d", code)
	}

	mgr := newTestSessionMgr(cfg)
	if _, err := mgr.EnsureSession("sess-1", "agent-a"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := Pair(s, cfg, mgr, "sess-1", nil, PairOpts{}); code != 0 {
			t.Fatalf("Pair returned %d, want 0", code)
		}
	})
	if !strings.Contains(out, "T-001") {
		t.Errorf("output %q does not mention T-001", out)
	}

	loaded, err := mgr.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Pairing == nil || !loaded.Pairing.Active {
		t.Fatal("pairing marker not active after Pair")
	}
	if loaded.Pairing.Item != "T-001" {
		t.Errorf("Pairing.Item = %q, want T-001", loaded.Pairing.Item)
	}
	if loaded.Pairing.Worktree != "T-001" {
		t.Errorf("Pairing.Worktree = %q, want T-001", loaded.Pairing.Worktree)
	}
}

// `st pair` with an empty stack is a clear error, not a silent no-op.
func TestPairNoActiveItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{})
	})
	if code != 1 {
		t.Errorf("Pair with empty stack returned %d, want 1", code)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker should not be set when there is no active item")
	}
}

// `st pair --off` clears a previously-set marker.
func TestPairOffClearsMarker(t *testing.T) {
	s, cfg := setupTestEnv(t)
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")
	captureStdout(t, func() { Pair(s, cfg, mgr, "sess-1", nil, PairOpts{}) })

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off returned %d, want 0", code)
	}
	if !strings.Contains(out, "Pairing OFF") {
		t.Errorf("expected the actually-cleared message, got: %q", out)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker still set after --off")
	}
}

// `st pair --off` when there was nothing to clear must say so, not report
// the same success message as an actual clear (I-1704 code review: a
// session-id resolution bug upstream would otherwise go unnoticed since
// both cases printed identical output).
func TestPairOffNoopReportsNothingToClear(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off (no-op) returned %d, want 0", code)
	}
	if strings.Contains(out, "Pairing OFF") {
		t.Errorf("no-op --off should NOT report 'Pairing OFF'; got: %q", out)
	}
	if !strings.Contains(out, "nothing to clear") {
		t.Errorf("no-op --off should say there was nothing to clear; got: %q", out)
	}
}

// `st pair --off <arg>` rejects arguments — --off takes none.
func TestPairOffRejectsArgs(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", []string{"I-999"}, PairOpts{Off: true})
	})
	if code != 2 {
		t.Errorf("Pair --off with an arg returned %d, want 2", code)
	}
}

// `st pair <id>` is not implemented in Slice 1 — a clear exit-2 error naming
// the tracking issue, not a silent no-op.
func TestPairWithArgNotImplemented(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", []string{"T-001"}, PairOpts{})
	})
	if code != 2 {
		t.Errorf("Pair with an arg returned %d, want 2", code)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker should not be set by the unimplemented arg path")
	}
}

// No resolvable session id — clear error, not a panic or silent no-op.
func TestPairNoSessionID(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "", nil, PairOpts{})
	})
	if code != 1 {
		t.Errorf("Pair with empty session id returned %d, want 1", code)
	}
}

// A session that reached this point via `st resume` (which never calls
// EnsureSession, unlike `st start`/`st sprint join`) has no session yaml yet.
// Pair must create it rather than fail with "session not found".
func TestPairWithoutPreexistingSessionFile(t *testing.T) {
	s, cfg := setupTestEnv(t)
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	mgr := newTestSessionMgr(cfg)
	// Deliberately no EnsureSession call — simulates `st resume`.

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-resumed", nil, PairOpts{})
	})
	if code != 0 {
		t.Fatalf("Pair on a not-yet-created session returned %d, want 0", code)
	}

	loaded, err := mgr.Load("sess-resumed")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Pairing == nil || !loaded.Pairing.Active {
		t.Fatalf("session/pairing not created: %+v", loaded)
	}
}
