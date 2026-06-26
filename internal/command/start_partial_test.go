package command

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
)

// I-1477(f): detectPartialStarts flags an item ONLY when a worktree dir exists
// for it AND the item is still in its START status (queued) — i.e. `st start`
// was interrupted before the status flip/stack push. Fully-started (active) and
// finished (terminal) items are never flagged, regardless of .workinfo state.
func TestDetectPartialStarts(t *testing.T) {
	// setupTestEnv seeds T-001 (queued), T-002 (queued), T-003 (active).
	s, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{
		Enabled: true, BaseDir: "wt", ParentDir: t.TempDir(), Repos: []string{"theraprac-api"},
	}
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase empty")
	}

	// Worktree dirs: T-001 queued → partial; T-003 active → not; T-999 no such
	// item → skip; scratch not item-shaped → ignored. (T-002 has no worktree.)
	for _, d := range []string{"T-001", "T-003", "T-999", "scratch"} {
		if err := os.MkdirAll(filepath.Join(wtBase, d, "theraprac-api"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	got := detectPartialStarts(s, cfg)
	want := []string{"T-001"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detectPartialStarts = %v, want %v", got, want)
	}
}

func TestLooksLikeItemDir(t *testing.T) {
	cases := map[string]bool{
		"I-1477": true, "T-1": true, "I-0": true,
		"I-": false, "X-12": false, "I-12a": false, "scratch": false, "": false,
	}
	for name, want := range cases {
		if got := looksLikeItemDir(name); got != want {
			t.Errorf("looksLikeItemDir(%q) = %v, want %v", name, got, want)
		}
	}
}
