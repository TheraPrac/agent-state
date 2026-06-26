package validate

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

func testConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {
			{Type: "deps_resolved"},
			{Type: "field_nonempty", Fields: []string{"title"}},
			{Type: "stage_reached", Stage: "merged"},
		},
	}
	cfg.Delivery = &config.DeliveryConfig{
		Stages: []string{"coding", "committed", "pushed", "pr_open", "merged", "deployed_dev", "smoke_passed"},
	}
	return cfg
}

func testItem(id, status string) *model.Item {
	return &model.Item{
		ID:       id,
		Type:     "task",
		Status:   status,
		Title:    "Test item",
		Delivery: map[string]interface{}{"stage": "merged"},
		Manifest: map[string]interface{}{},
		Doc:      &model.ParsedDocument{Lines: []model.Line{{Raw: "title: Test item", Key: "title", Value: "Test item"}}},
	}
}

func TestGatesAllPass(t *testing.T) {
	cfg := testConfig()
	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		failure := FirstFailure(results)
		t.Errorf("all gates should pass, but %q failed: %s", failure.Gate, failure.Message)
	}
}

func TestGatesNoConfig(t *testing.T) {
	cfg := config.Defaults() // no gates configured
	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("no gates configured should pass")
	}
}

func TestGateDepsResolvedPass(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "deps_resolved"}},
	}

	dep := testItem("T-002", "done")
	item := testItem("T-001", "active")
	item.DependsOn = []string{"T-002"}
	allItems := map[string]*model.Item{"T-001": item, "T-002": dep}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("deps_resolved should pass when dep is completed")
	}
}

func TestGateDepsResolvedFail(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "deps_resolved"}},
	}

	dep := testItem("T-002", "active") // not terminal
	item := testItem("T-001", "active")
	item.DependsOn = []string{"T-002"}
	allItems := map[string]*model.Item{"T-001": item, "T-002": dep}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("deps_resolved should fail when dep is active")
	}
}

func TestGateDepsResolvedMissing(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "deps_resolved"}},
	}

	item := testItem("T-001", "active")
	item.DependsOn = []string{"T-999"} // doesn't exist
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("deps_resolved should fail when dep is missing")
	}
}

func TestGateFieldNonemptyPass(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "field_nonempty", Fields: []string{"title"}}},
	}

	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("field_nonempty should pass when title is set")
	}
}

func TestGateFieldNonemptyFail(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "field_nonempty", Fields: []string{"summary"}}},
	}

	item := testItem("T-001", "active")
	// summary field doesn't exist in doc
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("field_nonempty should fail when summary is empty")
	}
}

func TestGateFieldNonemptyNull(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "field_nonempty", Fields: []string{"completed"}}},
	}

	item := testItem("T-001", "active")
	item.Doc.Lines = append(item.Doc.Lines, model.Line{Raw: "completed: null", Key: "completed", Value: "null"})
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("field_nonempty should treat null as empty")
	}
}

func TestGateStageReachedPass(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "stage_reached", Stage: "merged"}},
	}

	item := testItem("T-001", "active")
	item.Delivery["stage"] = "deployed_dev" // past merged
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("stage_reached should pass when at deployed_dev (past merged)")
	}
}

func TestGateStageReachedFail(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "stage_reached", Stage: "merged"}},
	}

	item := testItem("T-001", "active")
	item.Delivery["stage"] = "pr_open" // before merged
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("stage_reached should fail when at pr_open (before merged)")
	}
}

func TestGateStageReachedNoStage(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "stage_reached", Stage: "merged"}},
	}

	item := testItem("T-001", "active")
	item.Delivery = map[string]interface{}{} // no stage
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("stage_reached should fail when no stage set")
	}
}

func TestGateTestingCompleteNoConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = nil
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("testing_complete should pass when no testing config")
	}
}

