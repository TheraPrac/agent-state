package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

func setupPRTestEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupTestEnv(t)

	// Make T-003 active with testing_evidence structure
	if err := s.Mutate("T-003", func(it *model.Item) error {
		it.Doc.SetField("status", "active")
		it.Status = "active"
		return nil
	}); err != nil {
		t.Fatalf("mutate T-003: %v", err)
	}

	// Add testing config
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
			"api_lint": {Command: "make lint"},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {Command: "make integration-local"},
			"web_e2e":         {Command: "make e2e"},
		},
	}

	return s, cfg
}

func mockGitOpts(nameStatus, numstat, headSHA string, blobHashes map[string]string, existingFiles map[string]bool) PROpts {
	return PROpts{
		Repo:     "api",
		PRNumber: 42,
		GitNameStatus: func(dir string) (string, error) {
			return nameStatus, nil
		},
		GitNumstat: func(dir string) (string, error) {
			return numstat, nil
		},
		GitHeadSHA: func(dir string) (string, error) {
			return headSHA, nil
		},
		GitBlobHash: func(dir, path string) (string, error) {
			return blobHashes[path], nil
		},
		FileExists: func(path string) bool {
			// Strip the repo dir prefix for lookup
			base := filepath.Base(path)
			dir := filepath.Dir(path)
			key := filepath.Join(filepath.Base(dir), base)
			if existingFiles[path] {
				return true
			}
			if existingFiles[key] {
				return true
			}
			// Check just the relative path
			for k := range existingFiles {
				if strings.HasSuffix(path, k) {
					return true
				}
			}
			return false
		},
	}
}

func TestPRBasicFlow(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	opts := mockGitOpts(
		"M\tinternal/db/billing.go\nA\tinternal/db/billing_test.go\nM\tMakefile",
		"45\t12\tinternal/db/billing.go\n30\t0\tinternal/db/billing_test.go\n2\t1\tMakefile",
		"abc1234567890",
		map[string]string{
			"internal/db/billing.go":      "blobhash1",
			"internal/db/billing_test.go": "blobhash2",
			"Makefile":                    "blobhash3",
		},
		map[string]bool{
			"internal/db/billing_test.go": true,
		},
	)

	code := PR(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("PR returned %d, want 0", code)
	}

	// Verify manifest sidecar
	m, err := manifest.Load(cfg.ManifestDir(), "T-003")
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}
	if len(m.PRs) != 1 {
		t.Fatalf("PRs = %d, want 1", len(m.PRs))
	}
	pr := m.PRs[0]
	if pr.Repo != "api" || pr.PRNumber != 42 {
		t.Errorf("PR = %s#%d", pr.Repo, pr.PRNumber)
	}
	if len(pr.Files) != 3 {
		t.Errorf("files = %d, want 3", len(pr.Files))
	}

	// Verify item updated
	item, _ := s.Get("T-003")
	prs, _ := getNestedField(item, "manifest", "prs")
	if prs != "api#42" {
		t.Errorf("manifest.prs = %q, want api#42", prs)
	}

	// Verify code stats surfaced on item
	filesChanged, _ := getNestedField(item, "manifest", "files_changed")
	if filesChanged != "3" {
		t.Errorf("manifest.files_changed = %q, want 3", filesChanged)
	}
	insertions, _ := getNestedField(item, "manifest", "insertions")
	if insertions != "77" {
		t.Errorf("manifest.insertions = %q, want 77", insertions)
	}
	deletions, _ := getNestedField(item, "manifest", "deletions")
	if deletions != "13" {
		t.Errorf("manifest.deletions = %q, want 13", deletions)
	}
	netLines, _ := getNestedField(item, "manifest", "net_lines")
	if netLines != "+64" {
		t.Errorf("manifest.net_lines = %q, want +64", netLines)
	}

	// Verify head SHA
	headSHAVal, _ := getNestedField(item, "manifest", "head_sha")
	if headSHAVal != "abc1234567890" {
		t.Errorf("manifest.head_sha = %q, want abc1234567890", headSHAVal)
	}

	// Verify work_tracking.pr
	raw := item.Doc.String()
	if !strings.Contains(raw, "api#42") {
		t.Error("work_tracking.pr should contain api#42")
	}

	// Verify tests_written — billing_test.go is both a test file in the PR
	// and the 1:1 mapped test for billing.go
	if !strings.Contains(raw, "billing_test.go") {
		t.Error("testing_evidence.tests_written should contain billing_test.go")
	}
}

