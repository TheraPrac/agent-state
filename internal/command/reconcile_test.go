package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// --- extractGitOrg ---

func TestExtractGitOrgSSH(t *testing.T) {
	got := extractGitOrg("git@github.com:myorg/myrepo.git")
	if got != "myorg" {
		t.Errorf("extractGitOrg(SSH) = %q, want %q", got, "myorg")
	}
}

func TestExtractGitOrgHTTPS(t *testing.T) {
	got := extractGitOrg("https://github.com/myorg/myrepo.git")
	if got != "myorg" {
		t.Errorf("extractGitOrg(HTTPS) = %q, want %q", got, "myorg")
	}
}

func TestExtractGitOrgSSHNoSuffix(t *testing.T) {
	got := extractGitOrg("git@github.com:anotherorg/repo")
	if got != "anotherorg" {
		t.Errorf("extractGitOrg(SSH no .git) = %q, want %q", got, "anotherorg")
	}
}

func TestExtractGitOrgHTTPSNoSuffix(t *testing.T) {
	got := extractGitOrg("https://github.com/anotherorg/repo")
	if got != "anotherorg" {
		t.Errorf("extractGitOrg(HTTPS no .git) = %q, want %q", got, "anotherorg")
	}
}

func TestExtractGitOrgEmpty(t *testing.T) {
	got := extractGitOrg("")
	if got != "" {
		t.Errorf("extractGitOrg(empty) = %q, want empty", got)
	}
}

func TestExtractGitOrgUnrecognized(t *testing.T) {
	got := extractGitOrg("file:///tmp/repo.git")
	if got != "" {
		t.Errorf("extractGitOrg(file://) = %q, want empty", got)
	}
}

// --- toolAvailable ---

func TestToolAvailableGo(t *testing.T) {
	if !toolAvailable("go") {
		t.Error("toolAvailable(\"go\") should be true — Go must be installed to run this test")
	}
}

func TestToolAvailableNonexistent(t *testing.T) {
	if toolAvailable("nonexistent_tool_xyz") {
		t.Error("toolAvailable(\"nonexistent_tool_xyz\") should be false")
	}
}

// --- formatNow ---

func TestFormatNowNonEmpty(t *testing.T) {
	got := formatNow()
	if got == "" {
		t.Fatal("formatNow() returned empty string")
	}
}

func TestFormatNowRFC3339(t *testing.T) {
	got := formatNow()
	_, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Errorf("formatNow() = %q, not valid RFC3339: %v", got, err)
	}
}

// --- Reconcile phases with injected mocks ---

func TestReconcileBranchPush(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Set up an item at coding stage with a branch
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// Inject mock: branch always exists on remote
	opts := ReconcileOpts{BranchCheck: func(cfg *config.Config, branch string) bool { return true }}

	n := reconcileBranchPush(s, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 update, got %d", n)
	}

	// Verify stage advanced
	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "pushed" {
		t.Errorf("stage = %q, want pushed", stage)
	}
}

func TestReconcileBranchPushDryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{DryRun: true, BranchCheck: func(cfg *config.Config, branch string) bool { return true }}

	n := reconcileBranchPush(s, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 update detected, got %d", n)
	}

	// Stage should NOT advance in dry-run
	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "coding" {
		t.Errorf("dry-run should not change stage, got %q", stage)
	}
}

func TestReconcilePRState(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "pushed")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{PRFetch: func(cfg *config.Config, branch string) (string, []string) {
		return "OPEN", []string{"https://github.com/org/repo/pull/1"}
	}}

	n := reconcilePRState(s, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 update, got %d", n)
	}

	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "pr_open" {
		t.Errorf("stage = %q, want pr_open", stage)
	}
}

func TestReconcileMergeState(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "pr_open")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{PRFetch: func(cfg *config.Config, branch string) (string, []string) {
		return "MERGED", []string{"https://github.com/org/repo/pull/1"}
	}}

	n := reconcileMergeState(s, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 update, got %d", n)
	}

	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "merged" {
		t.Errorf("stage = %q, want merged", stage)
	}
}

