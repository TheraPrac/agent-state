package command

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/evidence"
	"github.com/theraprac/agent-state/internal/model"
)

// --- TestReviewFileMapping ---

func TestReviewFileMapping(t *testing.T) {
	cases := []struct {
		path     string
		wantRule string
	}{
		{"internal/foo.go", "Backend (Go)"},
		{"internal/foo_test.go", "Backend (Go)"},
		{"src/components/Patient.tsx", "Frontend"},
		{"src/lib/hooks/useData.ts", "Frontend"},
		{"db/changelog/001-create.sql", "Liquibase"},
		{"db/changelog/002-seed.xml", "Liquibase"},
		{"scripts/deploy.sh", "Bash"},
		{"api/openapi/api.yaml", "OpenAPI"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			stds := fileReviewStandards(tc.path)
			if len(stds) == 0 {
				t.Errorf("fileReviewStandards(%q): got no standards, want %q", tc.path, tc.wantRule)
				return
			}
			found := false
			for _, s := range stds {
				if strings.Contains(s, tc.wantRule) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("fileReviewStandards(%q): %v does not contain %q", tc.path, stds, tc.wantRule)
			}
		})
	}

	noRule := []string{"go.mod", "package.json", "README.md", "docker-compose.yml"}
	for _, p := range noRule {
		if stds := fileReviewStandards(p); stds != nil {
			t.Errorf("fileReviewStandards(%q): got %v, want nil", p, stds)
		}
	}
}

// --- mock helpers ---

// mockEvidenceBackend captures uploads for test assertions.
type mockEvidenceBackend struct {
	Uploads map[string][]byte
}

func newMockBackend() *mockEvidenceBackend {
	return &mockEvidenceBackend{Uploads: make(map[string][]byte)}
}

func (m *mockEvidenceBackend) Upload(key string, r io.Reader) (string, error) {
	data, _ := io.ReadAll(r)
	m.Uploads[key] = data
	return "mock://" + key, nil
}

func (m *mockEvidenceBackend) Download(key string, w io.Writer) error { return nil }
func (m *mockEvidenceBackend) List(prefix string) ([]string, error)   { return nil, nil }
func (m *mockEvidenceBackend) URI(key string) string                   { return "mock://" + key }
func (m *mockEvidenceBackend) Delete(key string) error                 { return nil }

var _ evidence.Backend = (*mockEvidenceBackend)(nil)

func mockReviewEngine(report *ReviewReport) RunEngine {
	return RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			reportJSON, _ := json.Marshal(report)
			result := ClaudeResult{
				Type:    "result",
				Subtype: "success",
				Result:  string(reportJSON),
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
	}
}

func syntheticDiff(sha string) func(*config.Config, string) (string, string, error) {
	return func(_ *config.Config, _ string) (string, string, error) {
		diff := "diff --git a/internal/foo.go b/internal/foo.go\n" +
			"index 0000000..1111111 100644\n" +
			"+++ b/internal/foo.go\n" +
			"+func newFoo() {}\n"
		return diff, sha, nil
	}
}

// --- TestReview ---

