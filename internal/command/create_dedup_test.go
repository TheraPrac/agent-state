package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeDedupEngine returns a RunEngine that always returns the given matchID
// from the dedup LLM call. Pass "" to simulate a "no match" response.
func makeDedupEngine(matchID string) RunEngine {
	data, _ := json.Marshal(dedupVerdict{MatchID: matchID, Reason: "test"})
	return RunEngine{
		RunClaude: func(cwd string, args []string, env []string) ([]byte, int, error) {
			return data, 0, nil
		},
	}
}

// TestExtractKeywords covers the tokeniser used for pre-filtering.
func TestExtractKeywords(t *testing.T) {
	tests := []struct {
		input    string
		contains []string
		absent   []string
	}{
		{
			input:    "ReDoS in @isaacs/brace-expansion severity=high via openapi-generator-cli",
			contains: []string{"@isaacs/brace-expansion", "severity=high", "openapi-generator-cli"},
			absent:   []string{"in"}, // too short
		},
		{
			input:    "path-to-regexp@8.3.0 ReDoS reaches theraprac-web via dev-only openapi-generator-cli",
			contains: []string{"path-to-regexp@8.3.0", "reaches", "theraprac-web", "openapi-generator-cli"},
		},
		{
			input:    "foo bar baz", // all short words
			contains: []string{},
		},
	}
	for _, tt := range tests {
		kws := extractKeywords(tt.input)
		kwSet := make(map[string]bool)
		for _, k := range kws {
			kwSet[k] = true
		}
		for _, want := range tt.contains {
			if !kwSet[want] {
				t.Errorf("extractKeywords(%q): want %q in result %v", tt.input, want, kws)
			}
		}
		for _, absent := range tt.absent {
			if kwSet[absent] {
				t.Errorf("extractKeywords(%q): %q should be absent, got %v", tt.input, absent, kws)
			}
		}
	}
}