func TestGateTestingCompleteAllPassing(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
			"api_lint": {Command: "make lint"},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_unit":        "pass abc1234 2026-03-26T10:00:00-06:00",
		"api_lint":        "pass abc1234 2026-03-26T10:00:00-06:00",
		"api_integration": "pass abc1234 2026-03-26T10:00:00-06:00",
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		f := FirstFailure(results)
		t.Errorf("testing_complete should pass, got: %s", f.Message)
	}
}

func TestGateTestingCompleteMissingRequired(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {},
			"api_lint": {},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_unit": "pass abc1234 2026-03-26T10:00:00-06:00",
		// api_lint missing
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("testing_complete should fail when required suite missing")
	}
}

func TestGateTestingCompleteScopeRequired(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_integration": "required", // triggered by st pr but not recorded
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("testing_complete should fail when scope suite required but not recorded")
	}
}

func TestGateTestingCompleteScopeNotTriggered(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"api_integration": {},
			"web_e2e":         {},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		// Neither scope suite triggered — should pass
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("testing_complete should pass when scope suites not triggered")
	}
}

// I-776: when an item declares a scope_class, testing_complete iterates that
// class's required suites instead of cfg.Testing.RequiredSuites. A
// workspace-config item with only `workspace_test: pass …` recorded must
// pass — no api/web evidence required.
func TestTestingComplete_ScopeClassUsesClassRequiredSuites(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
			"api_lint": {Command: "make lint"},
		},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {Command: "bash claude-config/hooks/run-changed-hook-tests.sh"},
				},
			},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("I-776", "active")
	item.ScopeClass = "workspace-config"
	item.TestingEvidence = map[string]interface{}{
		"workspace_test": "pass abc1234 2026-05-23T07:00:00-06:00",
		// No api_unit / api_lint — that's the whole point.
	}
	allItems := map[string]*model.Item{"I-776": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		f := FirstFailure(results)
		t.Errorf("workspace-config item with workspace_test should pass; got %s: %s", f.Gate, f.Message)
	}
}

// I-776: if an item declares a scope_class but the class's required suite
// is missing evidence, the gate fails with the standard "required suite
// not recorded" message — same shape as the default-class failure, so the
// recovery instruction (`st test … workspace_test`) is self-explanatory.
func TestTestingComplete_ScopeClassMissingClassSuite(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {Command: "bash run.sh"},
				},
			},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("I-776", "active")
	item.ScopeClass = "workspace-config"
	// No workspace_test evidence.
	allItems := map[string]*model.Item{"I-776": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("expected testing_complete to fail when class required suite missing")
	}
	f := FirstFailure(results)
	if f.Gate != "testing_complete" {
		t.Errorf("failing gate = %q, want testing_complete", f.Gate)
	}
	// Message must name the missing suite so the operator/agent knows what to run.
	if !strings.Contains(f.Message, "workspace_test") {
		t.Errorf("failure message should name workspace_test, got: %s", f.Message)
	}
}

// I-776: an item declaring an unknown scope_class fails fast with a
// targeted message. Silent fallback to the default class would re-impose
// api/web suites that the agent explicitly opted out of — worse than a
// loud failure that says "fix your config or your item".
func TestTestingComplete_UnknownScopeClass(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
		},
		ScopeClasses: map[string]config.ScopeClassConfig{
			// "workspace-config" defined elsewhere; this item names a bogus class.
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("I-776", "active")
	item.ScopeClass = "bogus-class"
	item.TestingEvidence = map[string]interface{}{
		"api_unit": "pass abc1234 2026-05-23T07:00:00-06:00",
	}
	allItems := map[string]*model.Item{"I-776": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("expected testing_complete to fail for unknown scope_class")
	}
	f := FirstFailure(results)
	if !strings.Contains(f.Message, "unknown scope_class") || !strings.Contains(f.Message, "bogus-class") {
		t.Errorf("failure should name the unknown class explicitly, got: %s", f.Message)
	}
}

