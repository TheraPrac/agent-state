package plan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	p := &Plan{
		ScopeRepos:    []string{"theraprac-api", "theraprac-web"},
		Approved:      true,
		ApprovedAt:    "2026-03-31T10:00:00-06:00",
		Approach:      "Add a new API endpoint and React component.",
		Steps:         []string{"Create Go handler", "Add OpenAPI spec", "Build React component"},
		FilesToCreate: []string{"internal/handlers/new.go", "src/components/New.tsx"},
		FilesToModify: []string{"cmd/server/main.go", "api/openapi/api.yaml"},
		ACs:           []string{"cmd: go test ./...", "cmd: npm run type-check"},
		Revisions: []Revision{
			{Timestamp: "2026-03-31T10:00:00-06:00", Summary: "Initial plan"},
		},
	}

	dir := t.TempDir()
	err := Save(dir, "T-001", p)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, "T-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded.ScopeRepos) != 2 || loaded.ScopeRepos[0] != "theraprac-api" {
		t.Errorf("ScopeRepos = %v", loaded.ScopeRepos)
	}
	if !loaded.Approved {
		t.Error("Approved should be true")
	}
	if loaded.ApprovedAt != "2026-03-31T10:00:00-06:00" {
		t.Errorf("ApprovedAt = %q", loaded.ApprovedAt)
	}
	if loaded.Approach != "Add a new API endpoint and React component." {
		t.Errorf("Approach = %q", loaded.Approach)
	}
	if len(loaded.Steps) != 3 {
		t.Errorf("Steps = %d, want 3", len(loaded.Steps))
	}
	if len(loaded.FilesToCreate) != 2 {
		t.Errorf("FilesToCreate = %d, want 2", len(loaded.FilesToCreate))
	}
	if len(loaded.FilesToModify) != 2 {
		t.Errorf("FilesToModify = %d, want 2", len(loaded.FilesToModify))
	}
	if len(loaded.ACs) != 2 {
		t.Errorf("ACs = %d, want 2", len(loaded.ACs))
	}
	if len(loaded.Revisions) != 1 {
		t.Errorf("Revisions = %d, want 1", len(loaded.Revisions))
	}
}

func TestLoadNotFound(t *testing.T) {
	p, err := Load(t.TempDir(), "T-999")
	if err != nil {
		t.Fatalf("Load should not error for missing file: %v", err)
	}
	if p != nil {
		t.Error("Load should return nil for missing file")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if Exists(dir, "T-001") {
		t.Error("should not exist")
	}

	Save(dir, "T-001", &Plan{
		Approach:   "test",
		ScopeRepos: []string{"api"},
		ACs:        []string{"cmd: echo ok"},
	})
	if !Exists(dir, "T-001") {
		t.Error("should exist after save")
	}
}

func TestPlainText(t *testing.T) {
	p := &Plan{
		Approach:      "Build the thing.",
		ScopeRepos:    []string{"api"},
		Steps:         []string{"Step one", "Step two"},
		FilesToCreate: []string{"new.go"},
		FilesToModify: []string{"main.go"},
	}
	text := PlainText(p)
	if !strings.Contains(text, "Build the thing.") {
		t.Error("missing approach")
	}
	if !strings.Contains(text, "Step one") {
		t.Error("missing steps")
	}
	if !strings.Contains(text, "new.go") {
		t.Error("missing files to create")
	}
}

func TestPlainTextFallback(t *testing.T) {
	p := &Plan{RawText: "---\nscope_repos: [api]\n---\n\nJust some raw text."}
	text := PlainText(p)
	if !strings.Contains(text, "Just some raw text.") {
		t.Errorf("fallback should strip frontmatter, got: %q", text)
	}
	if strings.Contains(text, "scope_repos") {
		t.Error("fallback should not include frontmatter")
	}
}

func TestParseFrontmatter(t *testing.T) {
	text := "---\nscope_repos: [api, web]\nplan_approved: true\napproved_at: 2026-03-31\n---\n\n## Approach\nDo stuff."
	p, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.ScopeRepos) != 2 {
		t.Errorf("ScopeRepos = %v, want 2", p.ScopeRepos)
	}
	if !p.Approved {
		t.Error("Approved should be true")
	}
	if p.Approach != "Do stuff." {
		t.Errorf("Approach = %q", p.Approach)
	}
}

func TestParseScopeFromMarkdown(t *testing.T) {
	// No frontmatter — scope_repos extracted from ## Scope section
	text := "## Approach\nDo ops stuff.\n\n## Scope\nRepos: theraprac-infra\n\n## Acceptance Criteria\n- cmd: echo ok\n"
	p, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.ScopeRepos) != 1 || p.ScopeRepos[0] != "theraprac-infra" {
		t.Errorf("ScopeRepos = %v, want [theraprac-infra]", p.ScopeRepos)
	}

	// Multiple repos
	text2 := "## Approach\nCleanup.\n\n## Scope\nRepos: theraprac-api, theraprac-web, theraprac-infra\n\n## Acceptance Criteria\n- cmd: echo ok\n"
	p2, err := Parse(text2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p2.ScopeRepos) != 3 {
		t.Errorf("ScopeRepos = %v, want 3 repos", p2.ScopeRepos)
	}

	// Frontmatter takes precedence over ## Scope
	text3 := "---\nscope_repos: [api]\n---\n\n## Approach\nTest.\n\n## Scope\nRepos: web, infra\n"
	p3, err := Parse(text3)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p3.ScopeRepos) != 1 || p3.ScopeRepos[0] != "api" {
		t.Errorf("ScopeRepos = %v, want [api] (frontmatter should win)", p3.ScopeRepos)
	}
}

func TestSaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "plans")
	err := Save(dir, "T-001", &Plan{
		Approach:   "test",
		ScopeRepos: []string{"api"},
		ACs:        []string{"cmd: echo ok"},
	})
	if err != nil {
		t.Fatalf("Save should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "T-001.md")); err != nil {
		t.Error("file should exist")
	}
}

func TestSaveRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()

	// Missing scope_repos
	err := Save(dir, "T-001", &Plan{Approach: "test", ACs: []string{"cmd: echo ok"}})
	if err == nil {
		t.Error("should reject plan missing scope_repos")
	}

	// Missing approach
	err = Save(dir, "T-002", &Plan{ScopeRepos: []string{"api"}, ACs: []string{"cmd: echo ok"}})
	if err == nil {
		t.Error("should reject plan missing approach")
	}

	// Missing ACs
	err = Save(dir, "T-003", &Plan{Approach: "test", ScopeRepos: []string{"api"}})
	if err == nil {
		t.Error("should reject plan missing ACs")
	}

	// Complete plan should succeed
	err = Save(dir, "T-004", &Plan{
		Approach:   "test",
		ScopeRepos: []string{"api"},
		ACs:        []string{"cmd: echo ok"},
	})
	if err != nil {
		t.Errorf("complete plan should save: %v", err)
	}
}

// TestSaveWithOptsLenientWarnsOnUnverifiableACs verifies the I-511
// default-mode contract: un-verifiable ACs produce stderr warnings
// but do NOT fail the save. Existing legacy plans / migrations
// continue to round-trip.
func TestSaveWithOptsLenientWarnsOnUnverifiableACs(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	p := &Plan{
		Approach:   "test approach",
		ScopeRepos: []string{"api"},
		ACs:        []string{"works correctly", "fix the bug"},
	}
	err := SaveWithOpts(dir, "T-100", p, SaveOpts{Stderr: &buf})
	if err != nil {
		t.Fatalf("lenient save should succeed despite vague ACs; got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "save warning") {
		t.Errorf("expected stderr warning header; got %q", out)
	}
	if !strings.Contains(out, "works correctly") || !strings.Contains(out, "fix the bug") {
		t.Errorf("expected each un-verifiable AC echoed in warning; got %q", out)
	}
}

// TestSaveWithOptsStrictRejectsUnverifiableACs verifies the I-511
// strict-mode contract: un-verifiable ACs cause Save to return an
// error so callers like `st plan approve --strict` can refuse.
func TestSaveWithOptsStrictRejectsUnverifiableACs(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	p := &Plan{
		Approach:   "test approach",
		ScopeRepos: []string{"api"},
		ACs:        []string{"works correctly"},
	}
	err := SaveWithOpts(dir, "T-101", p, SaveOpts{Strict: true, Stderr: &buf})
	if err == nil {
		t.Fatal("strict save should reject un-verifiable AC")
	}
	if !strings.Contains(err.Error(), "un-verifiable") {
		t.Errorf("error should mention 'un-verifiable'; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "works correctly") {
		t.Errorf("error should echo the offending AC; got %q", err.Error())
	}
}

// TestSaveWithOptsStrictAcceptsVerifiableACs verifies the positive
// strict-mode case: verifiable ACs save cleanly under --strict.
func TestSaveWithOptsStrictAcceptsVerifiableACs(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Approach:   "test approach",
		ScopeRepos: []string{"api"},
		ACs:        []string{"cmd: go test ./internal/foo/...", "TestFoo passes"},
	}
	if err := SaveWithOpts(dir, "T-102", p, SaveOpts{Strict: true}); err != nil {
		t.Errorf("strict save should accept verifiable ACs; got %v", err)
	}
}

// TestSaveLoadReport round-trips a plan-review report sidecar (I-565).
// Asserts that ReportExists is false on a fresh dir, true after
// SaveReport, and that LoadReport returns the body verbatim.
// Confirms a missing report gives ("", nil) — same semantics as Load.
func TestSaveLoadReport(t *testing.T) {
	dir := t.TempDir()

	if ReportExists(dir, "T-001") {
		t.Error("ReportExists should be false before save")
	}
	body, err := LoadReport(dir, "T-001")
	if err != nil {
		t.Fatalf("LoadReport on missing file: %v", err)
	}
	if body != "" {
		t.Errorf("LoadReport on missing file = %q, want empty", body)
	}

	want := "## Recommendation\nAccept\n\n## Notes\n- Confidence: high\n"
	if err := SaveReport(dir, "T-001", want); err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	if !ReportExists(dir, "T-001") {
		t.Error("ReportExists should be true after save")
	}
	got, err := LoadReport(dir, "T-001")
	if err != nil {
		t.Fatalf("LoadReport: %v", err)
	}
	if got != want {
		t.Errorf("LoadReport = %q, want %q", got, want)
	}

	if want, got := filepath.Join(dir, "T-001.report.md"), ReportPath(dir, "T-001"); want != got {
		t.Errorf("ReportPath = %q, want %q", got, want)
	}
}

// TestSaveWithOptsRejectedSkipsACValidation verifies that Rejected
// plans bypass both completeness AND AC-quality checks. Drafts may
// have empty / vague ACs while still being saved as rejection
// artifacts.
func TestSaveWithOptsRejectedSkipsACValidation(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Rejected: true,
		ACs:      []string{"works correctly"}, // vague + missing scope/approach
	}
	if err := SaveWithOpts(dir, "T-103", p, SaveOpts{Strict: true}); err != nil {
		t.Errorf("rejected plan should save under strict; got %v", err)
	}
}