// TestDedupNoMatchCreatesNewItem: when the LLM returns no match,
// Create proceeds normally and a new item is created.
func TestDedupNoMatchCreatesNewItem(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Engine that says "no match"
	engine := makeDedupEngine("")

	code := Create(s, cfg, "issue", "Something entirely new", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		Situation:      "POST /patients returns 500 for new practice sign-ups when RLS is active.",
		Background:     "Tenant ID not propagated to DB connection in internal/db/pool.go SetContext().",
		Assessment:     "RLS policy rejects queries running as app user with no tenant context.",
		Recommendation: "Add SetContext call in internal/db/pool.go:42.",
		Engine:         engine,
		NoValidate:     true, // skip SBAR validation, focus on dedup
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	_, ok := s.Get("I-002")
	if !ok {
		t.Error("item should have been created when no dedup match")
	}
}

// TestDedupMatchMergesIntoExistingItem: when the LLM returns the ID of an
// existing open item, no new item is created and an observation is recorded.
func TestDedupMatchMergesIntoExistingItem(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Pre-create the "canonical" item that the dedup will match against.
	codeFirst := Create(s, cfg, "issue", "ReDoS in @isaacs/brace-expansion devDep", CreateOpts{
		Priority:       1,
		EnforceGate:    false, // bypass all validation for setup
		Situation:      "brace-expansion@5.0.0 flagged by push gate for T-210.",
		Background:     "Transitive devDep via openapi-generator-cli.",
		Assessment:     "Build-time only, no prod exposure.",
		Recommendation: "npm audit fix",
	})
	if codeFirst != 0 {
		t.Fatalf("setup: first create failed with code %d", codeFirst)
	}
	canonical, ok := s.Get("I-002")
	if !ok {
		t.Fatal("setup: canonical item I-002 not found")
	}
	if len(canonical.Observations) != 0 {
		t.Fatalf("setup: expected 0 observations, got %d", len(canonical.Observations))
	}

	// Now attempt to create a semantically-duplicate item. The engine will
	// return I-002 as the match.
	engine := makeDedupEngine("I-002")
	codeDedup := Create(s, cfg, "issue", "@isaacs/brace-expansion@5.0.0 ReDoS blocks T-213 push", CreateOpts{
		Priority:       1,
		EnforceGate:    true,
		Situation:      "security-scan-on-push.sh flagged @isaacs/brace-expansion@5.0.0 HIGH advisory during T-213 push gate on theraprac-web.",
		Background:     "Same transitive devDep chain: openapi-generator-cli → glob → minimatch → @isaacs/brace-expansion@5.0.0. Build-time only, no prod exposure.",
		Assessment:     "Same root cause as I-002 — ReDoS in brace-expansion@5.0.0, build-time devDep with no untrusted input path.",
		Recommendation: "Run npm audit fix in theraprac-web to bump the transitive @isaacs/brace-expansion to a patched version.",
		Engine:         engine,
		NoValidate:     true,
	})
	if codeDedup != 0 {
		t.Fatalf("expected exit 0 after dedup merge, got %d", codeDedup)
	}

	// No new item should have been created — I-003 should not exist.
	_, exists := s.Get("I-003")
	if exists {
		t.Error("a new item was created despite dedup match — should have merged")
	}

	// Reload I-002 and verify an observation was appended.
	updated, ok := s.Get("I-002")
	if !ok {
		t.Fatal("I-002 disappeared after dedup merge")
	}
	if len(updated.Observations) != 1 {
		t.Errorf("expected 1 observation on I-002, got %d: %v", len(updated.Observations), updated.Observations)
	}
	obs := updated.Observations[0]
	if !strings.Contains(obs, "T-213") && !strings.Contains(obs, "brace-expansion") && !strings.Contains(obs, "|") {
		t.Errorf("observation entry looks wrong: %q", obs)
	}
}

// TestDedupNoDedupFlagBypasses: --no-dedup creates a new item even with a
// match-returning engine.
func TestDedupNoDedupFlagBypasses(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Pre-create canonical.
	Create(s, cfg, "issue", "Existing issue", CreateOpts{ //nolint:errcheck
		Priority: 2, EnforceGate: false,
		Situation: "something exists already",
	})

	engine := makeDedupEngine("I-002") // LLM would say "match"
	code := Create(s, cfg, "issue", "New duplicate with NoDedup", CreateOpts{
		Priority:       2,
		EnforceGate:    true,
		Situation:      "POST /patients returns 500 for new practice sign-ups when RLS policy is enforced by the app DB user.",
		Background:     "Tenant ID not propagated to DB connection in internal/db/pool.go SetContext(); pool.go line 42 missing ctx.Value(tenantKey) call.",
		Assessment:     "RLS policy rejects queries running as app user with no tenant context — app/rls_policy.sql line 18.",
		Recommendation: "Add SetContext(ctx, tenantID) call in internal/db/pool.go:42 before the querier invocation.",
		Engine:         engine,
		NoValidate:     true,
		NoDedup:        true, // explicitly bypass dedup
	})
	if code != 0 {
		t.Fatalf("expected exit 0 with --no-dedup, got %d", code)
	}
	_, ok := s.Get("I-003")
	if !ok {
		t.Error("item should have been created when --no-dedup is set")
	}
}

// TestDedupPriorityBump: recording observations bumps priority appropriately.
func TestDedupPriorityBump(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	// Create at p2.
	Create(s, cfg, "issue", "Priority bump target", CreateOpts{ //nolint:errcheck
		Priority: 2, EnforceGate: false,
		Situation: "medium priority issue",
	})

	// recordObservation should bump to p1 on first hit.
	if err := recordObservation(s, cfg, "I-002", "new situation for the same problem"); err != nil {
		t.Fatalf("recordObservation failed: %v", err)
	}
	item, _ := s.Get("I-002")
	if item.Priority == nil || *item.Priority != 1 {
		p := -1
		if item.Priority != nil {
			p = *item.Priority
		}
		t.Errorf("expected priority 1 after first observation, got %d", p)
	}
	if len(item.Observations) != 1 {
		t.Errorf("expected 1 observation, got %d", len(item.Observations))
	}

	// Third observation should bump to p0 (two more to reach 3 total).
	recordObservation(s, cfg, "I-002", "second hit") //nolint:errcheck
	recordObservation(s, cfg, "I-002", "third hit")  //nolint:errcheck
	item, _ = s.Get("I-002")
	if item.Priority == nil || *item.Priority != 0 {
		p := -1
		if item.Priority != nil {
			p = *item.Priority
		}
		t.Errorf("expected priority 0 after third observation, got %d", p)
	}
}