// I-776: items WITHOUT a scope_class continue to use the default
// RequiredSuites — this is the regression guard for the 99% case.
func TestTestingComplete_NoScopeClassUsesGlobalRequired(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
		},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {Command: "x"},
				},
			},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-099", "active")
	// No ScopeClass — should require api_unit, not workspace_test.
	item.TestingEvidence = map[string]interface{}{
		"workspace_test": "pass x 2026-05-23T07:00:00-06:00",
	}
	allItems := map[string]*model.Item{"T-099": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("expected testing_complete to fail — default class requires api_unit which is unrecorded")
	}
	f := FirstFailure(results)
	if !strings.Contains(f.Message, "api_unit") {
		t.Errorf("failure should name api_unit (the default-class required suite), got: %s", f.Message)
	}
}

// I-776: declared-but-empty class is a config error, not a free pass.
func TestTestingComplete_ScopeClassEmptyClassFailsLoud(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {RequiredSuites: map[string]config.SuiteConfig{}},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{"close": {{Type: "testing_complete"}}}

	item := testItem("I-776", "active")
	item.ScopeClass = "workspace-config"
	allItems := map[string]*model.Item{"I-776": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("empty class.RequiredSuites should fail the gate, not silently pass")
	}
	f := FirstFailure(results)
	if !strings.Contains(f.Message, "no required suites declared") {
		t.Errorf("failure should name the empty-class problem, got: %s", f.Message)
	}
}

// I-776: scope-class items skip the ScopeSuites loop entirely — the class
// IS the complete required-set definition. A scope suite incidentally marked
// 'required' (e.g., by st pr) on a class item must NOT block the gate.
func TestTestingComplete_ScopeClassSkipsScopeSuites(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {Command: "x"},
				},
			},
		},
		ScopeSuites: map[string]config.ScopeSuiteConfig{
			"web_e2e": {Command: "y"},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{"close": {{Type: "testing_complete"}}}

	item := testItem("I-776", "active")
	item.ScopeClass = "workspace-config"
	item.TestingEvidence = map[string]interface{}{
		"workspace_test": "pass abc1234 2026-05-23T08:00:00-06:00",
		"web_e2e":        "required", // would normally fail the gate
	}
	allItems := map[string]*model.Item{"I-776": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		f := FirstFailure(results)
		t.Errorf("class items should bypass ScopeSuites loop; gate failed: %s", f.Message)
	}
}

// I-776: when the default class has multiple missing required suites,
// the failure message must name them deterministically (sorted) — Go's
// map iteration order is randomized per process and would otherwise
// produce flaky UX and flaky test assertions.
func TestTestingComplete_DeterministicMissingSuiteMessage(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"zeta_suite":  {Command: "z"},
			"alpha_suite": {Command: "a"},
			"middle":      {Command: "m"},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{"close": {{Type: "testing_complete"}}}

	item := testItem("T-001", "active")
	// No evidence — all three suites are missing.
	allItems := map[string]*model.Item{"T-001": item}

	// Run the gate multiple times; the same suite name must surface every
	// time (the sorted-first one — "alpha_suite").
	const N = 25
	for i := 0; i < N; i++ {
		results := EvaluateGates(item, "close", cfg, allItems)
		f := FirstFailure(results)
		if !strings.Contains(f.Message, "alpha_suite") {
			t.Fatalf("iteration %d: expected alpha_suite (first sorted) in message, got: %s", i, f.Message)
		}
	}
}

func TestGateAgentAssignedPass(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "agent_assigned"}},
	}

	t.Setenv("AS_AGENT_ID", "agent-a")
	item := testItem("T-001", "active")
	item.AssignedTo = "agent-a"
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("agent_assigned should pass when agent matches")
	}
}

func TestGateAgentAssignedFail(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "agent_assigned"}},
	}

	t.Setenv("AS_AGENT_ID", "agent-b")
	item := testItem("T-001", "active")
	item.AssignedTo = "agent-a"
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("agent_assigned should fail when agent doesn't match")
	}
}

func TestGateAgentAssignedNoAgent(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "agent_assigned"}},
	}

	t.Setenv("AS_AGENT_ID", "")
	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("agent_assigned should pass when no agent identity set")
	}
}

func TestGateManifestExistsPass(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "manifest_exists"}},
	}

	item := testItem("T-001", "active")
	item.Manifest["prs"] = "https://github.com/org/repo/pull/42"
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("manifest_exists should pass when PRs recorded")
	}
}

