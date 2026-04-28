package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "issues")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// CRLF tolerance: findField TrimRight's "\r" so files written on
// Windows or pasted from rich-text editors don't false-skip every
// top-level field.
func TestProcessFile_CRLFLineEndings(t *testing.T) {
	path := fixture(t, "I-100-crlf.md",
		"id: I-100\r\ntype: issue\r\nstatus: open\r\ntitle: CRLF case\r\n")
	r, err := processFile(path, true /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil || r.OldStatus != "open" || r.NewStatus != "queued" {
		t.Errorf("expected open → queued, got %+v", r)
	}
}

// Dry-run must NOT write files. Regression guard: an earlier version
// of the I-406 sibling migrator overwrote files even with --dry-run set.
func TestProcessFile_DryRunDoesNotWriteFile(t *testing.T) {
	body := "id: I-101\ntype: issue\nstatus: open\ntitle: dry\n"
	path := fixture(t, "I-101-dry.md", body)
	r, err := processFile(path, true /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil {
		t.Fatalf("expected a remap result, got nil")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != body {
		t.Errorf("dry-run mutated file. got:\n%s\nwant:\n%s", got, body)
	}
}

// Execute mode writes the new status to disk.
func TestProcessFile_ExecuteWritesFile(t *testing.T) {
	path := fixture(t, "I-102-exec.md",
		"id: I-102\ntype: issue\nstatus: resolved\ntitle: exec\n")
	r, err := processFile(path, false /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil || r.OldStatus != "resolved" || r.NewStatus != "done" {
		t.Errorf("expected resolved → done, got %+v", r)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(got), "status: done") {
		t.Errorf("file not rewritten. got:\n%s", got)
	}
	if strings.Contains(string(got), "status: resolved") {
		t.Errorf("legacy status still present after rewrite")
	}
}

// Already-conforming items return nil — no result row, no rewrite.
// Covers queued/active/done/abandoned/archived.
func TestProcessFile_AlreadyConformingSkipped(t *testing.T) {
	for _, status := range []string{"queued", "active", "done", "abandoned", "archived"} {
		t.Run(status, func(t *testing.T) {
			body := "id: I-200\ntype: issue\nstatus: " + status + "\ntitle: x\n"
			path := fixture(t, "I-200-"+status+".md", body)
			r, err := processFile(path, false /*dryRun*/)
			if err != nil {
				t.Fatalf("processFile: %v", err)
			}
			if r != nil {
				t.Errorf("expected nil for already-conforming status %q, got %+v", status, r)
			}
			// Sanity: file untouched.
			got, _ := os.ReadFile(path)
			if string(got) != body {
				t.Errorf("file mutated for already-conforming status %q", status)
			}
		})
	}
}

// Pin the migration table so a refactor can't silently change the
// I-433 mapping.
func TestStatusRemap_TableContents(t *testing.T) {
	want := map[string]string{
		"open":      "queued",
		"resolved":  "done",
		"wontfix":   "abandoned",
		"completed": "done",
	}
	for from, to := range want {
		got, ok := statusRemap[from]
		if !ok {
			t.Errorf("statusRemap missing %q", from)
			continue
		}
		if got != to {
			t.Errorf("statusRemap[%q] = %q, want %q", from, got, to)
		}
	}
	for _, conforming := range []string{"queued", "active", "done", "abandoned", "archived"} {
		if _, ok := statusRemap[conforming]; ok {
			t.Errorf("statusRemap[%q] should not exist — already conforming", conforming)
		}
	}
}