func TestPRTestFileMissingWarns(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// App file with NO corresponding test file — should warn but not block
	opts := mockGitOpts(
		"M\tinternal/db/billing.go",
		"10\t5\tinternal/db/billing.go",
		"abc1234567890",
		map[string]string{"internal/db/billing.go": "hash"},
		map[string]bool{}, // no test files exist
	)

	code := PR(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("PR returned %d, want 0 (warning not block)", code)
	}
}

func TestPRDeletedFileSkipsTestGate(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// Deleted app file should not require test file
	opts := mockGitOpts(
		"D\tinternal/db/old.go",
		"0\t50\tinternal/db/old.go",
		"abc1234567890",
		map[string]string{},
		map[string]bool{},
	)

	code := PR(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("PR returned %d, want 0 (deleted files skip gate)", code)
	}
}

func TestPRMultiPR(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	// First PR: api repo
	opts1 := mockGitOpts(
		"M\tinternal/db/billing.go\nM\tinternal/db/billing_test.go",
		"10\t5\tinternal/db/billing.go\n5\t0\tinternal/db/billing_test.go",
		"sha1111",
		map[string]string{"internal/db/billing.go": "h1", "internal/db/billing_test.go": "h2"},
		map[string]bool{"internal/db/billing_test.go": true},
	)

	code := PR(s, cfg, "T-003", opts1)
	if code != 0 {
		t.Fatalf("PR 1 returned %d", code)
	}

	// Second PR: web repo
	opts2 := mockGitOpts(
		"M\tsrc/components/Button.tsx\nM\tsrc/components/Button.test.tsx",
		"20\t3\tsrc/components/Button.tsx\n15\t0\tsrc/components/Button.test.tsx",
		"sha2222",
		map[string]string{"src/components/Button.tsx": "h3", "src/components/Button.test.tsx": "h4"},
		map[string]bool{"src/components/Button.test.tsx": true},
	)
	opts2.Repo = "web"
	opts2.PRNumber = 15

	code = PR(s, cfg, "T-003", opts2)
	if code != 0 {
		t.Fatalf("PR 2 returned %d", code)
	}

	// Verify both PRs in manifest
	m, _ := manifest.Load(cfg.ManifestDir(), "T-003")
	if len(m.PRs) != 2 {
		t.Fatalf("PRs = %d, want 2", len(m.PRs))
	}

	// Verify summary
	item, _ := s.Get("T-003")
	prs, _ := getNestedField(item, "manifest", "prs")
	if !strings.Contains(prs, "api#42") || !strings.Contains(prs, "web#15") {
		t.Errorf("manifest.prs = %q", prs)
	}
}

func TestPRScopeSuiteComputation(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	opts := mockGitOpts(
		"M\tinternal/db/billing.go\nM\tinternal/db/billing_test.go",
		"10\t5\tinternal/db/billing.go\n5\t0\tinternal/db/billing_test.go",
		"abc1234",
		map[string]string{"internal/db/billing.go": "h1", "internal/db/billing_test.go": "h2"},
		map[string]bool{"internal/db/billing_test.go": true},
	)

	code := PR(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("PR returned %d", code)
	}

	// api repo should trigger api_integration
	m, _ := manifest.Load(cfg.ManifestDir(), "T-003")
	found := false
	for _, s := range m.PRs[0].ScopeSuites {
		if s == "api_integration" {
			found = true
		}
	}
	if !found {
		t.Errorf("scope suites = %v, want api_integration", m.PRs[0].ScopeSuites)
	}

	// Check testing_evidence updated
	item, _ := s.Get("T-003")
	ev, ok := getNestedField(item, "testing_evidence", "api_integration")
	if !ok || ev != "required" {
		t.Errorf("testing_evidence.api_integration = %q, want 'required'", ev)
	}
}