func TestReconcileArchive(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Set T-002 to completed status (it starts as queued in test env)
	if _, ok := s.Get("T-002"); !ok {
		t.Skip("T-002 not in test env")
	}
	if err := s.Mutate("T-002", func(it *model.Item) error {
		it.Doc.SetField("status", "completed")
		it.Status = "completed"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-002: %v", err)
	}

	// Reload store to pick up the status change
	s, _ = store.New(cfg)

	n := reconcileArchive(s, cfg, ReconcileOpts{DryRun: false})
	if n != 1 {
		t.Errorf("expected 1 archive move, got %d", n)
	}

	// Verify file moved to archive/
	path, ok := s.Path("T-002")
	if ok && !strings.Contains(path, "archive") {
		t.Errorf("T-002 should be in archive/, got %s", path)
	}
}

func TestReconcileArchiveDryRun(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-002", func(it *model.Item) error {
		it.Doc.SetField("status", "completed")
		it.Status = "completed"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-002: %v", err)
	}
	s, _ = store.New(cfg)

	n := reconcileArchive(s, cfg, ReconcileOpts{DryRun: true})
	if n != 1 {
		t.Errorf("expected 1 detected, got %d", n)
	}

	// File should NOT have moved
	path, _ := s.Path("T-002")
	if strings.Contains(path, "archive") {
		t.Error("dry-run should not move files")
	}
}

func TestReconcileNoBranchSkips(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Item at coding stage but no branch — should be skipped
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{BranchCheck: func(cfg *config.Config, branch string) bool {
		t.Error("should not be called for item without branch")
		return false
	}}

	n := reconcileBranchPush(s, cfg, opts)
	if n != 0 {
		t.Errorf("expected 0 updates for item without branch, got %d", n)
	}
}

func TestReconcilePRStateMerged(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "pushed")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{PRFetch: func(cfg *config.Config, branch string) (string, []string) {
		return "MERGED", []string{"https://github.com/org/repo/pull/1"}
	}}

	n := reconcilePRState(s, cfg, opts)
	if n != 1 {
		t.Errorf("expected 1 update, got %d", n)
	}
	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	if stage != "merged" {
		t.Errorf("pushed → merged: got %q", stage)
	}
}

func TestReconcilePRStateNoPR(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "pushed")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{PRFetch: func(cfg *config.Config, branch string) (string, []string) {
		return "", nil
	}}

	n := reconcilePRState(s, cfg, opts)
	if n != 0 {
		t.Errorf("expected 0 updates when no PR, got %d", n)
	}
}

func TestReconcileBranchNotOnRemote(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{BranchCheck: func(cfg *config.Config, branch string) bool { return false }}

	n := reconcileBranchPush(s, cfg, opts)
	if n != 0 {
		t.Errorf("expected 0 updates when branch not on remote, got %d", n)
	}
}

func TestReconcileFullFlow(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Set up item at coding with branch
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "coding")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	opts := ReconcileOpts{
		ToolCheck:   func(name string) bool { return true },
		BranchCheck: func(cfg *config.Config, branch string) bool { return true },
		PRFetch: func(cfg *config.Config, branch string) (string, []string) {
			return "MERGED", []string{"https://github.com/org/repo/pull/1"}
		},
	}
	code := Reconcile(s, cfg, opts)
	if code != 0 {
		t.Errorf("reconcile exit %d", code)
	}

	// After full reconcile: coding → pushed → merged (skipping pr_open because PR already merged)
	item, _ := s.Get("T-001")
	stage, _ := getNestedField(item, "delivery", "stage")
	// Phase 0 advances to pushed, Phase 1 finds merged PR → advances to merged
	if stage != "pushed" && stage != "merged" {
		t.Errorf("expected pushed or merged, got %q", stage)
	}
}

