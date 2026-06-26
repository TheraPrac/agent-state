package command

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// I-1477(f): detectPartialStarts flags item worktree dirs left without a
// .workinfo marker (interrupted `st start`), and only those.
func TestDetectPartialStarts(t *testing.T) {
	_, cfg := setupTestEnv(t)
	base := t.TempDir()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		BaseDir:   "wt",
		ParentDir: base,
		Repos:     []string{"repo"},
	}
	wtBase := cfg.WorktreeBase()
	if wtBase == "" {
		t.Fatal("WorktreeBase() empty")
	}

	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{wtBase}, parts...)...), 0755); err != nil {
			t.Fatalf("mkdir %v: %v", parts, err)
		}
	}
	touch := func(parts ...string) {
		if err := os.WriteFile(filepath.Join(append([]string{wtBase}, parts...)...), []byte("x"), 0644); err != nil {
			t.Fatalf("write %v: %v", parts, err)
		}
	}

	// I-9001: interrupted — has a repo subdir but NO .workinfo → flagged.
	mk("I-9001", "theraprac-api")
	// I-9002: complete — repo subdir AND .workinfo → not flagged.
	mk("I-9002", "theraprac-api")
	touch("I-9002", ".workinfo")
	// I-9003: empty leftover dir, no subdir → not flagged (noise).
	mk("I-9003")
	// not-an-item: wrong shape → ignored.
	mk("scratch", "theraprac-api")

	got := detectPartialStarts(cfg)
	want := []string{"I-9001"}
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
