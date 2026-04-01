package plan

import (
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

	Save(dir, "T-001", &Plan{Approach: "test"})
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

func TestSaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "plans")
	err := Save(dir, "T-001", &Plan{Approach: "test"})
	if err != nil {
		t.Fatalf("Save should create nested dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "T-001.md")); err != nil {
		t.Error("file should exist")
	}
}