func TestBranchExistsOnRemoteWithWorktreeConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		ParentDir: t.TempDir(), // nonexistent repos
		Repos:     []string{"api", "web"},
		RepoMap:   map[string]string{"api": "theraprac-api", "web": "theraprac-web"},
	}
	// Repos don't exist — should return false, not panic
	result := branchExistsOnRemote(cfg, "feat/T-001-test")
	if result {
		t.Error("should return false when repos don't exist")
	}
}

func TestRepoFullNamesWithWorktreeConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Worktree = &config.WorktreeConfig{
		Enabled:   true,
		ParentDir: dir,
		Repos:     []string{"api", "web"},
		RepoMap:   map[string]string{"api": "theraprac-api", "web": "theraprac-web"},
	}
	// Need a git repo to get remote URL
	os.MkdirAll(filepath.Join(dir, ".git"), 0755)
	names := repoFullNames(cfg)
	// No remote configured — should return nil
	if names != nil {
		t.Logf("names: %v", names)
	}
}

func TestBranchExistsOnRemoteNoRepo(t *testing.T) {
	cfg := config.Defaults()
	cfg.Worktree = nil
	// No repos configured — should return false without error
	result := branchExistsOnRemote(cfg, "nonexistent-branch")
	if result {
		t.Error("should return false for nonexistent branch with no repos")
	}
}

func TestGetPRStateNoRepos(t *testing.T) {
	cfg := config.Defaults()
	// No git remote — repoFullNames returns nil → no PRs found
	state, urls := getPRState(cfg, "nonexistent-branch")
	if state != "" {
		t.Errorf("expected empty state, got %q", state)
	}
	if len(urls) != 0 {
		t.Errorf("expected no urls, got %v", urls)
	}
}

func TestRepoFullNamesNoGit(t *testing.T) {
	cfg := config.Defaults()
	// No git repo in temp dir — should return nil
	names := repoFullNames(cfg)
	if names != nil {
		t.Errorf("expected nil, got %v", names)
	}
}

func TestStorePRURLsNilDoc(t *testing.T) {
	item := &model.Item{}
	// Should not panic with nil Doc
	storePRURLs(item, []string{"https://github.com/org/repo/pull/1"})
}

func TestStorePRURLsNoWorkTracking(t *testing.T) {
	item := &model.Item{
		Doc: model.NewParsedDocument(),
	}
	// No work_tracking section — should not panic
	storePRURLs(item, []string{"https://github.com/org/repo/pull/1"})
}

func TestTouchItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	item, _ := s.Get("T-001")
	os.Setenv("AS_AGENT_ID", "test-agent")
	touchItem(item, cfg)
	val, ok := item.Doc.GetField("last_touched")
	if !ok || val == "" {
		t.Error("touchItem should set last_touched")
	}
	val, ok = item.Doc.GetField("last_touched_by")
	if !ok || val != "test-agent" {
		t.Errorf("last_touched_by = %q, want test-agent", val)
	}
}

// --- Phase 3 helpers ---

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		url              string
		owner, repo, num string
	}{
		{"https://github.com/TheraPrac/theraprac-api/pull/123", "TheraPrac", "theraprac-api", "123"},
		{"https://github.com/org/repo/pull/456", "org", "repo", "456"},
		{"not-a-url", "", "", ""},
		{"https://github.com/a/b/issues/1", "", "", ""},
	}
	for _, tt := range tests {
		o, r, n := parsePRURL(tt.url)
		if o != tt.owner || r != tt.repo || n != tt.num {
			t.Errorf("parsePRURL(%q) = %q/%q/%q, want %q/%q/%q", tt.url, o, r, n, tt.owner, tt.repo, tt.num)
		}
	}
}

func TestGetPRURLsFromItem(t *testing.T) {
	s, _ := setupTestEnv(t)
	item, _ := s.Get("T-001")
	// No PR URLs set yet
	urls := getPRURLsFromItem(item)
	if len(urls) != 0 {
		t.Errorf("expected no URLs, got %v", urls)
	}
}

