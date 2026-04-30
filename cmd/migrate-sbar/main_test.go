package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// processFile on a legacy item with summary: seeds sbar.background
// from the summary and inserts placeholder TODOs for the other three
// fields. The sbar block lands directly after the summary block.
func TestProcessFile_SeedsBackgroundFromSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "I-001-legacy.md")
	body := `id: I-001
type: issue
status: queued
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00
title: Legacy item with a summary

summary: |-
  Two-paragraph blob of pre-SBAR content.
  Second line.

tags:
- legacy
`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r, err := processFile(path, false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil || r.Action != "seeded_from_summary" {
		t.Fatalf("result=%+v, want seeded_from_summary", r)
	}

	out, _ := os.ReadFile(path)
	s := string(out)
	if !strings.Contains(s, "sbar:") {
		t.Errorf("missing sbar block:\n%s", s)
	}
	if !strings.Contains(s, "background: |-") {
		t.Errorf("missing background block:\n%s", s)
	}
	if !strings.Contains(s, "Two-paragraph blob of pre-SBAR content.") {
		t.Errorf("background not seeded from summary:\n%s", s)
	}
	if !strings.Contains(s, "TODO: one-line symptom") {
		t.Errorf("missing situation placeholder:\n%s", s)
	}
	if !strings.Contains(s, "TODO: diagnosis") {
		t.Errorf("missing assessment placeholder:\n%s", s)
	}
	if !strings.Contains(s, "TODO: proposed fix") {
		t.Errorf("missing recommendation placeholder:\n%s", s)
	}
	// Original summary stays in place — migration is additive.
	if !strings.Contains(s, "summary: |-") {
		t.Errorf("legacy summary block was wiped:\n%s", s)
	}

	// Idempotent: a second pass is a no-op.
	r2, err := processFile(path, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if r2 == nil || r2.Action != "skipped_has_sbar" {
		t.Errorf("second pass should skip, got %+v", r2)
	}
}

// processFile on an item with no summary: at all writes a fully
// placeholder sbar block so the schema is uniform corpus-wide.
func TestProcessFile_AddsEmptyWhenNoSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "I-002-no-summary.md")
	body := `id: I-002
type: issue
status: queued
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00
title: Item with no summary

tags:
- bare
`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r, err := processFile(path, false)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil || r.Action != "added_empty" {
		t.Fatalf("result=%+v, want added_empty", r)
	}

	out, _ := os.ReadFile(path)
	s := string(out)
	if !strings.Contains(s, "TODO: prior context") {
		t.Errorf("background should be a TODO placeholder:\n%s", s)
	}
}

// dry-run mode does not write — original file is preserved byte-for-byte.
func TestProcessFile_DryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "I-003-dryrun.md")
	body := `id: I-003
type: issue
status: queued
created: 2026-04-01T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00
title: dry run case

summary: |-
  short
`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	original, _ := os.ReadFile(path)

	r, err := processFile(path, true)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if r == nil {
		t.Fatal("dry-run should still report the planned action")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Errorf("dry-run modified the file")
	}
}

// extractTopLevelBlock pulls the inner content of a |- block, with
// the indent stripped. Used by the migration to seed background.
func TestExtractTopLevelBlock(t *testing.T) {
	content := `id: I-001
title: x

summary: |-
  Line one.
  Line two.

tags:
- []
`
	got := extractTopLevelBlock(content, "summary")
	want := "Line one.\nLine two."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
