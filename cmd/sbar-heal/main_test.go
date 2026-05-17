package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/parse"
)

// corruptFixture mirrors the real post-edit T-196 shape: all four sbar
// sub-fields are clean single-line scalars (so the parser recovers a
// fully-populated item.SBAR), followed by I-487 col-0 dedented prose,
// a garbage line with a stray colon (spurious Key), and duplicate
// orphaned sub-field headers — then a genuine top-level key.
const corruptFixture = `id: T-999
type: task
status: queued
title: heal fixture
created: 2026-05-16T00:00:00-06:00
last_touched: 2026-05-16T00:00:00-06:00
sbar:
  situation: clean situation text
  background: clean background text
  assessment: clean assessment text
  recommendation: clean recommendation text
PROBLEM
some dedented narrative line
T-182 path: PostClientCharge spurious garbage
  assessment: |-
    stale orphan assessment body
  recommendation: |-
    stale orphan recommendation body
blocks:
- []
`

const cleanFixture = `id: T-998
type: task
status: queued
title: clean fixture
created: 2026-05-16T00:00:00-06:00
last_touched: 2026-05-16T00:00:00-06:00
sbar:
  situation: |-
    a clean situation
  background: |-
    a clean background
  assessment: |-
    a clean assessment
  recommendation: |-
    a clean recommendation

blocks:
- []
`

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestHealFile_DryRunDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	p := writeFixture(t, dir, "T-999.md", corruptFixture)

	r, err := healFile(p, false /* apply */)
	if err != nil {
		t.Fatalf("healFile: %v", err)
	}
	if r.Action != "healed" {
		t.Fatalf("action = %q, want healed", r.Action)
	}
	if r.Signature == "" {
		t.Error("expected a non-empty corruption signature")
	}
	got, _ := os.ReadFile(p)
	if string(got) != corruptFixture {
		t.Error("dry-run must not modify the file on disk")
	}
}

func TestHealFile_ApplyHealsAndPreservesContentAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := writeFixture(t, dir, "T-999.md", corruptFixture)

	// Capture the parsed SBAR before healing.
	before, err := parse.File(p)
	if err != nil {
		t.Fatalf("parse before: %v", err)
	}

	r, err := healFile(p, true /* apply */)
	if err != nil {
		t.Fatalf("healFile apply: %v", err)
	}
	if r.Action != "healed" {
		t.Fatalf("action = %q, want healed", r.Action)
	}

	healed, _ := os.ReadFile(p)
	hs := string(healed)

	// Structural assertions: exactly one of each sub-field header, no
	// col-0 garbage, the genuine following key intact.
	for _, k := range []string{"  situation:", "  background:", "  assessment:", "  recommendation:"} {
		if n := strings.Count(hs, "\n"+k) + boolToInt(strings.HasPrefix(hs, k)); n != 1 {
			t.Errorf("sub-field %q appears %d times, want 1\n%s", k, n, hs)
		}
	}
	for _, garbage := range []string{"PROBLEM", "some dedented narrative", "T-182 path:"} {
		if strings.Contains(hs, garbage) {
			t.Errorf("garbage %q survived heal:\n%s", garbage, hs)
		}
	}
	if !strings.Contains(hs, "\nblocks:\n- []") {
		t.Errorf("genuine following key not preserved:\n%s", hs)
	}

	// Content preservation: parsed SBAR identical before vs after.
	after, err := parse.File(p)
	if err != nil {
		t.Fatalf("parse after: %v", err)
	}
	if before.SBAR != after.SBAR {
		t.Errorf("SBAR content changed by heal.\nbefore: %+v\nafter:  %+v", before.SBAR, after.SBAR)
	}
	if after.SBAR.Situation != "clean situation text" ||
		after.SBAR.Recommendation != "clean recommendation text" {
		t.Errorf("unexpected healed SBAR: %+v", after.SBAR)
	}

	// Idempotency: a second apply is a no-op.
	r2, err := healFile(p, true)
	if err != nil {
		t.Fatalf("second healFile: %v", err)
	}
	if r2.Action != "skipped_clean" {
		t.Errorf("second run action = %q, want skipped_clean (not idempotent)", r2.Action)
	}
	healed2, _ := os.ReadFile(p)
	if string(healed2) != hs {
		t.Errorf("second apply changed bytes (not idempotent)")
	}
}

func TestHealFile_CleanFileUntouched(t *testing.T) {
	dir := t.TempDir()
	p := writeFixture(t, dir, "T-998.md", cleanFixture)

	r, err := healFile(p, true)
	if err != nil {
		t.Fatalf("healFile: %v", err)
	}
	if r.Action != "skipped_clean" {
		t.Errorf("clean file action = %q, want skipped_clean", r.Action)
	}
	got, _ := os.ReadFile(p)
	if string(got) != cleanFixture {
		t.Errorf("clean file was modified:\n%s", string(got))
	}
}

func TestItemIDFromFilename(t *testing.T) {
	cases := map[string]string{
		"T-196-claim-assembly-from-posted-charge.md":       "T-196",
		"I-577-wire-prod-stedi-api-key-via-aws-secrets.md": "I-577",
		"T-203-claims-submission-e2e-tests.md":             "T-203",
	}
	for in, want := range cases {
		if got := itemIDFromFilename(in); got != want {
			t.Errorf("itemIDFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
