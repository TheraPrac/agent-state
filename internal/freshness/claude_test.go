package freshness

import (
	"errors"
	"testing"
)

// TestParseFreshnessRecommendation_FreshLeadingToken — verdict
// emitted as "Fresh — false positive" maps to VerdictFresh.
func TestParseFreshnessRecommendation_FreshLeadingToken(t *testing.T) {
	v, _, ok := parseFreshnessRecommendation("RECOMMENDATION: Fresh — keyword overlap incidental")
	if !ok || v != VerdictFresh {
		t.Errorf("expected (Fresh, ok=true); got (%s, %v)", v, ok)
	}
}

// TestParseFreshnessRecommendation_StaleLeadingTokenWithFreshInRationale
// — "Stale — keyword fresh was incidental" must NOT match "fresh"
// inside the rationale. Leading-token logic anchored on the verdict.
func TestParseFreshnessRecommendation_StaleLeadingTokenWithFreshInRationale(t *testing.T) {
	v, _, ok := parseFreshnessRecommendation("RECOMMENDATION: Stale — keyword fresh was incidental")
	if !ok || v != VerdictStale {
		t.Errorf("leading-token parsing failed; got (%s, %v)", v, ok)
	}
}

// TestParseFreshnessRecommendation_NoHeaderShape — narrative-only
// output with no header returns ok=false → caller keeps the
// heuristic verdict.
func TestParseFreshnessRecommendation_NoHeaderShape(t *testing.T) {
	_, _, ok := parseFreshnessRecommendation("Plan looks good, my recommendation is to keep moving.")
	if ok {
		t.Errorf("expected ok=false on narrative-only output; parser found a verdict")
	}
}

// TestParseFreshnessRecommendation_UnknownToken — verdict words
// outside {fresh, drift, stale} return ok=false.
func TestParseFreshnessRecommendation_UnknownToken(t *testing.T) {
	_, _, ok := parseFreshnessRecommendation("RECOMMENDATION: Defer — circle back next week")
	if ok {
		t.Errorf("expected ok=false on unknown verdict 'Defer'; got match")
	}
}

// TestRunFreshnessClaudePass_SkipsWhenRunClaudeNil — nil engine
// keeps the heuristic verdict; no Claude call attempted.
func TestRunFreshnessClaudePass_SkipsWhenRunClaudeNil(t *testing.T) {
	v, rationale := runFreshnessClaudePass(nil, "/ws", VerdictDrift, "prompt")
	if v != VerdictDrift || rationale != "" {
		t.Errorf("expected (Drift, ''); got (%s, %q)", v, rationale)
	}
}

// TestRunFreshnessClaudePass_DemotesDriftToFresh — the model
// promotes a heuristic Drift to Fresh; runFreshnessClaudePass
// returns the new verdict + rationale.
func TestRunFreshnessClaudePass_DemotesDriftToFresh(t *testing.T) {
	runClaude := func(cwd string, args []string, env []string) ([]byte, int, error) {
		return []byte("RECOMMENDATION: Fresh — keyword overlap was incidental, file is still correct"), 0, nil
	}
	v, rationale := runFreshnessClaudePass(runClaude, "/ws", VerdictDrift, "prompt")
	if v != VerdictFresh {
		t.Errorf("expected demotion to Fresh; got %s", v)
	}
	if rationale == "" {
		t.Error("expected non-empty rationale on successful adjudication")
	}
}

// TestRunFreshnessClaudePass_PromotesDriftToStale — the model
// promotes Drift to Stale (e.g., file exists but its API shifted).
func TestRunFreshnessClaudePass_PromotesDriftToStale(t *testing.T) {
	runClaude := func(cwd string, args []string, env []string) ([]byte, int, error) {
		return []byte("RECOMMENDATION: Stale — public API of the touched function changed materially"), 0, nil
	}
	v, _ := runFreshnessClaudePass(runClaude, "/ws", VerdictDrift, "prompt")
	if v != VerdictStale {
		t.Errorf("expected promotion to Stale; got %s", v)
	}
}

// TestRunFreshnessClaudePass_EngineErrorKeepsHeuristicVerdict —
// claude exec error falls back to the heuristic verdict (fail
// closed: no silent waiver, no escalation).
func TestRunFreshnessClaudePass_EngineErrorKeepsHeuristicVerdict(t *testing.T) {
	runClaude := func(cwd string, args []string, env []string) ([]byte, int, error) {
		return nil, 1, errors.New("simulated claude exec error")
	}
	v, rationale := runFreshnessClaudePass(runClaude, "/ws", VerdictDrift, "prompt")
	if v != VerdictDrift {
		t.Errorf("engine-error path should keep heuristic verdict; got %s", v)
	}
	if rationale != "" {
		t.Errorf("expected empty rationale on engine error; got %q", rationale)
	}
}

// TestRunFreshnessClaudePass_UnparseableResponseKeepsHeuristicVerdict
// — non-zero exit, empty output, or no recognizable RECOMMENDATION
// line all fall back to the heuristic verdict.
func TestRunFreshnessClaudePass_UnparseableResponseKeepsHeuristicVerdict(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
		exit   int
	}{
		{"empty", []byte(""), 0},
		{"nonzero", []byte("RECOMMENDATION: Fresh — ok"), 1},
		{"no_header", []byte("looks fine to me"), 0},
		{"unknown_token", []byte("RECOMMENDATION: Maybe — not sure"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runClaude := func(cwd string, args []string, env []string) ([]byte, int, error) {
				return c.stdout, c.exit, nil
			}
			v, rationale := runFreshnessClaudePass(runClaude, "/ws", VerdictDrift, "prompt")
			if v != VerdictDrift {
				t.Errorf("expected fallback to Drift; got %s", v)
			}
			if rationale != "" {
				t.Errorf("expected empty rationale; got %q", rationale)
			}
		})
	}
}
