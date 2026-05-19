package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// Default (PLANNING) view = g.Ready(): T-001 (queued, unblocked,
// unassigned) is recommendable; it blocks the still-open T-002, so its
// rationale must NAME that leverage. T-002 (blocked by T-001), T-003
// (active+assigned) and T-004 (done) are correctly excluded.
func TestRecommend_TextPlanningView(t *testing.T) {
	s, cfg := setupTestEnv(t)

	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{}) })
	if rc != 0 {
		t.Fatalf("rc = %d\n%s", rc, out)
	}

	if !strings.Contains(out, "T-001") {
		t.Fatalf("T-001 must be recommendable\n%s", out)
	}
	if !strings.Contains(out, "unblocks 1 (T-002)") {
		t.Errorf("T-001 rationale must name the unblocked item\n%s", out)
	}
	if !strings.Contains(out, "priority p2") || !strings.Contains(out, "why:") {
		t.Errorf("rationale must be decomposed + labelled\n%s", out)
	}
	if strings.Contains(out, "T-002") && !strings.Contains(out, "unblocks 1 (T-002)") {
		t.Errorf("blocked T-002 must not itself be a candidate\n%s", out)
	}
	// Priority dominance: if the p1 issue is present it must precede the p2 task.
	if i, j := strings.Index(out, "I-001"), strings.Index(out, "T-001"); i >= 0 && i > j {
		t.Errorf("p1 I-001 must outrank p2 T-001\n%s", out)
	}
}

func TestRecommend_TopLimit(t *testing.T) {
	s, cfg := setupTestEnv(t)
	out := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Top: 1})
	})
	// Exactly one item row (each row prints one "why:" line).
	if n := strings.Count(out, "why:"); n != 1 {
		t.Fatalf("--top 1 must print exactly one item, got %d\n%s", n, out)
	}
}

func TestRecommend_JSONStableContract(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{JSON: true}) })
	if rc != 0 {
		t.Fatalf("json rc != 0\n%s", out)
	}

	var got []recommendJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) == 0 {
		t.Fatalf("expected ≥1 recommendation\n%s", out)
	}
	var t001 *recommendJSON
	for i := range got {
		if got[i].ID == "T-001" {
			t001 = &got[i]
		}
	}
	if t001 == nil {
		t.Fatalf("T-001 missing from JSON\n%s", out)
	}
	if t001.Priority != 2 {
		t.Errorf("T-001 priority = %d, want 2", t001.Priority)
	}
	if !strings.Contains(t001.Rationale, "unblocks 1 (T-002)") {
		t.Errorf("T-001 JSON rationale missing leverage: %q", t001.Rationale)
	}
	var hasUnblock bool
	for _, f := range t001.Factors {
		if f.Name == "unblock" {
			hasUnblock = true
		}
	}
	if !hasUnblock {
		t.Errorf("T-001 factors must include the unblock factor: %+v", t001.Factors)
	}
}

// DISPATCH view: empty queue ⇒ nothing; after a user-approved add the
// eligible item appears (mirrors selectNext's candidate set exactly).
func TestRecommend_QueueDispatchView(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("AS_AGENT_ID", "") // user-added ⇒ approved (I-490)

	empty := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Queue: true})
	})
	if !strings.Contains(empty, "No recommendable items") {
		t.Fatalf("empty queue must yield none, got:\n%s", empty)
	}

	QueueAdd(s, cfg, "T-001", QueueOpts{})
	out := captureStdout(t, func() {
		Recommend(s, cfg, RecommendOpts{Queue: true})
	})
	if !strings.Contains(out, "T-001") || !strings.Contains(out, "why:") {
		t.Fatalf("approved+eligible T-001 must appear in dispatch view:\n%s", out)
	}
}

// --scope sprint with no registry ⇒ resilient empty set, not an error.
func TestRecommend_SprintScopeNoRegistry(t *testing.T) {
	s, cfg := setupTestEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = Recommend(s, cfg, RecommendOpts{Scope: "sprint"}) })
	if rc != 0 {
		t.Fatalf("must not error without a registry, rc!=0\n%s", out)
	}
	if !strings.Contains(out, "No recommendable items") {
		t.Fatalf("no active sprint ⇒ no candidates, got:\n%s", out)
	}
}