func TestReview(t *testing.T) {
	s, cfg := setupTestEnv(t)

	report := &ReviewReport{
		ReviewedSHA: "abc1234",
		Verdict:     "pass",
		Files: []ReviewFile{
			{
				Path:                "internal/foo.go",
				ApplicableStandards: []string{"bugbot-rules: Backend (Go) Rules 1-6, 10-15, 20-26"},
				Violations:          []ReviewViolation{},
				Status:              "pass",
			},
		},
		Summary: "1 file reviewed, 0 violations.",
	}

	backend := newMockBackend()
	opts := ReviewOpts{
		Engine:      mockReviewEngine(report),
		Backend:     backend,
		CollectDiff: syntheticDiff("abc1234"),
	}

	code := Review(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("Review pass: got exit %d, want 0", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := item.Doc.GetField("review_evidence")
	if !strings.HasPrefix(ev, "pass abc1234") {
		t.Errorf("review_evidence: got %q, want prefix \"pass abc1234\"", ev)
	}
	if !strings.Contains(ev, "evidence:") {
		t.Errorf("review_evidence: missing evidence URI: %q", ev)
	}

	// Evidence upload key must match expected pattern.
	uploaded := false
	for key := range backend.Uploads {
		if strings.Contains(key, "T-003/review/abc1234") && strings.HasSuffix(key, "report.json.gz") {
			uploaded = true
		}
	}
	if !uploaded {
		t.Errorf("expected upload at T-003/review/abc1234/.../report.json.gz; got keys: %v",
			uploadKeys(backend.Uploads))
	}
}

func TestReviewFail(t *testing.T) {
	s, cfg := setupTestEnv(t)

	report := &ReviewReport{
		ReviewedSHA: "abc1234",
		Verdict:     "fail",
		Files: []ReviewFile{
			{
				Path:                "internal/rls.go",
				ApplicableStandards: []string{"bugbot-rules: Backend (Go) Rules 1-6, 10-15, 20-26"},
				Violations: []ReviewViolation{
					{RuleID: "1", Line: 42, Finding: "Missing RLS policy for theraprac_billing role"},
				},
				Status: "fail",
			},
		},
		Summary: "1 violation.",
	}

	opts := ReviewOpts{
		Engine:      mockReviewEngine(report),
		CollectDiff: syntheticDiff("abc1234"),
	}

	code := Review(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Review fail: got exit %d, want 1", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := item.Doc.GetField("review_evidence")
	if !strings.HasPrefix(ev, "fail abc1234") {
		t.Errorf("review_evidence: got %q, want prefix \"fail abc1234\"", ev)
	}
}

func TestReviewSHAMismatch(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Sub-agent echoes a stale SHA.
	report := &ReviewReport{
		ReviewedSHA: "stale00",
		Verdict:     "pass",
		Summary:     "ok",
	}

	opts := ReviewOpts{
		Engine:      mockReviewEngine(report),
		CollectDiff: syntheticDiff("newsha1"), // current HEAD
	}

	code := Review(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("SHA mismatch: got exit %d, want 1", code)
	}

	item, _ := s.Get("T-003")
	ev, _ := item.Doc.GetField("review_evidence")
	if !strings.HasPrefix(ev, "fail newsha1") {
		t.Errorf("SHA mismatch: review_evidence got %q, want prefix \"fail newsha1\"", ev)
	}
}

func TestReviewNotActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-001 is queued.
	code := Review(s, cfg, "T-001", ReviewOpts{})
	if code != 1 {
		t.Errorf("non-active item: got %d, want 1", code)
	}
}

func TestReviewNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := Review(s, cfg, "T-999", ReviewOpts{})
	if code != 1 {
		t.Errorf("missing item: got %d, want 1", code)
	}
}

func TestReviewEmptySHA(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// CollectDiff returns a non-empty diff but empty SHA — e.g., all git rev-parse calls failed.
	opts := ReviewOpts{
		CollectDiff: syntheticDiff(""),
	}
	code := Review(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("Review with empty SHA: got %d, want 1", code)
	}
	// Evidence must NOT be written — an empty SHA would corrupt the field format.
	item, _ := s.Get("T-003")
	ev, _ := item.Doc.GetField("review_evidence")
	if ev != "" {
		t.Errorf("review_evidence should not be written on empty SHA: got %q", ev)
	}
}

// --- parse tests ---

func TestParseReviewReport(t *testing.T) {
	raw := `{"reviewed_sha":"abc1234","verdict":"pass","files":[],"summary":"ok"}`
	report, err := parseReviewReport(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Verdict != "pass" || report.ReviewedSHA != "abc1234" {
		t.Errorf("got %+v", report)
	}
}

func TestParseReviewReportWithProse(t *testing.T) {
	raw := `Here is my review: {"reviewed_sha":"abc","verdict":"fail","files":[],"summary":"fail"} Done.`
	report, err := parseReviewReport(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Verdict != "fail" {
		t.Errorf("got verdict %q, want fail", report.Verdict)
	}
}

func TestParseReviewReportNoJSON(t *testing.T) {
	_, err := parseReviewReport("no json here")
	if err == nil {
		t.Error("expected error for no-JSON input")
	}
}

func TestBuildReviewPromptContainsSHA(t *testing.T) {
	prompt := buildReviewPrompt("T-123", "abc1234", "diff text")
	if !strings.Contains(prompt, "abc1234") {
		t.Error("SHA not in prompt")
	}
	if !strings.Contains(prompt, "diff text") {
		t.Error("diff not in prompt")
	}
	if !strings.Contains(prompt, "reviewed_sha") {
		t.Error("JSON schema not in prompt")
	}
}

func TestReviewEvidenceFieldIsTopLevel(t *testing.T) {
	s, _ := setupTestEnv(t)

	// Directly write review_evidence as a top-level field.
	s.Mutate("T-003", func(it *model.Item) error {
		it.Doc.SetField("review_evidence", "pass abc1234 2026-06-14T10:00:00-06:00 evidence:")
		return nil
	})

	item, _ := s.Get("T-003")
	ev, ok := item.Doc.GetField("review_evidence")
	if !ok || !strings.HasPrefix(ev, "pass abc1234") {
		t.Errorf("review_evidence top-level field: got %q ok=%v", ev, ok)
	}
}

func uploadKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
