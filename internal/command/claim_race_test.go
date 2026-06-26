package command

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// setupClaimRaceEnv builds a Config + Store with one task in queued state,
// no git, no worktree — focused on T-310's claim race semantics.
// Returns (s, cfg, itemID, itemFilePath).
func setupClaimRaceEnv(t *testing.T) (*store.Store, *config.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".as/sessions"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	itemPath := filepath.Join(root, "tasks", "T-001-x.md")
	if err := os.WriteFile(itemPath, []byte(`id: T-001
type: task
status: queued
created: 2026-04-26T10:00:00-06:00
last_touched: 2026-04-26T10:00:00-06:00

completed: null

title: Test item

depends_on:
- []
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s, cfg, "T-001", itemPath
}

// stampClaim writes claimed_by/claimed_at directly via Mutate so a test
// can simulate a pre-existing claim without driving the full Start path.
func stampClaim(t *testing.T, s *store.Store, id, sessionID string) {
	t.Helper()
	if err := s.Mutate(id, func(it *model.Item) error {
		it.ClaimedBy = sessionID
		it.ClaimedAt = "2026-04-26T10:00:00-06:00"
		it.Doc.SetField("claimed_by", sessionID)
		it.Doc.SetField("claimed_at", "2026-04-26T10:00:00-06:00")
		return nil
	}); err != nil {
		t.Fatalf("stampClaim: %v", err)
	}
}

// TestClaimMutateOnly: visible proof that the claim landed on disk via
// Mutate (which means it went through the item flock). Reading the file
// and confirming claimed_by is the simplest end-to-end check.
func TestClaimMutateOnly(t *testing.T) {
	s, _, id, itemPath := setupClaimRaceEnv(t)

	if err := s.Mutate(id, func(it *model.Item) error {
		it.ClaimedBy = "sess-mutate"
		it.Doc.SetField("claimed_by", "sess-mutate")
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	body, err := os.ReadFile(itemPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "claimed_by: sess-mutate") {
		t.Errorf("on-disk file missing claimed_by from Mutate:\n%s", body)
	}
}

// TestStartClaimRace: a concurrent claim attempt against a live rival
// must observe ErrAlreadyClaimed and refuse to overwrite. Without the
// Mutate-internal check, Start's pre-flight read could pass and then
// stamp on top of the rival.
func TestStartClaimRace(t *testing.T) {
	s, cfg, id, itemPath := setupClaimRaceEnv(t)

	// Live rival: registration with this test process's PID, then a
	// stamped claim referencing the rival's session.
	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-rival",
		SessionID:   "sess-rival",
		PID:         os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	stampClaim(t, s, id, "sess-rival")

	// Reload store so the in-memory state reflects the seeded claim.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}

	// Drive the Mutate-internal claim check the same way Start does.
	mySession := "sess-us"
	err = s2.Mutate(id, func(it *model.Item) error {
		if it.ClaimedBy != "" && it.ClaimedBy != mySession {
			if isSessionLive(cfg, it.ClaimedBy) {
				return store.ErrAlreadyClaimed
			}
		}
		it.ClaimedBy = mySession
		it.Doc.SetField("claimed_by", mySession)
		return nil
	})
	if err == nil {
		t.Fatal("expected ErrAlreadyClaimed against a live rival, got nil")
	}
	if err != store.ErrAlreadyClaimed {
		t.Fatalf("expected ErrAlreadyClaimed, got %v", err)
	}

	// Rival's claim is intact on disk — we did not overwrite.
	body, _ := os.ReadFile(itemPath)
	if !strings.Contains(string(body), "claimed_by: sess-rival") {
		t.Errorf("rival claim was overwritten — race detection failed:\n%s", body)
	}
}

// TestStaleClaimSweep: a claim left by a session whose process is gone
// must be released by SweepStaleClaims. The agent registry is the
// liveness source of truth; a session with no entry is dead by definition.
func TestStaleClaimSweep(t *testing.T) {
	s, cfg, id, itemPath := setupClaimRaceEnv(t)

	stampClaim(t, s, id, "sess-ghost") // no agent.Register → no liveness entry

	released, err := store.SweepStaleClaims(s, cfg, sessionLiveProbe(cfg))
	if err != nil {
		t.Fatalf("SweepStaleClaims: %v", err)
	}
	if len(released) != 1 || released[0] != id {
		t.Fatalf("expected sweep to release [%s], got %v", id, released)
	}
	body, _ := os.ReadFile(itemPath)
	if strings.Contains(string(body), "claimed_by: sess-ghost") {
		t.Errorf("on-disk file still names the dead session:\n%s", body)
	}
}

// TestStaleClaimSweepRespectsLiveClaims: the sweep must NOT release a
// claim whose owning PID is alive. This is the "do not race a legitimate
// claim" invariant.
func TestStaleClaimSweepRespectsLiveClaims(t *testing.T) {
	s, cfg, id, itemPath := setupClaimRaceEnv(t)

	if _, _, err := agent.Register(cfg, agent.Options{
		BaseAgentID: "agent-live",
		SessionID:   "sess-alive",
		PID:         os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	stampClaim(t, s, id, "sess-alive")

	released, err := store.SweepStaleClaims(s, cfg, sessionLiveProbe(cfg))
	if err != nil {
		t.Fatalf("SweepStaleClaims: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("expected no releases for live claim, got %v", released)
	}
	body, _ := os.ReadFile(itemPath)
	if !strings.Contains(string(body), "claimed_by: sess-alive") {
		t.Errorf("live claim was released:\n%s", body)
	}
}

// TestStartClaimRaceConcurrent: real concurrent Mutate calls on the
// same item, each from a "different process". Mutate's flock serializes
// them — exactly one wins, the rest see ErrAlreadyClaimed.
//
// Test geometry: all N goroutines pre-register their session with the
// test PID (so registry lookups during Mutate find them live), then a
// single barrier releases them simultaneously into the Mutate. Cleanup
// is held until ALL Mutates complete — in real life each st process
// stays alive for the duration of its claim, so cleanup-during-rival-
// Mutate is not a real-world race.
func TestStartClaimRaceConcurrent(t *testing.T) {
	s, cfg, id, _ := setupClaimRaceEnv(t)

	const N = 4
	sessions := make([]string, N)
	cleanups := make([]func(), N)
	for i := 0; i < N; i++ {
		sess := "sess-c-" + strings.Repeat("x", i+1)
		sessions[i] = sess
		_, cleanup, err := agent.Register(cfg, agent.Options{
			BaseAgentID: "agent-concurrent",
			SessionID:   sess,
			PID:         os.Getpid(),
		})
		if err != nil {
			t.Fatalf("register %s: %v", sess, err)
		}
		cleanups[i] = cleanup
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	var (
		wg       sync.WaitGroup
		ok       atomic.Int32
		conflict atomic.Int32
		start    = make(chan struct{})
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		sess := sessions[i]
		go func() {
			defer wg.Done()
			<-start // synchronize entry into Mutate
			err := s.Mutate(id, func(it *model.Item) error {
				if it.ClaimedBy != "" && it.ClaimedBy != sess {
					if isSessionLive(cfg, it.ClaimedBy) {
						return store.ErrAlreadyClaimed
					}
				}
				it.ClaimedBy = sess
				it.Doc.SetField("claimed_by", sess)
				return nil
			})
			switch {
			case err == nil:
				ok.Add(1)
			case err == store.ErrAlreadyClaimed:
				conflict.Add(1)
			default:
				t.Errorf("unexpected Mutate error from %s: %v", sess, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != 1 {
		t.Errorf("expected exactly 1 successful claim, got %d", ok.Load())
	}
	if conflict.Load() != int32(N-1) {
		t.Errorf("expected %d conflicts, got %d", N-1, conflict.Load())
	}
}
