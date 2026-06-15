package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

func TestMatchesItemID(t *testing.T) {
	cases := []struct {
		base string
		id   string
		want bool
	}{
		{"T-123-foo-bar.md", "T-123", true},
		{"T-123.md", "T-123", true},
		{"I-456-long-title-with-words.md", "I-456", true},
		{"T-123-foo.md", "T-124", false},
		{"T-1234-foo.md", "T-123", false}, // prefix but different id
		{"T-123-foo.md", "T-12", false},   // partial match should not count
		{"config.yaml", "T-123", false},
		{"T-123-foo.md", "", false},
	}
	for _, c := range cases {
		got := matchesItemID(c.base, c.id)
		if got != c.want {
			t.Errorf("matchesItemID(%q, %q) = %v, want %v", c.base, c.id, got, c.want)
		}
	}
}

func TestSynthesizeBundleMessage_NoChange(t *testing.T) {
	// Only the expected item file is staged → message unchanged.
	cached := "tasks/T-100-alpha.md"
	msg := synthesizeBundleMessage(t.TempDir(), "st update: T-100.sbar.situation", cached)
	if msg != "st update: T-100.sbar.situation" {
		t.Errorf("expected unchanged message, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_ExtraItems(t *testing.T) {
	// Two unexpected item files staged alongside the expected one.
	cached := "tasks/T-100-alpha.md\nissues/I-200-beta.md\ntasks/T-300-gamma.md"
	msg := synthesizeBundleMessage(t.TempDir(), "st update: T-100.sbar.situation", cached)

	if !strings.HasPrefix(msg, "st sync batch: ") {
		t.Errorf("expected bundle message prefix, got %q", msg)
	}
	// All three item IDs must appear in the bundle message (bare IDs, no .md).
	for _, want := range []string{"T-100-alpha", "I-200-beta", "T-300-gamma"} {
		if !strings.Contains(msg, want) {
			t.Errorf("bundle message missing %q: %q", want, msg)
		}
	}
}

func TestSynthesizeBundleMessage_OnlyAutoStage(t *testing.T) {
	// Only auto-stage subdirs alongside the expected item — no cross-attribution.
	cached := "tasks/T-100-alpha.md\n.plans/I-594.md\n.changelog/2026-06-14.md"
	msg := synthesizeBundleMessage(t.TempDir(), "st update: T-100.sbar.situation", cached)
	if msg != "st update: T-100.sbar.situation" {
		t.Errorf("auto-stage dirs should not trigger bundle, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_NonUpdateMessage(t *testing.T) {
	// Non "st update:" message — function is a no-op.
	cached := "tasks/T-100-alpha.md\ntasks/T-200-beta.md"
	msg := synthesizeBundleMessage(t.TempDir(), "st sync", cached)
	if msg != "st sync" {
		t.Errorf("non-update message should pass through unchanged, got %q", msg)
	}
}

func TestSynthesizeBundleMessage_SingleItemMultipleAutoStage(t *testing.T) {
	// Multiple .plans files staged with one item update — still fine.
	cached := "issues/I-594-foo.md\n.plans/I-594.md\n.as/sessions/abc.yaml"
	msg := synthesizeBundleMessage(t.TempDir(), "st update: I-594.sbar.assessment", cached)
	if msg != "st update: I-594.sbar.assessment" {
		t.Errorf("auto-stage dirs should not trigger bundle, got %q", msg)
	}
}

// TestGitSync_CrossAttributionBundlesMessage is the integration-level guard for
// I-594: when parallel agents each write their item file before the git lock is
// acquired, `git add -u` stages both. GitSync must synthesize a bundle commit
// message that names all staged files instead of mis-attributing the commit to
// only the calling agent's item.
func TestGitSync_CrossAttributionBundlesMessage(t *testing.T) {
	root, _ := setupTestDir(t)
	asDir := filepath.Join(root, ".as")
	os.MkdirAll(asDir, 0755)
	os.WriteFile(filepath.Join(asDir, "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	initGitRepo(t, root)

	gitT := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// commitStagedOntoMain advances refs/heads/main; ensure the branch is main.
	gitT("branch", "-M", "main")

	cfg, _ := config.Load(root)
	cfg.Git = &config.GitConfig{AutoCommit: true, AutoPush: false}
	s, _ := New(cfg)

	// Simulate parallel agents: both T-001 and T-002 are modified in the
	// working tree before the lock is acquired. git add -u will stage both.
	item1, _ := s.Get("T-001")
	item1.Doc.SetField("status", "active")
	s.write(item1)

	item2, _ := s.Get("T-002")
	item2.Doc.SetField("status", "queued")
	s.write(item2)

	// GitSync is called with a message that only names T-001.
	if err := s.GitSync("st update: T-001.sbar.situation"); err != nil {
		t.Fatalf("GitSync: %v", err)
	}

	msg := gitT("log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "st sync batch: ") {
		t.Errorf("expected bundle commit message starting with 'st sync batch: ', got %q", msg)
	}
	if !strings.Contains(msg, "T-001") {
		t.Errorf("bundle message should mention T-001, got %q", msg)
	}
	if !strings.Contains(msg, "T-002") {
		t.Errorf("bundle message should mention T-002, got %q", msg)
	}
}
