package command

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// tagTestEnv creates a workspace with a goal and an issue for tag routing tests.
func tagTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	_, cfg := setupTestEnv(t)

	ensureGoalsDir(t, cfg)
	seedGoalFile(t, cfg, "G-099", "active", 10)

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New after seeding goal: %v", err)
	}
	return s, cfg
}

func TestIsGoalID(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"G-014", true},
		{"G-1", true},
		{"G-9999", true},
		{"G-", false},
		{"G", false},
		{"g-014", false},
		{"post-alpha", false},
		{"alpha-1", false},
		{"I-014", false},
		{"T-014", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGoalID(c.s); got != c.want {
			t.Errorf("isGoalID(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestTag_GoalAdd_RoutesToGoalsField(t *testing.T) {
	s, cfg := tagTestEnv(t)

	var stdout bytes.Buffer
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w

	rc := Tag(s, cfg, "T-001", "add", "G-099")

	w.Close()
	os.Stdout = origStdout
	stdout.ReadFrom(r)
	out := stdout.String()

	if rc != 0 {
		t.Fatalf("Tag add G-099 exit %d, stdout: %s", rc, out)
	}
	if !strings.Contains(out, "goals:") {
		t.Errorf("output should mention goals:, got: %s", out)
	}

	s2, _ := store.New(cfg)
	item, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found after tag add")
	}
	if !sliceHas(item.Goals, "G-099") {
		t.Errorf("Goals = %v, want G-099", item.Goals)
	}
	if sliceHas(item.Tags, "G-099") {
		t.Errorf("G-099 must not appear in Tags: %v", item.Tags)
	}
}

func TestTag_GoalAdd_DuplicateRejected(t *testing.T) {
	s, cfg := tagTestEnv(t)

	if rc := Tag(s, cfg, "T-001", "add", "G-099"); rc != 0 {
		t.Fatalf("first add: exit %d", rc)
	}

	s2, _ := store.New(cfg)
	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	rc2 := Tag(s2, cfg, "T-001", "add", "G-099")
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if rc2 == 0 {
		t.Error("duplicate goal add should return non-zero")
	}
	if !strings.Contains(buf.String(), "already has goal") {
		t.Errorf("expected 'already has goal' error, got: %s", buf.String())
	}
}

func TestTag_GoalAdd_NonExistentGoalRejected(t *testing.T) {
	s, cfg := tagTestEnv(t)

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	rc := Tag(s, cfg, "T-001", "add", "G-9999")
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if rc == 0 {
		t.Error("adding non-existent goal should return non-zero")
	}
	if !strings.Contains(buf.String(), "goal not found") {
		t.Errorf("expected 'goal not found' error, got: %s", buf.String())
	}
}

func TestTag_GoalRm_RemovesFromGoalsField(t *testing.T) {
	s, cfg := tagTestEnv(t)

	if rc := Tag(s, cfg, "T-001", "add", "G-099"); rc != 0 {
		t.Fatalf("add: exit %d", rc)
	}
	s2, _ := store.New(cfg)
	if rc := Tag(s2, cfg, "T-001", "rm", "G-099"); rc != 0 {
		t.Fatalf("rm: exit %d", rc)
	}

	s3, _ := store.New(cfg)
	item, _ := s3.Get("T-001")
	if sliceHas(item.Goals, "G-099") {
		t.Errorf("G-099 still in Goals after rm: %v", item.Goals)
	}
}

func TestTag_GoalRm_FallsBackToTagsForLegacyEntry(t *testing.T) {
	s, cfg := tagTestEnv(t)

	// Simulate legacy state: G-099 in tags: only (goal was placed via old code path).
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Tags = append(it.Tags, "G-099")
		it.Doc.SetList("tags", it.Tags)
		return nil
	}); err != nil {
		t.Fatalf("seed legacy tag: %v", err)
	}

	s2, _ := store.New(cfg)
	if rc := Tag(s2, cfg, "T-001", "rm", "G-099"); rc != 0 {
		t.Fatalf("rm legacy goal tag: exit %d", rc)
	}

	s3, _ := store.New(cfg)
	item, _ := s3.Get("T-001")
	if sliceHas(item.Tags, "G-099") {
		t.Errorf("G-099 still in Tags after rm: %v", item.Tags)
	}
	if sliceHas(item.Goals, "G-099") {
		t.Errorf("G-099 should not be in Goals: %v", item.Goals)
	}
}

