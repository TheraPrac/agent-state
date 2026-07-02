package command

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/session"
)

var errPairTestTPUpFailed = errors.New("tp up failed (test fake)")
var errPairTestTPStatusCheckFailed = errors.New("tp status check failed (test fake)")

func newTestSessionMgr(cfg *config.Config) *session.Manager {
	return session.NewManager(cfg.SessionsDir(), 2*time.Hour)
}

// tpCallRecorder captures calls made through the stubbed tpStatus/tpUp seam
// and controls what they report back to Pair(). StatusUp/UpErr default to
// the "cold start, up succeeds" case (matches a first attach in a fresh test
// fixture); set StatusUp=true to simulate M4's already-live-stack path, or
// StatusErr to simulate the status check itself failing (distinct from a
// confirmed down-state — I-1706 code review).
type tpCallRecorder struct {
	StatusUp    bool
	StatusErr   error
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
	tpStatus = func(cfg *config.Config, worktreeID string) (bool, error) {
		rec.statusCalls = append(rec.statusCalls, worktreeID)
		return rec.StatusUp, rec.StatusErr
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

// I-1707: /pair --off promotion — seeds a plan draft when the item has none
// approved yet, reflecting the recorded edit/command events.
func TestPairOffPromotesUnapprovedItemToPlanSeed(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")
	exitVal := 0
	PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "edit", File: "src/foo.go", Repo: "theraprac-api"})
	PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "command", Command: "go build ./...", ExitCode: &exitVal})

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off returned %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "Seeded a plan draft") {
		t.Errorf("expected output to report a seeded plan, got: %q", out)
	}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found")
	}
	if len(item.LinkedPlans) == 0 {
		t.Fatal("T-001 has no linked plan after promotion")
	}
	if item.PlanApproved {
		t.Error("promoted plan must NOT be self-approved — approval is a separate gate")
	}

	planBody, err := os.ReadFile(filepath.Join(cfg.PlansDir(), "T-001.md"))
	if err != nil {
		t.Fatalf("reading seeded plan: %v", err)
	}
	if !strings.Contains(string(planBody), "src/foo.go") {
		t.Errorf("seeded plan does not mention the edited file:\n%s", planBody)
	}
	if !strings.Contains(string(planBody), "## Approach") || !strings.Contains(string(planBody), "## Tests") {
		t.Errorf("seeded plan missing expected sections:\n%s", planBody)
	}
}

// I-1707: an item that already has an approved plan must NOT have it
// overwritten by promotion — the recorded evidence is preserved via a
// changelog entry instead.
func TestPairOffSkipsSeedingWhenAlreadyApproved(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// setupTestEnv (I-716) pre-seeds every fixture item with a plan sidecar
	// for OTHER tests' convenience — capture its content here so this test
	// can assert promotion leaves it byte-for-byte untouched, not that no
	// file exists at all.
	planPath := filepath.Join(cfg.PlansDir(), "T-001.md")
	before, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("reading pre-seeded fixture plan: %v", err)
	}
	if err := s.Mutate("T-001", func(m *model.Item) error {
		m.PlanApproved = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")
	PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "edit", File: "src/foo.go", Repo: "theraprac-api"})

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off returned %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "already has an approved plan") {
		t.Errorf("expected output to report the skip, got: %q", out)
	}

	after, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("reading plan after promotion: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("approved plan was modified by promotion:\nbefore: %s\nafter:  %s", before, after)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "pairing_evidence" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a pairing_evidence changelog entry, got: %+v", entries)
	}
}

// I-1707: an observation event durably credits browser verification via the
// changelog, independent of whether a plan was seeded.
func TestPairOffCreditsBrowserVerification(t *testing.T) {
	s, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")
	PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "observation", Text: "login flow renders correctly"})

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off returned %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "Credited 1 browser-verification") {
		t.Errorf("expected output to report the credit, got: %q", out)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "pairing_browser_verified" && e.Reason == "login flow renders correctly" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a pairing_browser_verified changelog entry with the observation text, got: %+v", entries)
	}
}

// I-1707: a paired session with no recorded events promotes nothing — the
// pre-seeded fixture plan (I-716) is left untouched, just an informative note.
func TestPairOffNoPromotionWhenNoEventsRecorded(t *testing.T) {
	s, cfg := setupTestEnv(t)
	planPath := filepath.Join(cfg.PlansDir(), "T-001.md")
	before, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("reading pre-seeded fixture plan: %v", err)
	}
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")

	code := 0
	out := captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{Off: true})
	})
	if code != 0 {
		t.Fatalf("Pair --off returned %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "nothing to promote") {
		t.Errorf("expected output to note there was nothing to promote, got: %q", out)
	}
	after, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("reading plan after --off: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("fixture plan was modified despite no recorded events:\nbefore: %s\nafter:  %s", before, after)
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
	up, statusErr := defaultTPStatus(cfg, "I-9999")
	if statusErr != nil {
		t.Errorf("defaultTPStatus: unexpected error %v (stub's non-zero exit is a normal exec.ExitError, not an exec-start failure)", statusErr)
	}
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

// defaultTPStatus must distinguish "tp ran and reported not-up" (a normal
// exec.ExitError, per tp's documented contract) from "tp could not even be
// started" (exec.Error — binary missing/not executable) — I-1706 code
// review (bugbot rule 3): a bare bool return collapsed these into the same
// "false" and let ensureTPStack silently treat a genuine check failure as an
// ordinary down-state.
func TestDefaultTPStatusDistinguishesExecFailureFromDown(t *testing.T) {
	// tp not on PATH at all.
	t.Setenv("PATH", t.TempDir())
	_, cfg := setupTestEnv(t)

	up, err := defaultTPStatus(cfg, "I-9999")
	if err == nil {
		t.Fatal("defaultTPStatus with tp missing from PATH should return a non-nil error, got nil")
	}
	if up {
		t.Error("defaultTPStatus should report down (not up) when the check itself failed")
	}
}

// ensureTPStack must surface a status-check failure directly, WITHOUT
// falling through to tp up as if the stack were merely down — that would
// silently attempt to re-provision (or worse, per tp's kill-and-relaunch
// idempotency, tear down and rebuild) a stack whose actual state couldn't be
// determined. I-1706 code review.
func TestPairStatusCheckFailureBlocksPairingWithoutCallingTPUp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	rec := stubTP(t)
	rec.StatusErr = errPairTestTPStatusCheckFailed
	StackPush(s, cfg, "T-001", StackPushOpts{Reason: ""})
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")

	code := 0
	captureStdout(t, func() {
		code = Pair(s, cfg, mgr, "sess-1", nil, PairOpts{})
	})
	if code != 1 {
		t.Errorf("Pair returned %d, want 1 (status check failed)", code)
	}
	if len(rec.upCalls) != 0 {
		t.Errorf("tpUp calls = %v, want none — a status-check failure must not fall through to tp up", rec.upCalls)
	}

	loaded, _ := mgr.Load("sess-1")
	if loaded.Pairing != nil {
		t.Error("pairing marker should not be set when the status check fails")
	}
}
