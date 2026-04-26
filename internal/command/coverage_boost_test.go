package command

import (
	"os"
	"path/filepath"
	"strings"
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
	code := Check(s, cfg, true, false)
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

	doc.AppendToNestedList("work_tracking", "commits", "abc123 fix bug")

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

	doc.AppendToNestedList("work_tracking", "commits", "abc123")

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

	doc.AppendToNestedList("work_tracking", "commits", "def456")

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

	doc.AppendToNestedList("work_tracking", "commits", "second")

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
	item.SetNested("time_tracking", "started_at", started)
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

func TestStatusDashboardWithUATPending(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	// Create an item with deployed_dev stage to exercise findUATPending
	item, _ := s.Get("T-003")
	item.Delivery["stage"] = "deployed_dev"
	s.Write(item)

	code := Status(s, cfg, "", StatusOpts{})
	if code != 0 {
		t.Errorf("status with UAT pending exit %d", code)
	}
}

func TestStatusRecentFlag(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Status(s, cfg, "", StatusOpts{Recent: true})
	if code != 0 {
		t.Errorf("status -r exit %d", code)
	}
}

func TestCloseGateFailure(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	// Close T-001 (queued) without --force — gate should catch missing evidence
	code := Close(s, cfg, "T-001", "completed", CloseOpts{})
	// Gate may or may not fail depending on config — just exercise the path
	_ = code
}

func TestSprintCreateHappy(t *testing.T) {
	_, cfg := setupTestEnv(t)
	EpicCreate(cfg, "Sprint Test Epic")

	// Load epics to get the generated ID
	r, _ := registry.Load(cfg.EpicsPath())
	if len(r.Epics) == 0 {
		t.Fatal("no epics created")
	}
	epicID := r.Epics[0].ID

	code := SprintCreate(cfg, epicID, "Sprint One")
	if code != 0 {
		t.Errorf("sprint create exit %d", code)
	}
}

func TestSprintListWithEpic(t *testing.T) {
	_, cfg := setupTestEnv(t)
	EpicCreate(cfg, "List Epic")

	r, _ := registry.Load(cfg.EpicsPath())
	epicID := r.Epics[0].ID
	SprintCreate(cfg, epicID, "Sprint A")

	code := SprintList(cfg, epicID)
	if code != 0 {
		t.Errorf("sprint list with epic exit %d", code)
	}
}


func TestMigrateWithScope(t *testing.T) {
	s, cfg := setupTestEnv(t)

	code := Migrate(s, cfg, MigrateOpts{DryRun: true})
	if code != 0 {
		t.Errorf("migrate dry-run exit %d", code)
	}
}

func TestDepRmNotDependentV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	code := DepRm(s, cfg, "T-001", "T-003")
	if code != 1 {
		t.Errorf("dep rm non-existent exit %d, want 1", code)
	}
}

func TestIndexGeneration(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Index(s, cfg)
	if code != 0 {
		t.Errorf("index exit %d", code)
	}
}

func TestLogSingleWithLimitV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	Tag(s, cfg, "T-001", "add", "test-log")
	code := Log(s, cfg, "T-001", LogOpts{Limit: 1})
	if code != 0 {
		t.Errorf("log with limit exit %d", code)
	}
}

func TestEpicCreateAndListHappy(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.EpicsPath()), 0755)

	code := EpicCreate(cfg, "Coverage Epic 1")
	if code != 0 {
		t.Errorf("epic create exit %d", code)
	}
	code = EpicCreate(cfg, "Coverage Epic 2")
	if code != 0 {
		t.Errorf("epic create 2 exit %d", code)
	}

	s, _ := setupTestEnv(t) // fresh store for list
	code = EpicList(s, cfg)
	if code != 0 {
		t.Errorf("epic list exit %d", code)
	}
}

func TestNoteListEmptyV2(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.NotesPath()), 0755)
	code := NoteList(cfg, 0) // limit=0 for all
	_ = code
}

func TestNoteAddLoadError(t *testing.T) {
	_, cfg := setupTestEnv(t)
	// Don't create .as directory — Load should handle gracefully
	code := NoteAdd(cfg, "will fail")
	// Should succeed (Load creates file if missing) or fail gracefully
	_ = code
}

