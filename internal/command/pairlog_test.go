package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/session"
)

func setPairingActive(t *testing.T, mgr *session.Manager, sessionID, itemID string) {
	t.Helper()
	if _, err := mgr.EnsureSession(sessionID, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPairing(sessionID, &session.Pairing{
		Active:      true,
		Item:        itemID,
		Worktree:    itemID,
		ActivatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPairLogWritesEventWhenPaired(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")

	exitVal := 0
	code := PairLog(cfg, mgr, "sess-1", PairLogOpts{
		Type: "edit", File: "src/foo.go", Repo: "theraprac-api",
	})
	if code != 0 {
		t.Fatalf("PairLog returned %d, want 0", code)
	}
	code = PairLog(cfg, mgr, "sess-1", PairLogOpts{
		Type: "command", Command: "go build ./...", ExitCode: &exitVal,
	})
	if code != 0 {
		t.Fatalf("PairLog returned %d, want 0", code)
	}
	code = PairLog(cfg, mgr, "sess-1", PairLogOpts{
		Type: "server", Action: "up", Worktree: "T-001",
	})
	if code != 0 {
		t.Fatalf("PairLog returned %d, want 0", code)
	}
	code = PairLog(cfg, mgr, "sess-1", PairLogOpts{
		Type: "observation", Text: "login flow renders correctly",
	})
	if code != 0 {
		t.Fatalf("PairLog returned %d, want 0", code)
	}

	events, err := readPairLogEvents(cfg, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Type != "edit" || events[0].File != "src/foo.go" || events[0].Repo != "theraprac-api" {
		t.Errorf("edit event = %+v", events[0])
	}
	if events[1].Type != "command" || events[1].Command != "go build ./..." || events[1].ExitCode == nil || *events[1].ExitCode != 0 {
		t.Errorf("command event = %+v", events[1])
	}
	if events[2].Type != "server" || events[2].Action != "up" || events[2].Worktree != "T-001" {
		t.Errorf("server event = %+v", events[2])
	}
	if events[3].Type != "observation" || events[3].Text != "login flow renders correctly" {
		t.Errorf("observation event = %+v", events[3])
	}
}

func TestPairLogNoopsWhenNotPaired(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a") // no pairing set

	code := PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "command", Command: "rm -rf /"})
	if code != 0 {
		t.Errorf("PairLog on an unpaired session returned %d, want 0 (silent no-op)", code)
	}

	if _, err := os.Stat(pairingLogPath(cfg, "sess-1")); !os.IsNotExist(err) {
		t.Errorf("expected no pairing log file to be created for an unpaired session, stat err = %v", err)
	}
}

func TestPairLogNoopsWhenPairingInactive(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	mgr.EnsureSession("sess-1", "agent-a")
	if err := mgr.SetPairing("sess-1", &session.Pairing{Active: false, Item: "T-001"}); err != nil {
		t.Fatal(err)
	}

	code := PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "command", Command: "echo hi"})
	if code != 0 {
		t.Errorf("PairLog with active:false returned %d, want 0", code)
	}
	if _, err := os.Stat(pairingLogPath(cfg, "sess-1")); !os.IsNotExist(err) {
		t.Errorf("expected no pairing log file for active:false pairing, stat err = %v", err)
	}
}

func TestPairLogUnknownSessionIsNoop(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)

	code := PairLog(cfg, mgr, "no-such-session", PairLogOpts{Type: "command", Command: "echo hi"})
	if code != 0 {
		t.Errorf("PairLog for an unknown session returned %d, want 0", code)
	}
}

func TestPairLogEmptySessionIDIsNoop(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)

	code := PairLog(cfg, mgr, "", PairLogOpts{Type: "command", Command: "echo hi"})
	if code != 0 {
		t.Errorf("PairLog with empty session id returned %d, want 0", code)
	}
}

func TestPairLogRejectsUnknownType(t *testing.T) {
	_, cfg := setupTestEnv(t)
	mgr := newTestSessionMgr(cfg)
	setPairingActive(t, mgr, "sess-1", "T-001")

	code := PairLog(cfg, mgr, "sess-1", PairLogOpts{Type: "bogus"})
	if code != 2 {
		t.Errorf("PairLog with an unknown type returned %d, want 2", code)
	}
}

func TestReadPairLogEventsHandlesMissingFile(t *testing.T) {
	_, cfg := setupTestEnv(t)
	events, err := readPairLogEvents(cfg, "no-such-session")
	if err != nil {
		t.Fatalf("readPairLogEvents on a missing file returned an error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events from a missing file, want 0", len(events))
	}
}

func TestReadPairLogEventsSkipsMalformedLines(t *testing.T) {
	_, cfg := setupTestEnv(t)
	path := pairingLogPath(cfg, "sess-1")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"command","command":"ok"}
not valid json
{"type":"observation","text":"still parses after a bad line"}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	events, err := readPairLogEvents(cfg, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (malformed line skipped): %+v", len(events), events)
	}
	if events[0].Command != "ok" || events[1].Text != "still parses after a bad line" {
		t.Errorf("unexpected events: %+v", events)
	}
}
