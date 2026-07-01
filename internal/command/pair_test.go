package command

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/session"
)

var errPairTestTPUpFailed = errors.New("tp up failed (test fake)")

func newTestSessionMgr(cfg *config.Config) *session.Manager {
	return session.NewManager(cfg.SessionsDir(), 2*time.Hour)
}

// tpCallRecorder captures calls made through the stubbed tpStatus/tpUp seam
// and controls what they report back to Pair(). StatusUp/UpErr default to
// the "cold start, up succeeds" case (matches a first attach in a fresh test
// fixture); set StatusUp=true to simulate M4's already-live-stack path.
type tpCallRecorder struct {
	StatusUp    bool
	UpErr       error
	statusCalls []string
	upCalls     []string
}

// stubTP swaps the package-level tpStatus/tpUp vars for in-process fakes
// (t.Cleanup restored) so Pair() tests never shell out to `tp`/Docker —
// mirrors the nodeInstaller test-injection precedent in start_test.go.
// I-1706.
func stubTP(t *testing.T) *tpCallRecorder {
	t.Helper()
	rec := &tpCallRecorder{}
	origStatus, origUp := tpStatus, tpUp
	tpStatus = func(cfg *config.Config, worktreeID string) bool {
		rec.statusCalls = append(rec.statusCalls, worktreeID)
		return rec.StatusUp
	}
	tpUp = func(cfg *config.Config, worktreeID string) error {
		rec.upCalls = append(rec.upCalls, worktreeID)
		return rec.UpErr
	}
	t.Cleanup(func() { tpStatus, tpUp = origStatus, origUp })
	return rec
}

// Bare `st pair` attaches to the stack-top item and writes the marker.
func TestPairAttachesStackTop(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
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
	// M1 attach-current must never call Start() — its precondition is
	// "already started". We can't observe Start() directly here, but we can
	// confirm the tp reuse-or-start step ran for the right worktree.
	if len(rec.upCalls) != 1 || rec.upCalls[0] != "T-001" {
		t.Errorf("tpUp calls = %v, want exactly one call for T-001", rec.upCalls)
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
	stubTP(t)
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

// M2 attach-item: `st pair <id>` on an existing but not-yet-started item
// starts it (T-001 fixture is status=queued) before attaching.
func TestPairAttachItemStartsIfNotActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", []string{"T-001"}, PairOpts{})
	})
	if code != 0 {
		t.Fatalf("Pair(T-001) returned %d, want 0: %s", code, out)
	}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after Pair")
	}
	if item.Status != "active" {
		t.Errorf("T-001 status = %q after Pair, want active (Start() should have run)", item.Status)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing == nil || !loaded.Pairing.Active || loaded.Pairing.Item != "T-001" {
		t.Fatalf("pairing marker not set correctly: %+v", loaded.Pairing)
	}
	if len(rec.upCalls) != 1 || rec.upCalls[0] != "T-001" {
		t.Errorf("tpUp calls = %v, want exactly one call for T-001", rec.upCalls)
	}
}

// M2 attach-item: an id-shaped arg that doesn't resolve to an existing item
// is a hard error — never silently reinterpreted as a fresh title.
func TestPairAttachItemNonexistentIDErrors(t *testing.T) {
	s, cfg := setupTestEnv(t)
	stubTP(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	before := len(s.All())

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", []string{"I-9999"}, PairOpts{})
	})
	if code != 1 {
		t.Errorf("Pair(I-9999) returned %d, want 1", code)
	}
	if _, ok := s.Get("I-9999"); ok {
		t.Error("I-9999 should not have been created")
	}
	if len(s.All()) != before {
		t.Errorf("item count changed (%d -> %d); a nonexistent id-shaped arg must not create anything", before, len(s.All()))
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker should not be set on the nonexistent-id error path")
	}
}

// M3 fresh: an arg that isn't id-shaped is treated as a title — creates a
// new issue, starts it, then attaches.
func TestPairFreshTitleCreatesAndStarts(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	before := len(s.All())

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", []string{"add", "live", "pairing", "demo"}, PairOpts{})
	})
	if code != 0 {
		t.Fatalf("Pair(fresh title) returned %d, want 0: %s", code, out)
	}
	if len(s.All()) != before+1 {
		t.Fatalf("item count = %d, want %d (exactly one new item created)", len(s.All()), before+1)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing == nil || !loaded.Pairing.Active {
		t.Fatal("pairing marker not active after M3 fresh")
	}
	newID := loaded.Pairing.Item
	item, ok := s.Get(newID)
	if !ok {
		t.Fatalf("created item %s not found", newID)
	}
	if item.Title != "add live pairing demo" {
		t.Errorf("created item title = %q, want %q", item.Title, "add live pairing demo")
	}
	if item.Type != "issue" {
		t.Errorf("created item type = %q, want issue", item.Type)
	}
	if item.Status != "active" {
		t.Errorf("created item status = %q, want active (Start() should have run)", item.Status)
	}
	if len(rec.upCalls) != 1 || rec.upCalls[0] != newID {
		t.Errorf("tpUp calls = %v, want exactly one call for %s", rec.upCalls, newID)
	}
}

