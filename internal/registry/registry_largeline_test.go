package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// I-673: a single line over the old 512 KB bufio.Scanner cap made the
// whole shared registry unreadable. The reader is now unbounded.

func TestRegistryLoad_HandlesOversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.yaml")

	// A single-line message far larger than the retired 512 KB cap.
	big := strings.Repeat("x", 600*1024)
	r := &Registry{}
	r.AddNote("agent-b", "sess-1", big)
	r.AddNote("agent-b", "sess-1", "a normal note after the big one")
	if err := r.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Sanity: the file really does contain a >512 KB physical line
	// (the exact pathology that broke the old scanner).
	data, _ := os.ReadFile(path)
	longest := 0
	for _, ln := range strings.Split(string(data), "\n") {
		if len(ln) > longest {
			longest = len(ln)
		}
	}
	if longest <= 512*1024 {
		t.Fatalf("test premise broken: longest line %d bytes, want > 512 KB", longest)
	}

	r2, err := Load(path)
	if err != nil {
		t.Fatalf("Load on oversized-line registry returned error: %v", err)
	}
	if len(r2.Notes) != 2 {
		t.Fatalf("expected 2 notes round-tripped, got %d", len(r2.Notes))
	}
	if r2.Notes[0].Message != big {
		t.Errorf("oversized message not round-tripped intact (got %d bytes, want %d)",
			len(r2.Notes[0].Message), len(big))
	}
	if r2.Notes[1].Message != "a normal note after the big one" {
		t.Errorf("note after the oversized one was lost/garbled: %q", r2.Notes[1].Message)
	}
}

func TestRegistryLoad_NoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.yaml")
	// Deliberately no trailing newline on the last line.
	content := "notes:\n" +
		"  - id: aa\n" +
		"    timestamp: 2026-05-18T10:00:00-06:00\n" +
		"    author: agent-b\n" +
		"    session: s\n" +
		"    message: last line has no trailing newline"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Notes) != 1 || r.Notes[0].Message != "last line has no trailing newline" {
		t.Fatalf("final newline-less line not parsed: %+v", r.Notes)
	}
}

func TestValidateNoteMessage(t *testing.T) {
	if err := ValidateNoteMessage("a short note"); err != nil {
		t.Errorf("short message rejected: %v", err)
	}
	if err := ValidateNoteMessage(strings.Repeat("x", MaxNoteBytes)); err != nil {
		t.Errorf("message exactly at cap rejected: %v", err)
	}
	if err := ValidateNoteMessage(strings.Repeat("x", MaxNoteBytes+1)); err == nil {
		t.Error("message over cap was not rejected")
	}
}
