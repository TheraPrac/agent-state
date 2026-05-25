package command

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

func TestShowRendersDroppedReasonForAbandoned(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	Create(s, cfg, "task", "Will show dropped reason", CreateOpts{Priority: 2})
	Start(s, cfg, "T-001", StartOpts{})

	if rc := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: "superseded"}); rc != 0 {
		t.Fatalf("Close rc=%d", rc)
	}

	// Reload store so the archived item is picked up.
	s2, _ := store.New(cfg)

	var buf bytes.Buffer
	item, _ := s2.Get("T-001")
	showDefaultTo(&buf, s2, cfg, "T-001", (*modelItemRef)(item))

	out := buf.String()
	if !strings.Contains(out, "dropped_reason: superseded") {
		t.Errorf("show output missing dropped_reason: superseded\n%s", out)
	}
}
