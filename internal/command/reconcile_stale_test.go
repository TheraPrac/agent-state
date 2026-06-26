package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// staleFetchNone is a no-op PRFetch for stale-active tests where no PR exists.
func staleFetchNone(cfg *config.Config, branch string) (string, []string) {
	return "NONE", nil
}

// makeActiveWithAssignee sets an item to active and assigns it.
func makeActiveWithAssignee(t *testing.T, s interface {
	Mutate(string, func(*model.Item) error) error
}, id, assignee string) {
	t.Helper()
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		if assignee != "" {
			it.Doc.SetField("assigned_to", assignee)
			it.AssignedTo = assignee
		}
		return nil
	}); err != nil {
		t.Fatalf("mutate %s: %v", id, err)
	}
}

// writeChangelogEntry writes a single changelog entry with the given timestamp
// to <cfg.ChangelogDir()>/<id>.log.
func writeChangelogEntry(t *testing.T, cfg *config.Config, id string, ts time.Time) {
	t.Helper()
	dir := cfg.ChangelogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	entry := map[string]string{
		"timestamp": ts.Format(time.RFC3339),
		"op":        "start",
		"agent":     "agent-test",
	}
	data, _ := json.Marshal(entry)
	path := filepath.Join(dir, id+".log")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write changelog %s: %v", path, err)
	}
}

// defaultStaleOpts returns ReconcileOpts for stale-active tests.
func defaultStaleOpts() ReconcileOpts {
	return ReconcileOpts{
		ToolCheck: func(string) bool { return true },
		PRFetch:   staleFetchNone,
	}
}

// ---- changelogLatestTimestamp unit tests ----------------------------------

func TestChangelogLatestTimestamp_Happy(t *testing.T) {
	_, cfg := setupTestEnv(t)
	ts := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	writeChangelogEntry(t, cfg, "T-001", ts)

	got, ok := changelogLatestTimestamp(cfg, "T-001")
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if !got.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", got, ts)
	}
}

func TestChangelogLatestTimestamp_Missing(t *testing.T) {
	_, cfg := setupTestEnv(t)
	_, ok := changelogLatestTimestamp(cfg, "T-001")
	if ok {
		t.Error("expected ok=false for missing changelog, got true")
	}
}

func TestChangelogLatestTimestamp_UsesLastLine(t *testing.T) {
	_, cfg := setupTestEnv(t)
	dir := cfg.ChangelogDir()
	os.MkdirAll(dir, 0o755)

	old := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Second)
	recent := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)

	path := filepath.Join(dir, "T-001.log")
	for _, ts := range []time.Time{old, recent} {
		entry := map[string]string{"timestamp": ts.Format(time.RFC3339), "op": "test_recorded"}
		data, _ := json.Marshal(entry)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.Write(append(data, '\n'))
		f.Close()
	}

	got, ok := changelogLatestTimestamp(cfg, "T-001")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !got.Equal(recent) {
		t.Errorf("timestamp = %v, want most-recent %v", got, recent)
	}
}

// ---- reconcileStaleActive integration tests -------------------------------

// TestReconcileStaleActive_ReleasesAfterThreshold — item with no changelog
// activity past the 6h threshold and no live agent → released.
// Uses the staleLastTouched + fresh store pattern to avoid Mutate-vs-List
// cache ordering issues (matches the existing stale-active test style). I-874.
func TestReconcileStaleActive_ReleasesAfterThreshold(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_HOURS", "6")
	_, cfg := setupTestEnv(t)

	// Backdate T-003 (active fixture) so it looks stale, then protect it.
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)
	writeChangelogEntry(t, cfg, "T-003", time.Now().Add(-1*time.Hour))

	// Use T-003 as the test subject (it's already active in the fixture).
	// Write a stale changelog (7h ago).
	writeChangelogEntry(t, cfg, "T-003", time.Now().Add(-7*time.Hour))
	// Overwrite with ONLY the stale entry by rewriting the file.
	dir := cfg.ChangelogDir()
	os.MkdirAll(dir, 0o755)
	entry := map[string]string{"timestamp": time.Now().Add(-7 * time.Hour).Format(time.RFC3339), "op": "start"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(dir, "T-003.log"), append(data, '\n'), 0o644)

	freshStore := newStoreOrFail(t, cfg)
	n := reconcileStaleActive(freshStore, cfg, defaultStaleOpts())
	if n != 1 {
		t.Errorf("updates = %d, want 1", n)
	}
	postStore := newStoreOrFail(t, cfg)
	item, _ := postStore.Get("T-003")
	if item.Status == "active" {
		t.Error("item still active after release")
	}
}

