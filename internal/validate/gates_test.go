package validate

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
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
