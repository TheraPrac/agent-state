package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// setupAbandonEnv returns a fresh env with one started task T-001 ready to abandon.
func setupAbandonEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	Create(s, cfg, "task", "Will abandon", CreateOpts{Priority: 2})
	Start(s, cfg, "T-001", StartOpts{})
	return s, cfg
}

func TestCloseAbandonRequiresReason(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: ""})
	if rc == 0 {
		t.Error("Close abandoned with no reason should return non-zero")
	}
}

func TestCloseAbandonRejectsInvalidVocab(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: "no longer needed"})
	if rc == 0 {
		t.Error("Close abandoned with invalid reason should return non-zero")
	}
}

func TestCloseAbandonRejectsAgedReason(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: "aged"})
	if rc == 0 {
		t.Error("Close abandoned with reason=aged should return non-zero")
	}
}

func TestCloseAbandonAcceptsAllVocabReasons(t *testing.T) {
	for _, reason := range model.ValidDropReasons {
		t.Run(reason, func(t *testing.T) {
			root := setupTestEnvRoot(t)
			cfg, _ := config.Load(root)
			s, _ := store.New(cfg)
			os.MkdirAll(cfg.ChangelogDir(), 0755)
			Create(s, cfg, "task", "Will abandon "+reason, CreateOpts{Priority: 2})
			Start(s, cfg, "T-001", StartOpts{})

			rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: reason})
			if rc != 0 {
				t.Errorf("Close abandoned reason=%q rc=%d, want 0", reason, rc)
			}
			item, _ := s.Get("T-001")
			if item.Status != "abandoned" {
				t.Errorf("status = %q, want abandoned", item.Status)
			}
			if item.DroppedReason != reason {
				t.Errorf("DroppedReason = %q, want %q", item.DroppedReason, reason)
			}
		})
	}
}

func TestCloseAbandonWritesDroppedReasonField(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: "superseded"})
	if rc != 0 {
		t.Fatalf("Close rc=%d", rc)
	}

	// Find the archived file and verify dropped_reason appears on disk.
	archiveDir := filepath.Join(cfg.ItemDir(), "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("ReadDir archive: %v", err)
	}
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "T-001") {
			data, _ := os.ReadFile(filepath.Join(archiveDir, e.Name()))
			if strings.Contains(string(data), "dropped_reason: superseded") {
				found = true
			}
		}
	}
	if !found {
		t.Error("dropped_reason: superseded not found in archived item file")
	}
}

func TestCloseDeclinedDoesNotEnforceVocab(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Create an idea (ideas use D- prefix, lifecycle: captured → declined).
	Create(s, cfg, "idea", "A wild idea", CreateOpts{})
	rc := Close(s, cfg, "D-001", "declined", CloseOpts{Reason: "captured elsewhere"})
	if rc != 0 {
		t.Errorf("Close declined with free-form reason rc=%d, want 0 (declined exempt from vocab gate)", rc)
	}
}