// TestReconcileStaleActive_KeepsIfRecentChangelog — item with changelog
// activity within the 6h threshold → kept active. I-874.
func TestReconcileStaleActive_KeepsIfRecentChangelog(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_HOURS", "6")
	_, cfg := setupTestEnv(t)
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)

	// T-003 has last_touched 30d ago but a recent changelog entry (2h ago).
	writeChangelogEntry(t, cfg, "T-003", time.Now().Add(-2*time.Hour))

	freshStore := newStoreOrFail(t, cfg)
	n := reconcileStaleActive(freshStore, cfg, defaultStaleOpts())
	if n != 0 {
		t.Errorf("updates = %d, want 0 (recent changelog protects)", n)
	}
}

// TestReconcileStaleActive_ReleasesIfNoChangelogNoAgent — no changelog file
// and last_touched is old → released. I-874.
func TestReconcileStaleActive_ReleasesIfNoChangelogNoAgent(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_HOURS", "6")
	_, cfg := setupTestEnv(t)
	staleLastTouched(t, cfg, "T-003", 10*time.Hour)

	// No changelog file exists for T-003.
	os.Remove(filepath.Join(cfg.ChangelogDir(), "T-003.log"))

	freshStore := newStoreOrFail(t, cfg)
	n := reconcileStaleActive(freshStore, cfg, defaultStaleOpts())
	if n != 1 {
		t.Errorf("updates = %d, want 1 (no changelog, old last_touched)", n)
	}
}

// TestReconcileStaleActive_SkipsIfAssignedAgentLive — item assigned to an
// agent that has a live PID registration → not released (safety gate). I-874.
func TestReconcileStaleActive_SkipsIfAssignedAgentLive(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_HOURS", "6")
	s, cfg := setupTestEnv(t)
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour)
	// No changelog for T-003 (stale).

	// Re-assign T-003 to "agent-live" on disk.
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Doc.SetField("assigned_to", "agent-live")
		it.AssignedTo = "agent-live"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}
	staleLastTouched(t, cfg, "T-003", 30*24*time.Hour) // re-backdate after Mutate stomped it

	// Write a live agent registration for "agent-live" using our own PID.
	agentsDir := cfg.AgentsDir()
	os.MkdirAll(agentsDir, 0o755)
	regContent := fmt.Sprintf("agent_id: agent-live\npid: %d\nroot: %s\nstarted: 2026-06-01T00:00:00Z\n",
		os.Getpid(), cfg.Root())
	os.WriteFile(filepath.Join(agentsDir, "agent-live.yaml"), []byte(regContent), 0o644)

	if !agent.IsPIDLive(os.Getpid()) {
		t.Skip("own PID not live — skipping")
	}

	freshStore := newStoreOrFail(t, cfg)
	n := reconcileStaleActive(freshStore, cfg, defaultStaleOpts())
	if n != 0 {
		t.Errorf("updates = %d, want 0 (assigned agent is live)", n)
	}
	postStore := newStoreOrFail(t, cfg)
	item, _ := postStore.Get("T-003")
	if item.Status != "active" {
		t.Errorf("item status = %q, want active (live agent protects)", item.Status)
	}
}

// TestReconcileStaleActiveHours_ConfigOverridesDefault — sprints.stale_active_hours
// in config overrides the 6h default. I-874.
func TestReconcileStaleActiveHours_ConfigOverridesDefault(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Sprints = &config.SprintsConfig{StaleActiveHours: 12}
	if got := cfg.StaleActiveHours(); got != 12 {
		t.Errorf("StaleActiveHours() = %d, want 12", got)
	}
}

func TestReconcileStaleActiveHours_EnvVar(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_HOURS", "3")
	_, cfg := setupTestEnv(t)
	if got := cfg.StaleActiveHours(); got != 3 {
		t.Errorf("StaleActiveHours() = %d, want 3 (from env)", got)
	}
}

func TestReconcileStaleActiveHours_DaysEnvFallback(t *testing.T) {
	t.Setenv("ST_STALE_ACTIVE_DAYS", "2")
	_, cfg := setupTestEnv(t)
	if got := cfg.StaleActiveHours(); got != 48 {
		t.Errorf("StaleActiveHours() = %d, want 48 (2 days)", got)
	}
}

func TestReconcileStaleActiveHours_Default(t *testing.T) {
	_, cfg := setupTestEnv(t)
	if got := cfg.StaleActiveHours(); got != 6 {
		t.Errorf("StaleActiveHours() = %d, want 6 (default)", got)
	}
}
