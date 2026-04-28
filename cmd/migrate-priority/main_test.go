package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a single .md file under a fresh temp issues/ dir and
// returns the path so tests can run processFile against it.
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

// /code-review finding: CRLF line endings used to false-skip top-level
// fields because `strings.Split("\n")` left "\r" attached, making
// `ln != strings.TrimSpace(ln)` true. Files round-trip correctly now.
func TestProcessFile_CRLFLineEndings(t *testing.T) {
	path := fixture(t, "I-100-crlf.md",
		"id: I-100\r\ntype: issue\r\nstatus: open\r\ntitle: CRLF\r\nseverity: high\r\n")
	r, err := processFile(path, true /*dryRun*/)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil || r.Severity != "high" || r.NewPriority != 1 {
		t.Errorf("expected severity=high → priority=1, got %+v", r)
	}
}

// /code-review finding: inline `tags: [foo, bar]` form used to break
// ensureTag — it would append `- tech-debt` after the inline list,
// producing corrupt YAML. Now it normalizes to block form preserving
// existing tags + adding the new one.
func TestEnsureTag_InlineFormNormalized(t *testing.T) {
	lines := []string{
		"id: I-101",
		"title: x",
		"tags: [foo, bar]",
		"",
	}
	out := ensureTag(lines, "tech-debt")
	got := strings.Join(out, "\n")
	for _, want := range []string{"tags:", "- foo", "- bar", "- tech-debt"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[foo, bar]") {
		t.Errorf("inline form should have been normalized to block; got:\n%s", got)
	}
}

func TestEnsureTag_InlineFormAlreadyHasTag(t *testing.T) {
	lines := []string{"id: I-102", "tags: [tech-debt, foo]"}
	out := ensureTag(lines, "tech-debt")
	if len(out) != len(lines) {
		t.Errorf("expected no change when tag already present, got %d lines", len(out))
	}
}

func TestEnsureTag_BlockFormAppendsNewTag(t *testing.T) {
	lines := []string{"id: I-103", "tags:", "- foo", ""}
	out := ensureTag(lines, "tech-debt")
	got := strings.Join(out, "\n")
	if !strings.Contains(got, "- tech-debt") || !strings.Contains(got, "- foo") {
		t.Errorf("block form: expected both tags. Got:\n%s", got)
	}
}

func TestEnsureTag_BlockFormAlreadyHasTag(t *testing.T) {
	lines := []string{"id: I-104", "tags:", "- tech-debt", ""}
	out := ensureTag(lines, "tech-debt")
	if len(out) != len(lines) {
		t.Errorf("expected no change when tag already present, got %d lines", len(out))
	}
}

func TestEnsureTag_NoTagsField(t *testing.T) {
	lines := []string{"id: I-105", "title: x", ""}
	out := ensureTag(lines, "tech-debt")
	got := strings.Join(out, "\n")
	if !strings.Contains(got, "tags:") || !strings.Contains(got, "- tech-debt") {
		t.Errorf("expected fresh tags block. Got:\n%s", got)
	}
}

// /code-review finding: writeReport used to create the report file
// even in --dry-run mode. Now it writes to stdout (between markers)
// and the file is left untouched.
func TestWriteReport_DryRunDoesNotWriteFile(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "should-not-exist.md")
	writeReport(report, []fileResult{
		{Path: "I-100.md", Action: "add_priority", Severity: "high", NewPriority: 1},
	}, true /*dryRun*/)

	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote file at %s — should have been stdout-only (err=%v)", report, err)
	}
}

func TestWriteReport_ExecuteWritesFile(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.md")
	writeReport(report, []fileResult{
		{Path: "I-100.md", Action: "add_priority", Severity: "high", NewPriority: 1},
	}, false /*dryRun*/)

	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("execute mode should have written file: %v", err)
	}
	if !strings.Contains(string(body), "I-100.md") {
		t.Errorf("report missing I-100.md row")
	}
}

// /code-review finding (lower priority): migration table includes
// `critical/normal/minor` from real-world data even though I-406's
// original spec didn't list them. Pin the mapping so the table can't
// regress silently.
func TestSeverityToPriority_ExtendedRealWorldValues(t *testing.T) {
	for sev, want := range map[string]int{
		"critical": 0,
		"normal":   2,
		"minor":    4,
	} {
		got, ok := severityToPriority[sev]
		if !ok {
			t.Errorf("severity %q missing from migration table", sev)
			continue
		}
		if got != want {
			t.Errorf("severity %q maps to %d, want %d", sev, got, want)
		}
	}
}
