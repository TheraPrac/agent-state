package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// I-670: `st update <id> sbar [--stdin|<value>]` previously fell through
// to SetField and flattened the 4-section mapping into a `sbar: |-`
// string scalar, with no autonomous recovery (dotted-path could not
// un-stringify a scalar). These tests pin the fix: the stdin and
// positional-value paths parse the 4-section buffer and write via the
// heal-capable SetSBARBlock (which also REPAIRS an already-corrupted
// scalar), missing sections reject without writing a scalar, and a
// dotted write onto a corrupted scalar fails loudly with a recovery
// pointer instead of silently vanishing.

const fourSectionBuf = "situation: |-\n" +
	"  Sit body line one.\n" +
	"  Sit body line two.\n" +
	"background: |-\n" +
	"  Bg body.\n" +
	"assessment: |-\n" +
	"  Assess body.\n" +
	"recommendation: |-\n" +
	"  Rec body.\n"

// pipeStdin swaps os.Stdin for a pipe preloaded with content and returns
// a restore func, mirroring the established patch_test.go pattern.
func pipeStdin(t *testing.T, content string) func() {
	t.Helper()
	old := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	w.WriteString(content)
	w.Close()
	return func() { os.Stdin = old }
}

// writeCorruptScalarItem drops an issue whose `sbar` is the exact
// post-bug shape: a `sbar: |-` block scalar with the would-be mapping
// flattened into 2-space-indented prose body lines. Returns a freshly
// opened store that sees it.
func writeCorruptScalarItem(t *testing.T, s *store.Store, cfg *config.Config) *store.Store {
	t.Helper()
	p, ok := s.Path("I-001")
	if !ok {
		t.Fatal("I-001 fixture missing")
	}
	issuesDir := filepath.Dir(p)
	writeFile(t, filepath.Join(issuesDir, "I-900-corrupt.md"), `id: I-900
type: issue
status: queued
created: 2026-05-18T10:00:00-06:00
last_touched: 2026-05-18T10:00:00-06:00

title: Corrupted-scalar sbar fixture
priority: 2

sbar: |-
  situation: |-
  this entire blob was pasted as one scalar by the pre-fix bug
  background: |-
  none of these are real mapping keys anymore
last_touched_by: agent-b
`)
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-opening store: %v", err)
	}
	return s2
}

func TestUpdateSBAR_StdinParsesFourSectionMapping(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	restore := pipeStdin(t, fourSectionBuf)
	defer restore()

	if code := Update(s, cfg, "I-001", "sbar", "", UpdateModeStdin); code != 0 {
		t.Fatalf("Update sbar --stdin returned %d, want 0", code)
	}

	// File must be a proper mapping, not a `sbar: |-` scalar.
	path, _ := s.Path("I-001")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "sbar: |-") {
		t.Fatalf("sbar was scalar-wrapped, body:\n%s", string(body))
	}

	s2, _ := store.New(cfg)
	item, ok := s2.Get("I-001")
	if !ok {
		t.Fatal("I-001 not found after re-parse")
	}
	if item.Doc.SBARIsScalarCorrupted() {
		t.Error("sbar reported corrupted after a structured stdin write")
	}
	if !strings.Contains(item.SBAR.Situation, "Sit body line one.") ||
		!strings.Contains(item.SBAR.Situation, "Sit body line two.") {
		t.Errorf("situation = %q, want both body lines", item.SBAR.Situation)
	}
	if !strings.Contains(item.SBAR.Background, "Bg body.") {
		t.Errorf("background = %q", item.SBAR.Background)
	}
	if !strings.Contains(item.SBAR.Assessment, "Assess body.") {
		t.Errorf("assessment = %q", item.SBAR.Assessment)
	}
	if !strings.Contains(item.SBAR.Recommendation, "Rec body.") {
		t.Errorf("recommendation = %q", item.SBAR.Recommendation)
	}
}