// M4 attach-stack: when tp reports a live stack already up for the resolved
// worktree, Pair() reuses it instead of calling tp up again.
func TestPairReusesLiveStack(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
	rec.StatusUp = true // simulate: a live stack already exists for T-001
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{})
	})
	if code != 0 {
		t.Fatalf("Pair returned %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "reusing") {
		t.Errorf("expected output to report reuse, got: %q", out)
	}
	if len(rec.statusCalls) != 1 || rec.statusCalls[0] != "T-001" {
		t.Errorf("tpStatus calls = %v, want exactly one call for T-001", rec.statusCalls)
	}
	if len(rec.upCalls) != 0 {
		t.Errorf("tpUp calls = %v, want none — a live stack must not be re-provisioned", rec.upCalls)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing == nil || !loaded.Pairing.Active {
		t.Fatal("pairing marker not active after M4 reuse")
	}
}

// A `tp up` failure must not activate pairing — the marker only gets set
// once the stack is confirmed up.
func TestPairTPUpFailureBlocksPairing(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
	rec.UpErr = errPairTestTPUpFailed
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{})
	})
	if code != 1 {
		t.Errorf("Pair returned %d, want 1 (tp up failed)", code)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker should not be set when tp up fails")
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
	stubTP(t)
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

// TestDefaultTPStatusAndUp exercises the REAL defaultTPStatus/defaultTPUp/
// tpEnv subprocess-invocation code (not the test-fake seam other tests use)
// against a stub `tp` script on PATH — the one thing the fake-based tests
// above don't cover: that the actual exec.Command argv ("--worktree=<id>")
// and env (AS_AGENT_ID, THERAPRAC_AGENTS_ROOT) are constructed correctly.
// Real dep behavior once `tp` receives those args was proven live in I-1705.
func TestDefaultTPStatusAndUp(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "tp-invocations.log")
	stubPath := filepath.Join(dir, "tp")
	stub := `#!/usr/bin/env bash
{
  echo "ARGV:$*"
  echo "AS_AGENT_ID=${AS_AGENT_ID:-<unset>}"
  echo "THERAPRAC_AGENTS_ROOT=${THERAPRAC_AGENTS_ROOT:-<unset>}"
} >> "` + logPath + `"
if [ "$1" = "status" ]; then
  exit "${TP_STUB_STATUS_EXIT:-1}"
fi
exit "${TP_STUB_UP_EXIT:-0}"
`
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AS_AGENT_ID", "agent-zzz-should-be-overridden")

	_, cfg := setupTestEnv(t)

	// AgentRoot() resolution needs SOME root to derive THERAPRAC_AGENTS_ROOT
	// from; setupTestEnv's minimal fixture may not resolve one, and that's
	// fine — tpEnv skips the var when AgentRoot() is empty rather than
	// writing a garbage path. Assert only what's guaranteed: AS_AGENT_ID is
	// always injected as cfg.AgentID(), overriding the ambient env var above.
	up := defaultTPStatus(cfg, "I-9999")
	if up {
		t.Error("defaultTPStatus with TP_STUB_STATUS_EXIT unset (defaults nonzero) should report down")
	}
	if err := defaultTPUp(cfg, "I-9999"); err != nil {
		t.Errorf("defaultTPUp: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading stub log: %v", err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "ARGV:status --worktree=I-9999") {
		t.Errorf("stub log missing expected `tp status --worktree=I-9999` invocation:\n%s", log)
	}
	if !strings.Contains(log, "ARGV:up --worktree=I-9999") {
		t.Errorf("stub log missing expected `tp up --worktree=I-9999` invocation:\n%s", log)
	}
	wantAgentLine := "AS_AGENT_ID=" + cfg.AgentID()
	if !strings.Contains(log, wantAgentLine) {
		t.Errorf("stub log missing %q (AS_AGENT_ID must be injected as cfg.AgentID(), not left as the ambient env var):\n%s", wantAgentLine, log)
	}
}