func TestGateManifestExistsFail(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "manifest_exists"}},
	}

	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("manifest_exists should fail when no PRs")
	}
}

func TestGateUnknownType(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "nonexistent_gate"}},
	}

	item := testItem("T-001", "active")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("unknown gate should fail")
	}
}

func TestGatesStopAtFirstFailure(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {
			{Type: "stage_reached", Stage: "merged"},
			{Type: "manifest_exists"}, // should not be reached
		},
	}

	item := testItem("T-001", "active")
	item.Delivery["stage"] = "coding" // fails stage_reached
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if len(results) != 1 {
		t.Errorf("should stop at first failure, got %d results", len(results))
	}
	if results[0].Gate != "stage_reached" {
		t.Errorf("first failure should be stage_reached, got %s", results[0].Gate)
	}
}

func TestFirstFailureNil(t *testing.T) {
	results := []GateResult{{Passed: true}, {Passed: true}}
	if FirstFailure(results) != nil {
		t.Error("FirstFailure should return nil when all pass")
	}
}

func TestGateFieldNonemptyNoDoc(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "field_nonempty", Fields: []string{"title"}}},
	}

	item := testItem("T-001", "active")
	item.Doc = nil
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("field_nonempty should fail when doc is nil")
	}
}

func TestGateStageReachedDefaultStages(t *testing.T) {
	cfg := testConfig()
	cfg.Delivery = nil // use default stages
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "stage_reached", Stage: "merged"}},
	}

	item := testItem("T-001", "active")
	item.Delivery["stage"] = "merged"
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("stage_reached should pass with default stages")
	}
}

// T-438: auto-skip written by st test --auto is accepted by the gate for
// required suites — it signals "not applicable" (no files changed in that
// repo), not a user bypass.
func TestTestingComplete_AutoSkipPassesGate(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit":      {Command: "make test-unit"},
			"api_lint":      {Command: "make lint"},
			"web_typecheck": {Command: "make type-check"},
			"web_unit":      {Command: "make test-unit"},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	// Only web changed — api suites auto-skipped, web suites passed.
	item := testItem("T-438", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_unit":      "auto-skip: no files changed in theraprac-api",
		"api_lint":      "auto-skip: no files changed in theraprac-api",
		"web_typecheck": "pass abc1234 2026-05-30T10:00:00-06:00",
		"web_unit":      "pass abc1234 2026-05-30T10:00:00-06:00",
	}
	allItems := map[string]*model.Item{"T-438": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		f := FirstFailure(results)
		t.Errorf("auto-skip on required suite should pass gate, got: %s", f.Message)
	}
}

// A plain "skip:" (user-initiated) on a required suite must still fail the gate.
// auto-skip is the only system-accepted form; users cannot bypass via --skip.
func TestTestingComplete_UserSkipOnRequiredFails(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_unit": "skip: operator skipped manually",
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("user skip on required suite must not pass testing_complete gate")
	}
}

// All repos touched — no auto-skips, all suites must pass.
func TestTestingComplete_AllReposTouchedNoAutoSkip(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {Command: "make test-unit"},
			"web_unit": {Command: "make test-unit"},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.TestingEvidence = map[string]interface{}{
		"api_unit": "auto-skip: no files changed in theraprac-api",
		// web_unit missing — should fail
	}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("gate should fail when web_unit is missing (auto-skip only covers api_unit)")
	}
}

