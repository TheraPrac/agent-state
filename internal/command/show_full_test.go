package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowFull_EverySectionAndCollapsePolicy(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		if rc := Show(s, cfg, "T-002", ShowOpts{Full: true}); rc != 0 {
			t.Fatalf("rc != 0")
		}
	})

	// Title banner.
	if !strings.Contains(out, "T-002 — ") {
		t.Errorf("missing title banner\n%s", out)
	}
	// Every facet appears as a section header (▼ expanded or ▶ collapsed).
	for _, k := range facetOrder {
		if !strings.Contains(out, "▼ "+k+"  (") && !strings.Contains(out, "▶ "+k+"  (") {
			t.Errorf("missing section header for %q\n%s", k, out)
		}
	}
	// Default collapse policy: human sections expanded, machine collapsed.
	for _, k := range []string{"item", "plan", "ac"} {
		if !strings.Contains(out, "▼ "+k+"  (") {
			t.Errorf("human section %q must be expanded by default\n%s", k, out)
		}
	}
	for _, k := range []string{"history", "commits", "accounting", "deps"} {
		if !strings.Contains(out, "▶ "+k+"  (") {
			t.Errorf("machine section %q must be collapsed by default\n%s", k, out)
		}
	}
}

func TestShowFull_AllExpandsEverything(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Show(s, cfg, "T-002", ShowOpts{Full: true, FullAll: true})
	})
	if strings.Contains(out, "▶ ") {
		t.Errorf("--all must expand every section (no ▶ collapsed glyphs)\n%s", out)
	}
	for _, k := range facetOrder {
		if !strings.Contains(out, "▼ "+k+"  (") {
			t.Errorf("--all: section %q must be expanded\n%s", k, out)
		}
	}
}

func TestShowFull_Deterministic(t *testing.T) {
	s, cfg := setupTestEnv(t)
	run := func() string {
		return captureStdout(t, func() { Show(s, cfg, "T-002", ShowOpts{Full: true}) })
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("composite view must be deterministic\nA:\n%s\nB:\n%s", a, b)
	}
}

func TestShowFull_SelfDocumentingHeaders(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// I-716: setupTestEnv seeds a passable sidecar; this test
	// exercises the empty-plan/empty-AC self-documenting headers,
	// so remove the fixture sidecar first.
	if err := os.Remove(filepath.Join(cfg.PlansDir(), "T-001.md")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing fixture sidecar: %v", err)
	}
	out := captureStdout(t, func() {
		Show(s, cfg, "T-001", ShowOpts{Full: true})
	})
	// T-001 has no plan / no AC in the fixture — the header itself must
	// say so (the §5 "header is the at-a-glance" property).
	if !strings.Contains(out, "▼ plan  (none)") {
		t.Errorf("plan header must self-document the empty state\n%s", out)
	}
	if !strings.Contains(out, "▼ ac  (none)") {
		t.Errorf("ac header must self-document the empty state\n%s", out)
	}
}

func TestShowFull_NotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := Show(s, cfg, "NOPE-999", ShowOpts{Full: true}); rc != 1 {
		t.Errorf("not-found with --full must rc=1, got %d", rc)
	}
}