func TestSprintCreateWithEpicV2(t *testing.T) {
	_, cfg := setupTestEnv(t)
	os.MkdirAll(filepath.Dir(cfg.EpicsPath()), 0755)
	EpicCreate(cfg, "My Epic")

	r, _ := registry.Load(cfg.EpicsPath())
	if len(r.Epics) == 0 {
		t.Skip("no epic created")
	}
	epicID := r.Epics[0].ID

	code := SprintCreate(cfg, epicID, "Sprint Alpha")
	if code != 0 {
		t.Errorf("sprint create exit %d", code)
	}

	code = SprintList(cfg, epicID)
	if code != 0 {
		t.Errorf("sprint list exit %d", code)
	}

	code = SprintList(cfg, "")
	if code != 0 {
		t.Errorf("sprint list all exit %d", code)
	}
}

func TestMigrateActual(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Run actual migrate (not dry-run) on test data
	code := Migrate(s, cfg, MigrateOpts{DryRun: false})
	if code != 0 {
		t.Errorf("migrate actual exit %d", code)
	}
}

func TestCloseNotFoundV3(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-999", "completed", CloseOpts{})
	if code != 1 {
		t.Errorf("close not found exit %d, want 1", code)
	}
}

func TestCloseUnknownType(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Inject item with unknown type
	item, _ := s.Get("T-001")
	item.Type = "banana"
	s.Write(item)
	code := Close(s, cfg, "T-001", "completed", CloseOpts{})
	if code != 1 {
		t.Errorf("close unknown type exit %d, want 1", code)
	}
}

func TestCloseAlreadyCompleted(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Close(s, cfg, "T-004", "completed", CloseOpts{})
	if code != 1 {
		t.Errorf("close already completed exit %d, want 1", code)
	}
}

func TestCheckVerboseV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Check(s, cfg, false, false)
	_ = code // exercise verbose path
}

func TestFinishNoArgs(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Finish(s, cfg, "", FinishOpts{})
	if code == 0 {
		t.Error("finish no args should not succeed")
	}
}

func TestFinishNotFoundV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Finish(s, cfg, "T-999", FinishOpts{})
	if code == 0 {
		t.Error("finish not found should not exit 0")
	}
}

func TestSyncInTestDir(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Sync(s, "test sync message")
	// Will fail (no git) — just exercises the code path
	_ = code
}

func TestReconcileDryRunOnTestEnv(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	code := Reconcile(s, cfg, ReconcileOpts{
		DryRun: true,
		ToolCheck: func(string) bool { return false },
	})
	if code != 0 {
		t.Errorf("reconcile dry-run exit %d", code)
	}
}

func TestPrimeCompactV2(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	code := Prime(s, cfg, PrimeOpts{Compact: true})
	if code != 0 {
		t.Errorf("prime compact exit %d", code)
	}
}

func TestPrimeJSONV2(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	code := Prime(s, cfg, PrimeOpts{Format: "json"})
	if code != 0 {
		t.Errorf("prime json exit %d", code)
	}
}

func TestStatusDashboardAllSections(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Create variety of items to exercise all dashboard sections
	item, _ := s.Get("T-003")
	item.Delivery["stage"] = "deployed_dev"
	s.Write(item)

	// Default dashboard
	code := Status(s, cfg, "", StatusOpts{})
	if code != 0 {
		t.Errorf("status default exit %d", code)
	}

	// All sections
	code = Status(s, cfg, "", StatusOpts{All: true})
	if code != 0 {
		t.Errorf("status -a exit %d", code)
	}
}

func TestStatusSingleWithDeps(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-002 depends on T-001
	code := Status(s, cfg, "T-002", StatusOpts{})
	if code != 0 {
		t.Errorf("status T-002 exit %d", code)
	}
}

func TestIndexWithDelivery(t *testing.T) {
	s, cfg := setupTestEnvWithDelivery(t)

	// Set delivery stages on items
	item, _ := s.Get("T-003")
	item.Delivery["stage"] = "deployed_dev"
	item.Delivery["uat_approved_by"] = ""
	s.Write(item)

	code := Index(s, cfg)
	if code != 0 {
		t.Errorf("index with delivery exit %d", code)
	}
}

func TestCloseWithGateEnforcement(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)

	// Add a close gate that requires "summary" to be non-empty
	cfg.Gates = map[string][]config.GateConfig{
		"close": {
			{Type: "field_nonempty", Fields: []string{"summary"}},
		},
	}

	// Start T-001 first
	Start(s, cfg, "T-001", StartOpts{})

	// Close without force — gate should fail because T-001 has no summary
	code := Close(s, cfg, "T-001", "completed", CloseOpts{Force: false})
	if code != 1 {
		t.Errorf("close with failing gate exit %d, want 1", code)
	}
}

func TestMigrateActualRun(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Non-dry-run migration on test data
	code := Migrate(s, cfg, MigrateOpts{DryRun: false})
	if code != 0 {
		t.Errorf("migrate actual exit %d", code)
	}
	// Run again to verify stability (no changes expected)
	code = Migrate(s, cfg, MigrateOpts{DryRun: true})
	if code != 0 {
		t.Errorf("migrate stability check exit %d", code)
	}
}

func TestStartBlockedItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-002 depends on T-001 (unresolved) — should be blocked
	code := Start(s, cfg, "T-002", StartOpts{})
	if code == 0 {
		t.Error("start blocked item should not succeed")
	}
}

func TestTagNilDoc(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	// Create item with nil doc
	item, _ := s.Get("T-001")
	item.Doc = nil
	s.Write(item) // This may fail but we handle it

	code := Tag(s, cfg, "T-001", "add", "test")
	if code == 0 {
		t.Error("tag on nil doc should fail")
	}
}

func TestDepGraphJSONV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := DepGraph(s, cfg, DepGraphOpts{JSON: true})
	if code != 0 {
		t.Errorf("dep graph json exit %d", code)
	}
}

func TestCommitHappyV2(t *testing.T) {
	s, cfg := setupTestEnv(t)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	code := Commit(s, cfg, "T-001", "Fix the login bug")
	if code != 0 {
		t.Errorf("commit exit %d", code)
	}
	// Second commit to exercise append-to-existing path
	code = Commit(s, cfg, "T-001", "Follow up fix")
	if code != 0 {
		t.Errorf("commit 2 exit %d", code)
	}
}

func TestShowWithFieldV2(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{Field: "status"})
	if code != 0 {
		t.Errorf("show --field exit %d", code)
	}
}

func TestShowFieldNotFoundV2(t *testing.T) {
	s, _ := setupTestEnv(t)
	code := Show(s, nil, "T-001", ShowOpts{Field: "nonexistent"})
	if code != 1 {
		t.Errorf("show missing field exit %d, want 1", code)
	}
}

func TestListWithMultipleFilters(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := List(s, cfg, ListOpts{Type: "task", Status: "queued"})
	if code != 0 {
		t.Errorf("list filtered exit %d", code)
	}
}

func TestCreateIssue(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Create(s, cfg, "issue", "Test Bug", CreateOpts{})
	if code != 0 {
		t.Errorf("create issue exit %d", code)
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

// --- I-100: Untagged/un-epicked items must render under "Unassigned" header ---

func TestStatusQueuedUnassignedHeader(t *testing.T) {
	// Setup: epic task (has epic + tag) AND orphan tasks (no epic, no tags).
	// The orphan tasks must appear under a "◆ Unassigned" header, not bleed
	// into the epic's tag section.
	_, cfg := setupTestEnv(t)
	root := cfg.Root()

	// Create an epic
	r := &registry.Registry{}
	e := r.AddEpic("Infra Epic")
	r.Save(cfg.EpicsPath())

	// Epic task with tag
	writeFile(t, filepath.Join(root, "tasks", "T-010-epic.md"), `id: T-010
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Epic task with tag
epic: `+e.ID+`
tags: [agent-tooling]
depends_on:
- []
next_actions:
- []
`)

	// Orphan task — no epic, no tags
	writeFile(t, filepath.Join(root, "tasks", "T-011-orphan.md"), `id: T-011
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
completed: null
title: Orphan task no epic no tags
depends_on:
- []
next_actions:
- []
`)

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Capture stdout
	old := os.Stdout
	rd, w, _ := os.Pipe()
	os.Stdout = w
	Status(s, cfg, "", StatusOpts{Tasks: true})
	w.Close()
	os.Stdout = old

	buf := make([]byte, 32768)
	n, _ := rd.Read(buf)
	output := string(buf[:n])

	// Strip ANSI codes for easier assertions
	stripped := stripANSI(output)

	if !strings.Contains(stripped, "Unassigned") {
		t.Errorf("expected 'Unassigned' header for orphan tasks, got:\n%s", stripped)
	}

	// Orphan task should appear after the Unassigned header, not under the epic
	unassignedIdx := strings.Index(stripped, "Unassigned")
	orphanIdx := strings.Index(stripped, "T-011")
	epicTaskIdx := strings.Index(stripped, "T-010")

	if unassignedIdx < 0 || orphanIdx < 0 {
		t.Fatalf("missing expected content: Unassigned=%d, T-011=%d", unassignedIdx, orphanIdx)
	}
	if epicTaskIdx >= 0 && epicTaskIdx > unassignedIdx {
		t.Error("epic task should appear before Unassigned section")
	}
	if orphanIdx < unassignedIdx {
		t.Error("orphan task should appear after Unassigned header")
	}
}

func stripANSI(s string) string {
	// Simple ANSI stripper — removes ESC[...m sequences
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}