func TestPRItemNotFound(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := mockGitOpts("", "", "", nil, nil)
	code := PR(s, cfg, "T-999", opts)
	if code != 1 {
		t.Errorf("PR returned %d, want 1", code)
	}
}

func TestPRItemNotActive(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := mockGitOpts("", "", "", nil, nil)
	// T-001 is queued, not active
	code := PR(s, cfg, "T-001", opts)
	if code != 1 {
		t.Errorf("PR returned %d, want 1", code)
	}
}

func TestPRNoChangedFiles(t *testing.T) {
	s, cfg := setupPRTestEnv(t)
	opts := mockGitOpts("", "", "abc123", nil, nil)
	code := PR(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("PR returned %d, want 1 (no files)", code)
	}
}

// --- Unit tests for helpers ---

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/db/billing.go", "app"},
		{"internal/db/billing_test.go", "test"},
		{"src/components/Button.tsx", "app"},
		{"src/components/Button.test.tsx", "test"},
		{"src/components/Button.spec.ts", "test"},
		{"src/__tests__/util.ts", "test"},
		{"db/changelog/001-init.xml", "migration"},
		{"api/openapi/api.yaml", "spec"},
		{"README.md", "doc"},
		{"docs/ARCHITECTURE.md", "doc"},
		{"Makefile", "config"},
		{"Dockerfile", "config"},
		{"docker-compose.yml", "config"},
		{"go.mod", "config"},
		{"package.json", "config"},
		{"internal/handler/routes.go", "app"},
	}
	for _, tt := range tests {
		got := classifyFile(tt.path)
		if got != tt.want {
			t.Errorf("classifyFile(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestTestFileFor(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/db/billing.go", "internal/db/billing_test.go"},
		{"src/components/Button.tsx", "src/components/Button.test.tsx"},
		{"src/hooks/useFoo.ts", "src/hooks/useFoo.test.ts"},
		// Skip cases
		{"internal/db/billing_test.go", ""},
		{"src/Button.test.tsx", ""},
		{"types.ts", ""},
		{"index.ts", ""},
		{"index.tsx", ""},
		{"internal/gen/models_gen.go", ""},
		{"src/api/types.d.ts", ""},
		// No convention
		{"config.yaml", ""},
		{"README.md", ""},
	}
	for _, tt := range tests {
		got := testFileFor(tt.path)
		if got != tt.want {
			t.Errorf("testFileFor(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestParseNameStatus(t *testing.T) {
	input := "M\tinternal/db/billing.go\nA\tnew_file.go\nD\told_file.go\nR100\told.go\tnew.go"
	files := parseNameStatus(input)
	if len(files) != 4 {
		t.Fatalf("files = %d, want 4", len(files))
	}
	if files[0].Action != "M" || files[0].Path != "internal/db/billing.go" {
		t.Errorf("file[0] = %+v", files[0])
	}
	if files[3].Action != "R" || files[3].Path != "new.go" {
		t.Errorf("file[3] = %+v", files[3])
	}
}

func TestMergeNumstat(t *testing.T) {
	files := []fileEntry{
		{Path: "a.go"},
		{Path: "b.go"},
	}
	mergeNumstat(files, "10\t5\ta.go\n20\t3\tb.go")
	if files[0].LinesAdded != 10 || files[0].LinesDeleted != 5 {
		t.Errorf("a.go = +%d/-%d", files[0].LinesAdded, files[0].LinesDeleted)
	}
	if files[1].LinesAdded != 20 || files[1].LinesDeleted != 3 {
		t.Errorf("b.go = +%d/-%d", files[1].LinesAdded, files[1].LinesDeleted)
	}
}

func TestComputeScopeSuitesTriggers(t *testing.T) {
	cfg := &config.Config{
		Testing: &config.TestingConfig{
			ScopeSuites: map[string]config.ScopeSuiteConfig{
				"api_integration": {Triggers: []string{"internal/db/**"}},
				"web_e2e":         {Triggers: []string{"src/app/**"}},
			},
		},
	}
	files := []fileEntry{
		{Path: "internal/db/billing.go"},
	}
	suites := computeScopeSuites(cfg, "api", files)
	if len(suites) != 1 || suites[0] != "api_integration" {
		t.Errorf("suites = %v, want [api_integration]", suites)
	}
}

func TestResolveRepoDir(t *testing.T) {
	cfg := &config.Config{
		Worktree: &config.WorktreeConfig{
			ParentDir: "/home/user/dev/project",
			RepoMap:   map[string]string{"api": "theraprac-api", "web": "theraprac-web"},
		},
	}
	got := resolveRepoDir(cfg, "api")
	want := "/home/user/dev/project/theraprac-api"
	if got != want {
		t.Errorf("resolveRepoDir = %q, want %q", got, want)
	}
}

func TestResolveRepoDirNoWorktree(t *testing.T) {
	cfg := &config.Config{}
	got := resolveRepoDir(cfg, "api")
	if got != "api" {
		t.Errorf("resolveRepoDir = %q, want 'api'", got)
	}
}

func TestE2ESpecFor(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"src/app/(app)/app/notes/page.tsx", "tests/e2e/notes.spec.ts"},
		{"src/app/(app)/app/clients/page.tsx", "tests/e2e/clients.spec.ts"},
		{"src/app/(app)/app/clients/[clientId]/page.tsx", "tests/e2e/clients.spec.ts"},
		{"src/app/(app)/app/clinical/sessions/[sessionId]/page.tsx", "tests/e2e/clinical-sessions.spec.ts"},
		{"src/app/(app)/app/billing/page.tsx", "tests/e2e/billing.spec.ts"},
		{"src/app/(app)/app/settings/page.tsx", "tests/e2e/settings.spec.ts"},
		// Non-page files return empty
		{"src/components/clinical/NotesSidebar.tsx", ""},
		{"src/lib/hooks/useNotes.ts", ""},
		{"src/app/(app)/app/layout.tsx", ""},
		// Marketing pages
		{"src/app/(marketing)/page.tsx", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := e2eSpecFor(tt.path)
			if got != tt.want {
				t.Errorf("e2eSpecFor(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestPRDocChangesRecorded(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	opts := mockGitOpts(
		"M\tinternal/db/billing.go\nA\tdocs/BILLING.md\nM\tREADME.md",
		"10\t5\tinternal/db/billing.go\n50\t0\tdocs/BILLING.md\n5\t2\tREADME.md",
		"sha123",
		map[string]string{
			"internal/db/billing.go": "h1",
			"docs/BILLING.md":       "h2",
			"README.md":             "h3",
		},
		map[string]bool{},
	)

	code := PR(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("PR returned %d", code)
	}

	item, _ := s.Get("T-003")
	raw := item.Doc.String()
	// Both docs/BILLING.md and README.md are classified as "doc"
	if !strings.Contains(raw, "BILLING.md") {
		t.Error("doc_changes should contain BILLING.md")
	}
	if !strings.Contains(raw, "README.md") {
		t.Error("doc_changes should contain README.md")
	}
}

func TestPRLastTouchedBySet(t *testing.T) {
	s, cfg := setupPRTestEnv(t)

	opts := mockGitOpts(
		"M\tinternal/db/billing.go",
		"10\t5\tinternal/db/billing.go",
		"sha123",
		map[string]string{"internal/db/billing.go": "h1"},
		map[string]bool{},
	)

	PR(s, cfg, "T-003", opts)

	item, _ := s.Get("T-003")
	lt, _ := item.Doc.GetField("last_touched")
	if lt == "" {
		t.Error("last_touched should be set after st pr")
	}
}

// setupPRTestEnvWithManifest creates a test env with .manifest dir pre-created
func setupPRTestEnvWithManifest(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupPRTestEnv(t)
	os.MkdirAll(cfg.ManifestDir(), 0755)
	return s, cfg
}
