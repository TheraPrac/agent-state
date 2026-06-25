package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// I-1587: `st close` removes the item's local test-log mirror directory so
// `st test` logs do not accumulate after an item is done.
func TestCloseRemovesTestLogs(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	Create(s, cfg, "task", "Has test logs", CreateOpts{Priority: 2})
	Start(s, cfg, "T-001", StartOpts{})

	logDir := filepath.Join(cfg.Root(), ".as", "test-logs", "T-001")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "api_unit-20260101T000000.log"), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := Close(s, cfg, "T-001", "done", CloseOpts{}); rc != 0 {
		t.Fatalf("Close rc=%d, want 0", rc)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("test-log dir still present after close (stat err=%v)", err)
	}
}

// I-1305: `st close` ergonomics — near-miss resolutions are corrected and
// confirmed, and every parse-error path prints the full corrected
// invocation (usage + concrete example) so a confused caller reaches a
// successful close in one step.

// TestCorrectResolution unit-tests the prefix/case correction helper in
// isolation so the table is independent of the full close side effects.
func TestCorrectResolution(t *testing.T) {
	terminal := []string{"done", "abandoned", "archived"}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"abandon", "abandoned", true}, // unique prefix
		{"archive", "archived", true},  // unique prefix
		{"don", "done", true},          // unique prefix
		{"Done", "done", true},         // case-only mismatch
		{"ABANDONED", "abandoned", true},
		{"done", "done", false},     // exact — no correction
		{"abandoned", "", false},    // exact — no correction (want ignored)
		{"a", "", false},            // ambiguous: abandoned|archived
		{"superseded", "", false},   // not a resolution at all
		{"superseded x", "", false}, // free text
		{"", "", false},             // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := correctResolution(tc.in, terminal)
			if ok != tc.ok {
				t.Fatalf("correctResolution(%q) ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("correctResolution(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCloseUsageShowsExample verifies the usage helper embeds the live
// terminal vocabulary and a concrete copy-pasteable example.
func TestCloseUsageShowsExample(t *testing.T) {
	got := closeUsage("T-457", []string{"done", "abandoned", "archived"})
	for _, want := range []string{
		"usage: st close <id> <done|abandoned|archived> [--reason <text>]",
		"st close T-457 abandoned --reason superseded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("closeUsage missing %q; got:\n%s", want, got)
		}
	}
	// With no id the placeholder is shown rather than an empty token.
	if !strings.Contains(closeUsage("", []string{"done"}), "st close <id> abandoned") {
		t.Errorf("closeUsage(\"\") should fall back to <id> placeholder")
	}
}

// TestClosePrefixResolutionAccepted is the headline AC: `st close <id>
// abandon` is corrected to `abandoned` and the close completes, with a
// confirmation note on stderr.
func TestClosePrefixResolutionAccepted(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	var rc int
	stderr := captureStderr(t, func() int {
		rc = Close(s, cfg, "T-001", "abandon", CloseOpts{Reason: "superseded"})
		return rc
	})
	if rc != 0 {
		t.Fatalf("Close rc=%d, want 0", rc)
	}
	if !strings.Contains(stderr, `interpreting "abandon" as "abandoned"`) {
		t.Errorf("expected interpreting note, got stderr: %s", stderr)
	}
	item, _ := s.Get("T-001")
	if item.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned", item.Status)
	}
}

// TestCloseCaseCorrectionAccepted verifies a case-only mismatch is
// corrected (Archived → archived) and accepted.
func TestCloseCaseCorrectionAccepted(t *testing.T) {
	root := setupTestEnvRoot(t)
	cfg, _ := config.Load(root)
	s, _ := store.New(cfg)
	os.MkdirAll(cfg.ChangelogDir(), 0755)
	Create(s, cfg, "task", "Will archive", CreateOpts{Priority: 2})
	Start(s, cfg, "T-001", StartOpts{})

	var rc int
	stderr := captureStderr(t, func() int {
		rc = Close(s, cfg, "T-001", "Archived", CloseOpts{})
		return rc
	})
	if rc != 0 {
		t.Fatalf("Close rc=%d, want 0; stderr: %s", rc, stderr)
	}
	if !strings.Contains(stderr, `interpreting "Archived" as "archived"`) {
		t.Errorf("expected case-correction note, got stderr: %s", stderr)
	}
}

// TestCloseAmbiguousResolutionShowsUsage verifies an ambiguous prefix is
// rejected with the full corrected-usage example, not just an enum list.
func TestCloseAmbiguousResolutionShowsUsage(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	var rc int
	stderr := captureStderr(t, func() int {
		rc = Close(s, cfg, "T-001", "a", CloseOpts{})
		return rc
	})
	if rc == 0 {
		t.Fatal("ambiguous resolution should be rejected")
	}
	if !strings.Contains(stderr, "st close T-001 abandoned --reason superseded") {
		t.Errorf("expected usage example in stderr, got: %s", stderr)
	}
	// Item must not have been mutated.
	item, _ := s.Get("T-001")
	if item.Status == "abandoned" || item.Status == "archived" || item.Status == "done" {
		t.Errorf("item closed despite rejected resolution: status=%q", item.Status)
	}
}

// TestCloseFreeTextResolutionHintsReasonFlag verifies free text in the
// resolution slot yields the usage example and points at --reason.
func TestCloseFreeTextResolutionHintsReasonFlag(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	var rc int
	stderr := captureStderr(t, func() int {
		rc = Close(s, cfg, "T-001", "superseded reason", CloseOpts{})
		return rc
	})
	if rc == 0 {
		t.Fatal("free-text resolution should be rejected")
	}
	for _, want := range []string{
		"the reason for closing goes in --reason",
		"st close T-001 abandoned --reason superseded",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected stderr to contain %q, got: %s", want, stderr)
		}
	}
}

// TestCloseMissingResolutionWithReasonHints verifies the forgotten-
// resolution case (`st close <id> --reason "x"`, which reaches Close with
// resolution="") names the missing token, points at --reason vs the slot,
// and prints the example.
func TestCloseMissingResolutionWithReasonHints(t *testing.T) {
	s, cfg := setupAbandonEnv(t)
	var rc int
	stderr := captureStderr(t, func() int {
		rc = Close(s, cfg, "T-001", "", CloseOpts{Reason: "superseded: replaced by T-999"})
		return rc
	})
	if rc == 0 {
		t.Fatal("missing resolution should be rejected")
	}
	for _, want := range []string{
		"missing resolution",
		"the reason for closing goes in --reason",
		"st close T-001 abandoned --reason superseded",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("expected stderr to contain %q, got: %s", want, stderr)
		}
	}
}