func TestTag_NonGoal_StillWritesToTagsField(t *testing.T) {
	s, cfg := tagTestEnv(t)

	if rc := Tag(s, cfg, "T-001", "add", "post-alpha"); rc != 0 {
		t.Fatalf("add post-alpha: exit %d", rc)
	}

	s2, _ := store.New(cfg)
	item, _ := s2.Get("T-001")
	if !sliceHas(item.Tags, "post-alpha") {
		t.Errorf("post-alpha not in Tags: %v", item.Tags)
	}
	if sliceHas(item.Goals, "post-alpha") {
		t.Errorf("post-alpha must not appear in Goals: %v", item.Goals)
	}
}

func TestTag_GoalAdd_ChangelogRecordsGoalsField(t *testing.T) {
	s, cfg := tagTestEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-test")

	if rc := Tag(s, cfg, "T-001", "add", "G-099"); rc != 0 {
		t.Fatalf("add: exit %d", rc)
	}

	clPath := filepath.Join(cfg.Root(), ".as", "changelog.jsonl")
	data, err := os.ReadFile(clPath)
	if err != nil {
		t.Skipf("no changelog at %s: %v", clPath, err)
	}
	if !strings.Contains(string(data), `"goals"`) {
		t.Errorf("changelog does not record field=goals:\n%s", string(data))
	}
}

func TestTag_SingleIDBackwardCompat(t *testing.T) {
	s, cfg := tagTestEnv(t)

	rc := Tag(s, cfg, "T-001", "add", "post-alpha")
	if rc != 0 {
		t.Fatalf("single-ID Tag returned %d", rc)
	}

	s2, _ := store.New(cfg)
	item, _ := s2.Get("T-001")
	if !sliceHas(item.Tags, "post-alpha") {
		t.Errorf("post-alpha not in Tags after single-ID Tag: %v", item.Tags)
	}
}

func TestTagMany_BatchesMultipleIDsInOneSync(t *testing.T) {
	// setupTestEnv seeds T-001, T-002, T-003 already.
	s, cfg := setupTestEnv(t)

	syncCount := 0
	orig := autoSyncGitFn
	defer func() { autoSyncGitFn = orig }()
	autoSyncGitFn = func(_ *store.Store, _ string, _ ...string) error {
		syncCount++
		return nil
	}

	rc := TagMany(s, cfg, []string{"T-001", "T-002", "T-003"}, "add", "batch-tag")
	if rc != 0 {
		t.Fatalf("TagMany returned %d", rc)
	}
	if syncCount != 1 {
		t.Errorf("expected exactly 1 autoSync call, got %d", syncCount)
	}

	s2, _ := store.New(cfg)
	for _, id := range []string{"T-001", "T-002", "T-003"} {
		item, _ := s2.Get(id)
		if !sliceHas(item.Tags, "batch-tag") {
			t.Errorf("%s missing batch-tag after TagMany, tags: %v", id, item.Tags)
		}
	}
}

func TestAutoSync_RetriesThenHardFailsOnGitLockTimeout(t *testing.T) {
	_, cfg := setupTestEnv(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	attempts := 0
	orig := autoSyncGitFn
	defer func() { autoSyncGitFn = orig }()
	autoSyncGitFn = func(_ *store.Store, _ string, _ ...string) error {
		attempts++
		return fmt.Errorf("%w after 5s", store.ErrGitLockTimeout)
	}

	err = autoSync(s, "test-msg")
	if err == nil {
		t.Fatal("expected non-nil error after exhausted retries, got nil")
	}
	if !errors.Is(err, store.ErrGitLockTimeout) {
		t.Errorf("expected errors.Is(err, ErrGitLockTimeout), got %v", err)
	}
	if attempts != autoSyncMaxRetries {
		t.Errorf("expected %d attempts, got %d", autoSyncMaxRetries, attempts)
	}
}

// sliceHas reports whether s appears in slice.
func sliceHas(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
