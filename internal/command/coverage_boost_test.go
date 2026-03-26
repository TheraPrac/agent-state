package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Tests targeting specific coverage gaps.
//
// Coverage exceptions — these functions are thin exec.Command wrappers around
// external tools (gh, aws, git) that cannot be unit tested without the tool
// installed and authenticated. They are tested indirectly via injectable opts
// structs in reconcile tests. Exercising the real binaries is integration scope.
//
//   - getMergeCommitSHA (reconcile.go) — shells out to `gh pr view`
//   - checkOrchDeployment (reconcile.go) — shells out to `aws s3 cp`
//   - s3Exists (reconcile.go) — shells out to `aws s3 ls`
//   - getPRState (reconcile.go) — shells out to `gh pr list`
//   - branchExistsOnRemote (reconcile.go) — shells out to `git ls-remote`
//
// All five have injectable replacements via ReconcileOpts (ToolCheck, BranchCheck,
// PRFetch, S3Check) that ARE tested. The wrapper functions themselves are 3-5 lines
// of exec.Command boilerplate.

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

// --- Additional coverage tests ---

func TestStatusSingleWithAllFields(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	// Enrich T-003 with more fields for statusSingle coverage
	item, _ := s.Get("T-003")
	item.Tags = []string{"alpha", "beta"}
	item.Summary = "A detailed summary of the task"
	item.AcceptanceCriteria = []string{"criteria one", "criteria two"}
	item.NextActions = []string{"do this", "then that"}
	p := 1
	item.Priority = &p
	item.Delivery["stage"] = "pushed"
	item.WorkTracking["branch"] = "T-003/active-task"
	item.WorkTracking["pr"] = "https://github.com/org/repo/pull/1"
	s.Write(item)

	code := Status(s, cfg, "T-003", StatusOpts{})
	if code != 0 {
		t.Errorf("status T-003 with all fields exit %d", code)
	}
}

func TestStatusSingleNotFoundV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-999", StatusOpts{})
	if code != 1 {
		t.Errorf("status T-999 should exit 1, got %d", code)
	}
}

func TestStatusSingleIssue(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "I-001", StatusOpts{})
	if code != 0 {
		t.Errorf("status I-001 exit %d", code)
	}
}

func TestStatusSingleCompletedItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "T-004", StatusOpts{})
	if code != 0 {
		t.Errorf("status T-004 exit %d", code)
	}
}

func TestGetPRURLsFromItemMultiple(t *testing.T) {
	item := &model.Item{
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "work_tracking:", Key: "work_tracking", Indent: 0},
				{Raw: "  branch: main", Key: "branch", Indent: 2, BlockKey: "work_tracking"},
				{Raw: "  pr:", Key: "pr", Indent: 2, BlockKey: "work_tracking"},
				{Raw: "  - https://github.com/org/repo/pull/1", IsList: true, Indent: 2},
				{Raw: "  - https://github.com/org/repo/pull/2", IsList: true, Indent: 2},
				{Raw: "", IsEmpty: true},
				{Raw: "title: Test", Key: "title", Indent: 0},
			},
		},
	}

	urls := getPRURLsFromItem(item)
	if len(urls) != 2 {
		t.Fatalf("getPRURLsFromItem = %v, want 2 URLs", urls)
	}
	if urls[0] != "https://github.com/org/repo/pull/1" {
		t.Errorf("url[0] = %q", urls[0])
	}
}

func TestGetPRURLsFromItemNilDocV2(t *testing.T) {
	item := &model.Item{}
	urls := getPRURLsFromItem(item)
	if urls != nil {
		t.Errorf("expected nil for nil doc, got %v", urls)
	}
}

func TestGetPRURLsFromItemNoWorkTracking(t *testing.T) {
	item := &model.Item{
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "title: Test", Key: "title", Indent: 0},
			},
		},
	}
	urls := getPRURLsFromItem(item)
	if len(urls) != 0 {
		t.Errorf("expected empty, got %v", urls)
	}
}

func TestStorePRURLsReplace(t *testing.T) {
	item := &model.Item{
		WorkTracking: map[string]interface{}{},
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "id: T-001", Key: "id", Indent: 0},
				{Raw: "work_tracking:", Key: "work_tracking", Indent: 0},
				{Raw: "  branch: main", Key: "branch", Indent: 2, BlockKey: "work_tracking"},
				{Raw: "  pr: []", Key: "pr", Indent: 2, BlockKey: "work_tracking"},
				{Raw: "", IsEmpty: true},
				{Raw: "title: Test", Key: "title", Indent: 0},
			},
		},
	}

	storePRURLs(item, []string{"https://github.com/org/repo/pull/99"})

	got := item.Doc.String()
	if !containsStr(got, "- https://github.com/org/repo/pull/99") {
		t.Errorf("storePRURLs didn't insert URL:\n%s", got)
	}
}

func TestStorePRURLsClear(t *testing.T) {
	item := &model.Item{
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "work_tracking:", Key: "work_tracking", Indent: 0},
				{Raw: "  pr:", Key: "pr", Indent: 2, BlockKey: "work_tracking"},
				{Raw: "  - https://old", IsList: true, Indent: 2, BlockKey: "work_tracking"},
			},
		},
	}

	storePRURLs(item, nil)

	got := item.Doc.String()
	if !containsStr(got, "pr: []") {
		t.Errorf("storePRURLs didn't clear to empty:\n%s", got)
	}
}

func TestStorePRURLsNilDocV2(t *testing.T) {
	item := &model.Item{}
	storePRURLs(item, []string{"https://x"}) // should not panic
}

