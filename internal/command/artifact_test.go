package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/store"
)

// Every declared kind dispatches, returns rc 0, and prints non-empty
// text — a missing facet must degrade to a clear "(no …)" line, never a
// crash or a silent blank (operator silent-failure principle).
func TestArtifact_EveryKindTextDispatches(t *testing.T) {
	s, cfg := setupTestEnv(t)
	for _, k := range facetOrder {
		var rc int
		out := captureStdout(t, func() {
			rc = Artifact(s, cfg, "T-002", ArtifactOpts{Kind: k})
		})
		if rc != 0 {
			t.Errorf("kind %q rc=%d, want 0\n%s", k, rc, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("kind %q produced empty output", k)
		}
	}
}

func TestArtifact_KnownItemFacetContent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "item"})
	})
	if !strings.Contains(out, "T-002") {
		t.Fatalf("item facet must name the item\n%s", out)
	}
	// T-002 depends_on T-001 — the deps facet must reflect that edge.
	dout := captureStdout(t, func() {
		Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "deps"})
	})
	if !strings.Contains(dout, "T-001") {
		t.Errorf("deps facet must show the T-002→T-001 edge\n%s", dout)
	}
}

func TestArtifact_MissingFacetDegradesCleanly(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// I-716: setupTestEnv seeds a passable sidecar; this test
	// explicitly exercises the "no plan / no AC" facet degrade
	// path, so remove the sidecar first.
	if err := os.Remove(filepath.Join(cfg.PlansDir(), "T-001.md")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing fixture sidecar: %v", err)
	}
	// T-001 fixture has no plan / no AC — must say so, rc 0, no panic.
	for _, k := range []string{"plan", "ac", "history"} {
		var rc int
		out := captureStdout(t, func() {
			rc = Artifact(s, cfg, "T-001", ArtifactOpts{Kind: k})
		})
		if rc != 0 || !strings.Contains(out, "(no ") {
			t.Errorf("missing %q must degrade to a clear empty (rc=%d): %q", k, rc, out)
		}
	}
}

func TestArtifact_AllJSONStableAndValid(t *testing.T) {
	s, cfg := setupTestEnv(t)
	run := func() string {
		return captureStdout(t, func() {
			if rc := Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "all", Format: "json"}); rc != 0 {
				t.Fatalf("all/json rc != 0")
			}
		})
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("all --format json must be deterministic across runs\nA:%s\nB:%s", a, b)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(a), &obj); err != nil {
		t.Fatalf("all/json is not a valid JSON object: %v\n%s", err, a)
	}
	for _, k := range facetOrder {
		if _, ok := obj[k]; !ok {
			t.Errorf("all/json missing facet key %q", k)
		}
	}
}

func TestArtifact_AllTextHasEverySection(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "all"})
	})
	for _, k := range facetOrder {
		if !strings.Contains(out, "━━━ "+k+" ━━━") {
			t.Errorf("all text missing section header for %q\n%s", k, out)
		}
	}
}

func TestArtifact_JSONFacetValid(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "item", Format: "json"})
	})
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("item/json invalid: %v\n%s", err, out)
	}
	if v["id"] != "T-002" {
		t.Errorf("item/json id = %v, want T-002", v["id"])
	}
}

// Regression (T-371 live-verify): a parsed field can be an empty scalar
// (work_tracking.pr flattened to ""); the summary helper must never yield
// blank, or the composite header reads a meaningless "(—)".
func TestNonEmptyOr(t *testing.T) {
	cases := []struct {
		v    any
		fb   string
		want string
	}{
		{"as#130", "manifest", "as#130"},
		{"", "manifest", "manifest"},
		{"  ", "manifest", "manifest"},
		{nil, "fallback", "fallback"}, // %v of nil = "<nil>" → treated blank → fallback
		{[]any{"as#1", "as#1"}, "manifest", "[as#1 as#1]"},
	}
	for _, c := range cases {
		if got := nonEmptyOr(c.v, c.fb); got != c.want {
			t.Errorf("nonEmptyOr(%#v,%q) = %q, want %q", c.v, c.fb, got, c.want)
		}
	}
}

func TestArtifact_ErrorPaths(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "nonsense"}); rc != 2 {
		t.Errorf("unknown kind must rc=2, got %d", rc)
	}
	if rc := Artifact(s, cfg, "NOPE-999", ArtifactOpts{Kind: "item"}); rc != 1 {
		t.Errorf("not-found item must rc=1, got %d", rc)
	}
	if rc := Artifact(s, cfg, "T-002", ArtifactOpts{Kind: "item", Format: "xml"}); rc != 2 {
		t.Errorf("bad --format must rc=2, got %d", rc)
	}
}

// T-437: observations facet — empty and non-empty item variants.
func TestFacetObservations(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// Empty: item with no observations → summary "none", non-empty text.
	out := captureStdout(t, func() {
		Artifact(s, cfg, "T-001", ArtifactOpts{Kind: "observations"})
	})
	if !strings.Contains(out, "none") && !strings.Contains(out, "(no ") {
		t.Errorf("empty observations should say none: %s", out)
	}

	// Non-empty: write an item with observations and confirm they render.
	writeFile(t, filepath.Join(cfg.Root(), "tasks", "T-999-obs.md"), `id: T-999
type: task
status: active
created: 2026-05-30T10:00:00-06:00
last_touched: 2026-05-30T10:00:00-06:00
title: Observations test item
observations:
- 2026-05-30T10:01:00-06:00 | st create I-999-duplicate | duplicate situation observed
- 2026-05-30T10:02:00-06:00 | st create I-999-another | second re-discovery
sbar:
  situation: test item with observations
  background: used for facet test
  assessment: straightforward
  recommendation: verify facet renders observations
`)
	// Re-load the store to pick up the new file.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	out = captureStdout(t, func() {
		Artifact(s2, cfg, "T-999", ArtifactOpts{Kind: "observations"})
	})
	if !strings.Contains(out, "re-discover") {
		t.Errorf("non-empty observations should mention re-discover: %s", out)
	}
	if !strings.Contains(out, "duplicate situation observed") {
		t.Errorf("observations text should contain first entry: %s", out)
	}

	// JSON shape: must be a []string slice.
	outJSON := captureStdout(t, func() {
		Artifact(s2, cfg, "T-999", ArtifactOpts{Kind: "observations", Format: "json"})
	})
	var obs []string
	if err := json.Unmarshal([]byte(outJSON), &obs); err != nil {
		t.Errorf("observations JSON must be []string, got error: %v\n%s", err, outJSON)
	}
	if len(obs) != 2 {
		t.Errorf("expected 2 observations, got %d: %v", len(obs), obs)
	}
}
