// Package manifest provides sidecar JSON storage for PR manifests.
// Each item gets a .manifest/<id>.json file with full file analysis data.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Manifest holds the full PR + file analysis data for an item.
type Manifest struct {
	PRs []PRRecord `json:"prs"`
}

// PRRecord captures one `st pr` invocation.
type PRRecord struct {
	Repo        string       `json:"repo"`
	PRNumber    int          `json:"pr_number"`
	HeadSHA     string       `json:"head_sha"`
	Files       []FileRecord `json:"files"`
	CodeStats   CodeStats    `json:"code_stats"`
	ScopeSuites []string     `json:"scope_suites"`
	RecordedAt  string       `json:"recorded_at"`
}

// FileRecord describes a single changed file.
type FileRecord struct {
	Path         string `json:"path"`
	Action       string `json:"action"` // A, M, D, R
	Type         string `json:"type"`   // app, test, migration, config, spec, doc
	BlobHash     string `json:"blob_hash"`
	LinesAdded   int    `json:"lines_added"`
	LinesDeleted int    `json:"lines_deleted"`
}

// CodeStats summarizes the overall change.
type CodeStats struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// Load reads a manifest from .manifest/<id>.json. Returns an empty manifest if not found.
func Load(dir, id string) (*Manifest, error) {
	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{}, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Save writes a manifest to .manifest/<id>.json.
func Save(dir, id string, m *Manifest) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".json"), data, 0644)
}

// AppendPR loads the manifest, appends a PR record, and saves.
//
// I-1723: the load→mutate→save cycle runs under an exclusive flock on
// .manifest/<id>.json.lock so concurrent st processes (multiple agents share
// one canonical working tree) cannot interleave and silently drop each
// other's PR records — the classic lost update observed live 2026-07-02 on
// I-1719's manifest. The flock blocks (no timeout): holders span a single
// JSON read+write (microseconds) and the kernel releases on process death.
func AppendPR(dir, id string, pr PRRecord) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	lockPath := filepath.Join(dir, id+".json.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open manifest lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	m, err := Load(dir, id)
	if err != nil {
		return err
	}
	// Replace existing entry for the same repo#number (dedup on re-record)
	replaced := false
	for i, existing := range m.PRs {
		if existing.Repo == pr.Repo && existing.PRNumber == pr.PRNumber {
			m.PRs[i] = pr
			replaced = true
			break
		}
	}
	if !replaced {
		m.PRs = append(m.PRs, pr)
	}
	return Save(dir, id, m)
}

// AllScopeSuites returns the union of scope suites across all PRs.
func (m *Manifest) AllScopeSuites() []string {
	seen := map[string]bool{}
	var result []string
	for _, pr := range m.PRs {
		for _, s := range pr.ScopeSuites {
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
	}
	return result
}
