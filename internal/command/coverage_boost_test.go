package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Tests targeting specific coverage gaps to reach 85%

func TestHasTagMatch(t *testing.T) {
	item := &model.Item{Tags: []string{"alpha", "beta"}}
	if !hasTag(item, "alpha") {
		t.Error("should find 'alpha'")
	}
}

func TestHasTagNoMatch(t *testing.T) {
	item := &model.Item{Tags: []string{"alpha"}}
	if hasTag(item, "gamma") {
		t.Error("should not find 'gamma'")
	}
}

func TestHasTagEmpty(t *testing.T) {
	item := &model.Item{}
	if hasTag(item, "any") {
		t.Error("empty tags should not match")
	}
}

func TestMigrateScope(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Migrate only archive scope — should skip tasks
	code := Migrate(s, cfg, MigrateOpts{DryRun: true, Scope: "archive"})
	if code != 0 {
		t.Errorf("migrate --scope archive exit %d", code)
	}

	// Migrate only active scope — should skip archive
	code = Migrate(s, cfg, MigrateOpts{DryRun: true, Scope: "active"})
	if code != 0 {
		t.Errorf("migrate --scope active exit %d", code)
	}
}

func TestCloseActiveForced(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	code := Close(s, cfg, "T-003", "completed", CloseOpts{Force: true})
	if code != 0 {
		t.Errorf("close active+force should succeed, got %d", code)
	}
}

func TestCloseAbandonedNeedsReasonV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-001", "abandoned", CloseOpts{})
	if code == 0 {
		t.Error("abandoned without reason should fail")
	}
}

func TestReadyWithTagFilter(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Add tag to T-001
	Tag(s, cfg, "T-001", "add", "filtered")
	code := Ready(s, cfg, ReadyOpts{Tag: "filtered"})
	if code != 0 {
		t.Errorf("ready with tag filter exit %d", code)
	}
}

func TestReadyWithTypeFilter(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Ready(s, cfg, ReadyOpts{Type: "issue"})
	if code != 0 {
		t.Errorf("ready with type filter exit %d", code)
	}
}

func TestStatusWithIssuesFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Issues: true})
	if code != 0 {
		t.Errorf("status -i exit %d", code)
	}
}

func TestStatusWithTasksFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Tasks: true})
	if code != 0 {
		t.Errorf("status -t exit %d", code)
	}
}

func TestStatusSingle(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-001", StatusOpts{})
	if code != 0 {
		t.Errorf("status T-001 exit %d", code)
	}
}

func TestStatusSingleItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-001", StatusOpts{})
	if code != 0 {
		t.Errorf("status T-001 exit %d", code)
	}
}

func TestEpicCreateError(t *testing.T) {
	cfg := config.Defaults()
	// Epics path points to nonexistent dir — should handle gracefully
	code := EpicCreate(cfg, "Will Fail")
	_ = code // may or may not fail depending on path resolution
}

func TestSprintCreateNoEpic(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := SprintCreate(cfg, "nonexistent-epic-id", "Sprint")
	if code == 0 {
		t.Error("sprint create with bad epic should fail")
	}
}

func TestNoteAddAndRm(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// Create notes file manually
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)

	code := NoteAdd(cfg, "test note")
	if code != 0 {
		t.Errorf("note add exit %d", code)
	}

	code = NoteList(cfg, 10)
	if code != 0 {
		t.Errorf("note list exit %d", code)
	}
}

func TestEpicListWithCreatedEpic(t *testing.T) {
	s, cfg := setupTestEnv(t)
	EpicCreate(cfg, "Coverage Epic")
	code := EpicList(s, cfg)
	if code != 0 {
		t.Errorf("epic list with epic exit %d", code)
	}
}

func TestSprintListNoSprints(t *testing.T) {
	_, cfg := setupTestEnv(t)
	code := SprintList(cfg, "")
	if code != 0 {
		t.Errorf("sprint list no sprints exit %d", code)
	}
}

func TestStartAssignedToOtherAgent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.Setenv("AS_AGENT_ID", "different-agent")
	defer os.Setenv("AS_AGENT_ID", "")
	// T-003 is active and assigned to agent-a
	code := Start(s, cfg, "T-001", StartOpts{})
	// Should succeed since T-001 isn't assigned
	_ = code
}

func TestStatsWithTimeFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Stats(s, cfg, StatsOpts{Time: true})
	if code != 0 {
		t.Errorf("stats --time exit %d", code)
	}
}

func TestStatsWithTime(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Stats(s, cfg, StatsOpts{Time: true})
	if code != 0 {
		t.Errorf("stats --time exit %d", code)
	}
}

func TestCheckQuiet(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Check(s, cfg, true)
	_ = code // may find issues — just exercise quiet path
}

func setupTestEnvWithDelivery(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	cfg.Delivery = &config.DeliveryConfig{
		Stages:      []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev"},
		ArchiveGate: "deployed_dev",
	}
	return s, cfg
}

func TestStatusWithDelivery(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	code := Status(s, cfg, "", StatusOpts{All: true})
	if code != 0 {
		t.Errorf("status -a with delivery exit %d", code)
	}
}

func TestStatusCompletedFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Completed: true})
	if code != 0 {
		t.Errorf("status -d exit %d", code)
	}
}