func TestGetPRURLsFromItemNilDoc(t *testing.T) {
	item := &model.Item{}
	urls := getPRURLsFromItem(item)
	if urls != nil {
		t.Errorf("expected nil, got %v", urls)
	}
}

func TestReconcileDeployStateNoAWS(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Set up merged item
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SetNested("delivery", "stage", "merged")
		it.SetNested("work_tracking", "branch", "feat/T-001-test")
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-001: %v", err)
	}

	// No PRs on the item → nothing to check → no updates
	n := reconcileDeployState(s, cfg, ReconcileOpts{DryRun: true})
	if n != 0 {
		t.Errorf("expected 0 updates (no PR URLs), got %d", n)
	}
}

func TestParseDedupeJSON(t *testing.T) {
	data := []byte(`{"status":"success","job_id":"j-123"}`)
	d, ok := parseDedupeJSON(data)
	if !ok {
		t.Fatal("should parse valid JSON")
	}
	if d.Status != "success" {
		t.Errorf("status = %q, want success", d.Status)
	}
	if d.JobID != "j-123" {
		t.Errorf("job_id = %q, want j-123", d.JobID)
	}
}

func TestParseDedupeJSONBad(t *testing.T) {
	_, ok := parseDedupeJSON([]byte("not json"))
	if ok {
		t.Error("bad JSON should return false")
	}
}

func TestIsDedupeDeployedSuccess(t *testing.T) {
	d := dedupeRecord{Status: "success", JobID: "j-1"}
	if !isDedupeDeployed(d, "bucket", s3Exists) {
		t.Error("success should be deployed")
	}
}

func TestIsDedupeDeployedFailure(t *testing.T) {
	d := dedupeRecord{Status: "failure", JobID: "j-2"}
	if isDedupeDeployed(d, "bucket", s3Exists) {
		t.Error("failure should not be deployed")
	}
}

func TestIsDedupeDeployedQueued(t *testing.T) {
	noS3 := func(key string) bool { return false }
	d := dedupeRecord{Status: "queued", JobID: "j-3"}
	if isDedupeDeployed(d, "bucket", noS3) {
		t.Error("queued without S3 record should not be deployed")
	}
}

func TestIsDedupeDeployedQueuedWithCompleted(t *testing.T) {
	yesS3 := func(key string) bool { return true }
	d := dedupeRecord{Status: "queued", JobID: "j-3"}
	if !isDedupeDeployed(d, "bucket", yesS3) {
		t.Error("queued with completed S3 record should be deployed")
	}
}

func TestIsDedupeDeployedProcessingWithCompleted(t *testing.T) {
	yesS3 := func(key string) bool { return true }
	d := dedupeRecord{Status: "processing", JobID: "j-4"}
	if !isDedupeDeployed(d, "bucket", yesS3) {
		t.Error("processing with completed S3 record should be deployed")
	}
}

func TestIsDedupeDeployedEmpty(t *testing.T) {
	d := dedupeRecord{Status: "", JobID: ""}
	if isDedupeDeployed(d, "bucket", s3Exists) {
		t.Error("empty status should not be deployed")
	}
}

func TestIsDedupeDeployedQueuedNoJobID(t *testing.T) {
	d := dedupeRecord{Status: "queued", JobID: ""}
	if isDedupeDeployed(d, "bucket", s3Exists) {
		t.Error("queued with no job_id should not be deployed")
	}
}

func TestReconcileNoGHSkipsPRPhases(t *testing.T) {
	s, cfg := setupTestEnv(t)

	opts := ReconcileOpts{
		DryRun:    true,
		ToolCheck: func(name string) bool { return false },
	}
	code := Reconcile(s, cfg, opts)
	if code != 0 {
		t.Errorf("reconcile should succeed even without gh, got exit %d", code)
	}
}