// I-831: when a default-class item has goal tags matching a scope class and a
// required suite is missing, the gate failure message includes a hint.
func TestGateHintsScopeClassForGoalTaggedItem(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {},
		},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {},
				},
				AppliesToGoals: []string{"st-tooling"},
			},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.Tags = []string{"goal:st-tooling"}
	// No evidence recorded — gate should fail with hint.
	item.TestingEvidence = map[string]interface{}{}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("gate should fail when required suite has no evidence")
	}

	var failMsg string
	for _, r := range results {
		if !r.Passed {
			failMsg = r.Message
			break
		}
	}
	if !strings.Contains(failMsg, "workspace-config") {
		t.Errorf("failure message should mention workspace-config, got: %s", failMsg)
	}
	if !strings.Contains(failMsg, "st update") {
		t.Errorf("failure message should mention st update, got: %s", failMsg)
	}
	// Original st-test recovery instruction must still be present (augment, not replace).
	if !strings.Contains(failMsg, "st test") {
		t.Errorf("failure message should still mention st test, got: %s", failMsg)
	}
}

// I-831: a default-class item without goal tags gets the undecorated failure.
func TestGateNoHintForUntaggedMissingSuite(t *testing.T) {
	cfg := testConfig()
	cfg.Testing = &config.TestingConfig{
		RequiredSuites: map[string]config.SuiteConfig{
			"api_unit": {},
		},
		ScopeClasses: map[string]config.ScopeClassConfig{
			"workspace-config": {
				RequiredSuites: map[string]config.SuiteConfig{
					"workspace_test": {},
				},
				AppliesToGoals: []string{"st-tooling"},
			},
		},
	}
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "testing_complete"}},
	}

	item := testItem("T-001", "active")
	item.Tags = []string{"unrelated-tag"}
	item.TestingEvidence = map[string]interface{}{}
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Fatal("gate should fail when required suite has no evidence")
	}

	for _, r := range results {
		if !r.Passed && strings.Contains(r.Message, "scope_class") && strings.Contains(r.Message, "goal tags suggest") {
			t.Errorf("unexpected scope_class hint in message for untagged item: %s", r.Message)
		}
	}
}

// --- TestGateReviewEvidencePassed ---

func testItemWithReviewEvidence(ev string) *model.Item {
	item := testItem("T-001", "active")
	if ev != "" {
		item.Doc.SetField("review_evidence", ev)
	}
	return item
}

func TestGateReviewEvidencePassedMissing(t *testing.T) {
	// No review_evidence field → soft-pass (legacy items without review).
	item := testItemWithReviewEvidence("")
	result := evalReviewEvidencePassed(item)
	if !result.Passed {
		t.Errorf("missing review_evidence: want pass (soft enforcement), got fail: %s", result.Message)
	}
}

func TestGateReviewEvidencePassedPass(t *testing.T) {
	item := testItemWithReviewEvidence("pass abc1234 2026-06-14T10:00:00-06:00 evidence:s3://bucket/key.gz")
	result := evalReviewEvidencePassed(item)
	if !result.Passed {
		t.Errorf("pass verdict: want pass, got fail: %s", result.Message)
	}
}

func TestGateReviewEvidencePassedFail(t *testing.T) {
	item := testItemWithReviewEvidence("fail abc1234 2026-06-14T10:00:00-06:00 evidence:")
	result := evalReviewEvidencePassed(item)
	if result.Passed {
		t.Errorf("fail verdict: want gate fail, got pass")
	}
	if !strings.Contains(result.Message, "re-run") {
		t.Errorf("fail message should include re-run hint: %s", result.Message)
	}
}

func TestGateReviewEvidencePassedViaEvaluateGates(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "review_evidence_passed"}},
	}

	item := testItemWithReviewEvidence("pass abc1234 2026-06-14T10:00:00-06:00 evidence:")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if !GatesPassed(results) {
		t.Error("review_evidence_passed gate: should pass with 'pass ...' evidence")
	}
}

func TestGateReviewEvidencePassedFailsViaEvaluateGates(t *testing.T) {
	cfg := testConfig()
	cfg.Gates = map[string][]config.GateConfig{
		"close": {{Type: "review_evidence_passed"}},
	}

	item := testItemWithReviewEvidence("fail abc1234 2026-06-14T10:00:00-06:00 evidence:")
	allItems := map[string]*model.Item{"T-001": item}

	results := EvaluateGates(item, "close", cfg, allItems)
	if GatesPassed(results) {
		t.Error("review_evidence_passed gate: should fail with 'fail ...' evidence")
	}
}