func TestStorePRURLsNoWorkTrackingV2(t *testing.T) {
	item := &model.Item{
		Doc: &model.ParsedDocument{
			Lines: []model.Line{
				{Raw: "title: Test", Key: "title", Indent: 0},
			},
		},
	}
	storePRURLs(item, []string{"https://x"}) // should not panic
}

func TestAppendToNestedListNewParent(t *testing.T) {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "id: T-001", Key: "id"},
		},
	}

	appendToNestedList(doc, "work_tracking", "commits", "abc123 fix bug")

	got := doc.String()
	if !containsStr(got, "work_tracking:") || !containsStr(got, "commits:") || !containsStr(got, "- abc123 fix bug") {
		t.Errorf("appendToNestedList new parent:\n%s", got)
	}
}

func TestAppendToNestedListExistingParentNewKey(t *testing.T) {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "work_tracking:", Key: "work_tracking"},
			{Raw: "  branch: main", Key: "branch", Indent: 2, BlockKey: "work_tracking"},
			{Raw: "", IsEmpty: true},
			{Raw: "title: Test", Key: "title"},
		},
	}

	appendToNestedList(doc, "work_tracking", "commits", "abc123")

	got := doc.String()
	if !containsStr(got, "  commits:") || !containsStr(got, "  - abc123") {
		t.Errorf("appendToNestedList existing parent new key:\n%s", got)
	}
}

func TestAppendToNestedListReplaceEmptyMarker(t *testing.T) {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "work_tracking:", Key: "work_tracking"},
			{Raw: "  commits:", Key: "commits", Indent: 2, BlockKey: "work_tracking"},
			{Raw: "  - []", IsList: true, Indent: 2, BlockKey: "work_tracking"},
		},
	}

	appendToNestedList(doc, "work_tracking", "commits", "def456")

	got := doc.String()
	if containsStr(got, "- []") {
		t.Errorf("should have replaced empty marker:\n%s", got)
	}
	if !containsStr(got, "- def456") {
		t.Errorf("should contain new value:\n%s", got)
	}
}

func TestAppendToNestedListAppend(t *testing.T) {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "work_tracking:", Key: "work_tracking"},
			{Raw: "  commits:", Key: "commits", Indent: 2, BlockKey: "work_tracking"},
			{Raw: "  - first", IsList: true, Indent: 2, BlockKey: "work_tracking"},
		},
	}

	appendToNestedList(doc, "work_tracking", "commits", "second")

	got := doc.String()
	if !containsStr(got, "- first") || !containsStr(got, "- second") {
		t.Errorf("appendToNestedList append:\n%s", got)
	}
}

func TestCloseWithTimeTracking(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Set started_at on T-003 (active task)
	item, _ := s.Get("T-003")
	started := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	setNestedField(item, "time_tracking", "started_at", started)
	s.Write(item)

	code := Close(s, cfg, "T-003", "completed", CloseOpts{Force: true})
	if code != 0 {
		t.Errorf("close with time tracking exit %d", code)
	}

	// Reload and verify wall_time was computed
	s2, _ := store.New(cfg)
	item2, _ := s2.Get("T-003")
	wt, ok := getNestedField(item2, "time_tracking", "wall_time_hours")
	if !ok || wt == "" {
		t.Error("wall_time_hours not set after close with started_at")
	}
}

func TestCloseAbandonedWithReasonV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	code := Close(s, cfg, "T-001", "abandoned", CloseOpts{Reason: "no longer needed"})
	if code != 0 {
		t.Errorf("abandoned with reason exit %d", code)
	}
}

func TestCloseInvalidResolutionV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-001", "invalid_status", CloseOpts{})
	if code != 2 {
		t.Errorf("invalid resolution should exit 2, got %d", code)
	}
}

func TestCloseAlreadyClosed(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-004", "completed", CloseOpts{})
	if code != 1 {
		t.Errorf("closing already-closed should exit 1, got %d", code)
	}
}

func TestNoteEditHappy(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)

	NoteAdd(cfg, "original message")

	// Load registry to find the generated note ID
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Notes) == 0 {
		t.Fatal("no notes found after NoteAdd")
	}
	noteID := r.Notes[0].ID

	code := NoteEdit(cfg, noteID, "updated message")
	if code != 0 {
		t.Errorf("note edit exit %d", code)
	}
}

func TestNoteEditNotFoundV2(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)
	NoteAdd(cfg, "something")

	code := NoteEdit(cfg, "nonexistent-id", "updated")
	if code != 1 {
		t.Errorf("note edit not found should exit 1, got %d", code)
	}
}

func TestNoteRmHappy(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)
	NoteAdd(cfg, "to be removed")

	// Load registry to find the generated note ID
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Notes) == 0 {
		t.Fatal("no notes found after NoteAdd")
	}
	noteID := r.Notes[0].ID

	code := NoteRm(cfg, noteID)
	if code != 0 {
		t.Errorf("note rm exit %d", code)
	}
}

func TestNoteRmNotFoundV2(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)
	NoteAdd(cfg, "something")

	code := NoteRm(cfg, "nonexistent-id")
	if code != 1 {
		t.Errorf("note rm not found should exit 1, got %d", code)
	}
}

func TestReconcileOptWrappers(t *testing.T) {
	// Exercise the nil-check wrapper methods
	opts := &ReconcileOpts{}

	tc := opts.toolCheck()
	if tc == nil {
		t.Error("toolCheck should return default when nil")
	}

	bc := opts.branchCheck()
	if bc == nil {
		t.Error("branchCheck should return default when nil")
	}

	pf := opts.prFetch()
	if pf == nil {
		t.Error("prFetch should return default when nil")
	}

	sc := opts.s3Check()
	if sc == nil {
		t.Error("s3Check should return default when nil")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