func TestUpdateSBAR_PositionalValueParsesFourSection(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	if code := Update(s, cfg, "I-001", "sbar", fourSectionBuf, UpdateModeValue); code != 0 {
		t.Fatalf("Update sbar <value> returned %d, want 0", code)
	}
	path, _ := s.Path("I-001")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "sbar: |-") {
		t.Fatalf("sbar was scalar-wrapped via positional value, body:\n%s", string(body))
	}
	s2, _ := store.New(cfg)
	item, _ := s2.Get("I-001")
	if item.Doc.SBARIsScalarCorrupted() {
		t.Error("sbar reported corrupted after a structured positional write")
	}
	if !strings.Contains(item.SBAR.Recommendation, "Rec body.") {
		t.Errorf("recommendation = %q", item.SBAR.Recommendation)
	}
}

func TestUpdateSBAR_MissingSectionRejectedNoScalarWritten(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	path, _ := s.Path("I-001")
	before, _ := os.ReadFile(path)

	restore := pipeStdin(t, "situation: |-\n  only this\nbackground: |-\n  and this\n")
	defer restore()

	var code int
	stderr := captureStderr(t, func() int {
		code = Update(s, cfg, "I-001", "sbar", "", UpdateModeStdin)
		return code
	})

	if code != 2 {
		t.Fatalf("missing-section sbar stdin returned %d, want 2", code)
	}
	if !strings.Contains(stderr, "missing required section") {
		t.Errorf("stderr lacked actionable message: %q", stderr)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("item file mutated on rejected sbar write.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestUpdateSBAR_RecoversCorruptedScalar(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	s = writeCorruptScalarItem(t, s, cfg)

	pre, ok := s.Get("I-900")
	if !ok {
		t.Fatal("I-900 corrupt fixture missing")
	}
	if !pre.Doc.SBARIsScalarCorrupted() {
		t.Fatal("fixture is not detected as a corrupted scalar — test premise broken")
	}

	restore := pipeStdin(t, fourSectionBuf)
	defer restore()

	if code := Update(s, cfg, "I-900", "sbar", "", UpdateModeStdin); code != 0 {
		t.Fatalf("recovery Update returned %d, want 0", code)
	}

	s2, _ := store.New(cfg)
	item, _ := s2.Get("I-900")
	if item.Doc.SBARIsScalarCorrupted() {
		t.Error("sbar still corrupted after recovery write")
	}
	if !strings.Contains(item.SBAR.Situation, "Sit body line one.") ||
		!strings.Contains(item.SBAR.Background, "Bg body.") ||
		!strings.Contains(item.SBAR.Assessment, "Assess body.") ||
		!strings.Contains(item.SBAR.Recommendation, "Rec body.") {
		t.Errorf("recovered SBAR incomplete: %+v", item.SBAR)
	}
}

func TestUpdateSBAR_DottedOnCorruptedScalarRefusesLoudly(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)
	s = writeCorruptScalarItem(t, s, cfg)

	path, _ := s.Path("I-900")
	before, _ := os.ReadFile(path)

	var code int
	stderr := captureStderr(t, func() int {
		code = Update(s, cfg, "I-900", "sbar.situation", "sneak this in", UpdateModeValue)
		return code
	})

	if code != 2 {
		t.Fatalf("dotted write onto corrupted scalar returned %d, want 2", code)
	}
	if !strings.Contains(stderr, "corrupted scalar") || !strings.Contains(stderr, "st update I-900 sbar --stdin") {
		t.Errorf("stderr lacked corruption diagnosis + recovery pointer: %q", stderr)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("item file mutated on refused dotted write.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestUpdateSBAR_DottedOnCleanMappingStillWorks(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	if code := Update(s, cfg, "I-001", "sbar.situation", "regression text", UpdateModeValue); code != 0 {
		t.Fatalf("dotted write on clean mapping returned %d, want 0", code)
	}
	s2, _ := store.New(cfg)
	item, _ := s2.Get("I-001")
	if item.Doc.SBARIsScalarCorrupted() {
		t.Error("clean mapping reported corrupted after a valid dotted write")
	}
	if item.SBAR.Situation != "regression text" {
		t.Errorf("situation = %q, want %q", item.SBAR.Situation, "regression text")
	}
	// Sibling sections must survive the targeted edit.
	if !strings.Contains(item.SBAR.Recommendation, "Keep priority and queued status stable.") {
		t.Errorf("recommendation clobbered by dotted situation write: %q", item.SBAR.Recommendation)
	}
}
