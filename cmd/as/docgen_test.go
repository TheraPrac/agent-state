package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDocgenRender exercises the in-process renderer end-to-end on the
// real cobra tree from newApp. Locking in the AC-required substrings
// here means a missing group annotation or a regressed walker fails
// `go test` before the regenerated doc ever lands in the workspace.
func TestDocgenRender(t *testing.T) {
	app := newApp("")
	var buf bytes.Buffer
	if err := renderDocs(&buf, app); err != nil {
		t.Fatalf("renderDocs failed: %v", err)
	}
	got := buf.String()
	if got == "" {
		t.Fatal("renderDocs produced empty output")
	}

	for _, want := range []string{
		// Auto-generated marker (AC 6).
		"Auto-generated",
		// Preamble survival (AC 8).
		"## Workspace clone layout",
		// Command coverage (AC 7).
		"st show",
		"st queue",
		"st arc",
		"st artifact",
		"st watch",
		"st tui",
		"st recommend",
		"st status",
		// Group section headers (AC 9 — added by plan reviewer).
		"### Queue & Stack",
		"### State Management",
		"### Workflow",
		"### Querying",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated doc missing %q", want)
		}
	}

	// The hidden docgen command should not appear in its own output.
	if strings.Contains(got, "st docgen") {
		t.Error("hidden `st docgen` command leaked into generated doc")
	}
}

// TestDocgenDeterministic runs the renderer twice on a fresh app and
// asserts byte-equal output. Without this, a map iteration in the
// walker would silently churn the committed doc every regeneration.
func TestDocgenDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := renderDocs(&a, newApp("")); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if err := renderDocs(&b, newApp("")); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if a.String() != b.String() {
		t.Error("renderDocs is non-deterministic; output differs between runs")
	}
}
