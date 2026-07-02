package manifest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		PRs: []PRRecord{
			{
				Repo:     "api",
				PRNumber: 42,
				HeadSHA:  "abc1234",
				Files: []FileRecord{
					{Path: "internal/db/billing.go", Action: "M", Type: "app", BlobHash: "def5678", LinesAdded: 10, LinesDeleted: 3},
				},
				CodeStats:   CodeStats{FilesChanged: 1, Insertions: 10, Deletions: 3},
				ScopeSuites: []string{"api_integration"},
				RecordedAt:  "2026-03-26T10:00:00-06:00",
			},
		},
	}

	if err := Save(dir, "T-001", m); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "T-001.json")); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	loaded, err := Load(dir, "T-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded.PRs) != 1 {
		t.Fatalf("PRs count = %d, want 1", len(loaded.PRs))
	}
	pr := loaded.PRs[0]
	if pr.Repo != "api" || pr.PRNumber != 42 || pr.HeadSHA != "abc1234" {
		t.Errorf("PR = %+v", pr)
	}
	if len(pr.Files) != 1 || pr.Files[0].Path != "internal/db/billing.go" {
		t.Errorf("Files = %+v", pr.Files)
	}
	if pr.CodeStats.Insertions != 10 || pr.CodeStats.Deletions != 3 {
		t.Errorf("CodeStats = %+v", pr.CodeStats)
	}
}

func TestLoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir, "T-999")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.PRs) != 0 {
		t.Errorf("PRs = %v, want empty", m.PRs)
	}
}

func TestAppendPR(t *testing.T) {
	dir := t.TempDir()

	pr1 := PRRecord{Repo: "api", PRNumber: 1, HeadSHA: "aaa", RecordedAt: "2026-03-26T10:00:00-06:00"}
	pr2 := PRRecord{Repo: "web", PRNumber: 2, HeadSHA: "bbb", RecordedAt: "2026-03-26T11:00:00-06:00"}

	if err := AppendPR(dir, "T-001", pr1); err != nil {
		t.Fatalf("AppendPR 1: %v", err)
	}
	if err := AppendPR(dir, "T-001", pr2); err != nil {
		t.Fatalf("AppendPR 2: %v", err)
	}

	m, err := Load(dir, "T-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.PRs) != 2 {
		t.Fatalf("PRs count = %d, want 2", len(m.PRs))
	}
	if m.PRs[0].Repo != "api" || m.PRs[1].Repo != "web" {
		t.Errorf("repos = %s, %s", m.PRs[0].Repo, m.PRs[1].Repo)
	}
}

func TestAllScopeSuites(t *testing.T) {
	m := &Manifest{
		PRs: []PRRecord{
			{ScopeSuites: []string{"api_integration"}},
			{ScopeSuites: []string{"api_integration", "web_e2e"}},
		},
	}
	suites := m.AllScopeSuites()
	if len(suites) != 2 {
		t.Fatalf("suites = %v, want 2", suites)
	}
	// Check dedup
	found := map[string]bool{}
	for _, s := range suites {
		found[s] = true
	}
	if !found["api_integration"] || !found["web_e2e"] {
		t.Errorf("suites = %v", suites)
	}
}

func TestAllScopeSuitesEmpty(t *testing.T) {
	m := &Manifest{}
	suites := m.AllScopeSuites()
	if len(suites) != 0 {
		t.Errorf("suites = %v, want empty", suites)
	}
}

func TestLoadBadJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "T-001.json"), []byte("not json"), 0644)
	_, err := Load(dir, "T-001")
	if err == nil {
		t.Error("expected error on bad JSON")
	}
}

func TestSaveCreatesDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "dir")
	m := &Manifest{PRs: []PRRecord{{Repo: "api"}}}
	if err := Save(dir, "T-001", m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir, "T-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.PRs) != 1 {
		t.Error("expected 1 PR")
	}
}

// TestAppendPRConcurrent guards I-1723: AppendPR's load->mutate->save cycle
// runs under an exclusive flock, so concurrent appends to the same manifest
// must all survive. Pre-flock, interleaved goroutines silently dropped each
// other's records (observed live 2026-07-02 on I-1719's PR manifest).
func TestAppendPRConcurrent(t *testing.T) {
	dir := t.TempDir()
	const n = 20

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- AppendPR(dir, "T-900", PRRecord{
				Repo:     "as",
				PRNumber: 100 + i,
				HeadSHA:  "abc",
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendPR: %v", err)
		}
	}

	m, err := Load(dir, "T-900")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.PRs) != n {
		t.Errorf("lost update: %d/%d PR records survived concurrent AppendPR", len(m.PRs), n)
	}
}
