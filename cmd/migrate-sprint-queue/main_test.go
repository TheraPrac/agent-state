package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile is a tiny helper. Avoids the temptation to depend on
// internal/command for test setup.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBackfillAddsMissingSprintMembers(t *testing.T) {
	items := []itemRow{
		{ID: "T-001", Status: "queued", Sprint: "demo"},
		{ID: "T-002", Status: "active", Sprint: "demo"},
		{ID: "T-003", Status: "queued"}, // not in sprint — skipped silently
	}
	results, updated := backfill(items, nil)

	if len(updated) != 2 {
		t.Fatalf("updated has %d entries, want 2", len(updated))
	}
	for _, e := range updated {
		if e.Source != "sprint" {
			t.Errorf("entry %s Source = %q, want sprint", e.ID, e.Source)
		}
		if e.Approved {
			t.Errorf("entry %s should be pending", e.ID)
		}
		if e.AddedBy != "migration" {
			t.Errorf("entry %s AddedBy = %q, want migration", e.ID, e.AddedBy)
		}
	}
	for _, r := range results {
		if r.Action != "added" {
			t.Errorf("result for %s = %q, want added", r.ID, r.Action)
		}
	}
}

func TestBackfillIsIdempotent(t *testing.T) {
	items := []itemRow{
		{ID: "T-001", Status: "queued", Sprint: "demo"},
	}
	// First pass.
	results1, entries1 := backfill(items, nil)
	if results1[0].Action != "added" {
		t.Fatalf("first pass action = %q, want added", results1[0].Action)
	}
	if len(entries1) != 1 {
		t.Fatalf("first pass entries = %d, want 1", len(entries1))
	}

	// Second pass — should be a no-op (already queued).
	results2, entries2 := backfill(items, entries1)
	if results2[0].Action != "skipped_already_queued" {
		t.Errorf("second pass action = %q, want skipped_already_queued", results2[0].Action)
	}
	if len(entries2) != 1 {
		t.Errorf("second pass entries = %d, want 1 (no duplicate)", len(entries2))
	}
}

// Pre-existing queue entries are left exactly as the operator left them —
// the migration never rewrites Source, even on legacy entries with an
// empty Source field. The operator may have curated those manually under
// pre-I-488 code; stamping them as sprint-sourced would silently let a
// future `st sprint rm` cascade-remove operator-curated work.
func TestBackfillNeverRewritesExistingEntry(t *testing.T) {
	items := []itemRow{
		{ID: "T-001", Status: "queued", Sprint: "demo"},
		{ID: "T-002", Status: "queued", Sprint: "demo"},
	}
	existing := []queueEntry{
		{ID: "T-001", Approved: true, AddedBy: "user"},                     // legacy, empty source
		{ID: "T-002", Approved: true, AddedBy: "user", Source: "manual"},   // explicit manual
	}
	results, updated := backfill(items, existing)
	if len(updated) != 2 {
		t.Fatalf("updated entries = %d, want 2 (no new appended)", len(updated))
	}
	if updated[0].Source != "" {
		t.Errorf("legacy empty-source entry rewritten — Source = %q", updated[0].Source)
	}
	if updated[1].Source != "manual" {
		t.Errorf("manual entry overwritten — Source = %q", updated[1].Source)
	}
	for _, r := range results {
		if r.Action != "skipped_already_queued" {
			t.Errorf("%s action = %q, want skipped_already_queued", r.ID, r.Action)
		}
	}
}

func TestBackfillSkipsTerminalItems(t *testing.T) {
	items := []itemRow{
		{ID: "T-001", Status: "done", Sprint: "demo"},
		{ID: "T-002", Status: "abandoned", Sprint: "demo"},
		{ID: "T-003", Status: "archived", Sprint: "demo"},
		{ID: "T-004", Status: "resolved", Sprint: "demo"}, // legacy, still terminal
	}
	results, updated := backfill(items, nil)
	if len(updated) != 0 {
		t.Errorf("expected no entries for terminal items, got %d", len(updated))
	}
	for _, r := range results {
		if r.Action != "skipped_terminal" {
			t.Errorf("%s action = %q, want skipped_terminal", r.ID, r.Action)
		}
	}
}

// End-to-end: scan a temp workspace, write queue, re-scan, expect
// no-op on the second run.
func TestEndToEndIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tasks", "T-001-x.md"), `id: T-001
type: task
status: queued
sprint: demo
title: x
`)
	writeFile(t, filepath.Join(root, "issues", "I-001-y.md"), `id: I-001
type: issue
status: active
sprint: demo
title: y
`)
	writeFile(t, filepath.Join(root, "archive", "T-099-old.md"), `id: T-099
type: task
status: done
sprint: demo
title: old
`)

	dirs := []string{
		filepath.Join(root, "issues"),
		filepath.Join(root, "tasks"),
		filepath.Join(root, "archive"),
	}
	queuePath := filepath.Join(root, ".as", "queue.yaml")

	// Pass 1.
	items, err := scanItems(dirs)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	entries, err := loadQueue(queuePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	results1, updated := backfill(items, entries)
	if err := saveQueue(queuePath, updated); err != nil {
		t.Fatalf("save: %v", err)
	}
	added := 0
	for _, r := range results1 {
		if r.Action == "added" {
			added++
		}
	}
	if added != 2 {
		t.Errorf("pass 1: added %d, want 2 (T-099 is terminal)", added)
	}

	// Pass 2 — should be a pure no-op.
	items2, _ := scanItems(dirs)
	entries2, _ := loadQueue(queuePath)
	results2, updated2 := backfill(items2, entries2)
	if len(updated2) != 2 {
		t.Errorf("pass 2 entries = %d, want 2 (no duplicates)", len(updated2))
	}
	for _, r := range results2 {
		if r.Action == "added" {
			t.Errorf("pass 2 unexpected action %s for %s", r.Action, r.ID)
		}
	}
}
