package validate

import (
	"fmt"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// GateResult represents the outcome of evaluating a gate.
type GateResult struct {
	Passed  bool
	Gate    string // gate type
	Message string // human-readable reason for failure
}

// EvaluateGates checks all gates for a transition. Returns results for each gate.
// Stops at the first failure (gives the agent one clear action).
func EvaluateGates(item *model.Item, transition string, cfg *config.Config, allItems map[string]*model.Item) []GateResult {
	gates := cfg.Gates[transition]
	if len(gates) == 0 {
		return nil
	}

	var results []GateResult
	for _, gate := range gates {
		result := evaluateGate(item, gate, cfg, allItems)
		results = append(results, result)
		if !result.Passed {
			break // first failure stops evaluation
		}
	}
	return results
}

// GatesPassed returns true if all gates passed (or no gates configured).
func GatesPassed(results []GateResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return true
}

// FirstFailure returns the first failed gate, or nil if all passed.
func FirstFailure(results []GateResult) *GateResult {
	for _, r := range results {
		if !r.Passed {
			return &r
		}
	}
	return nil
}

func evaluateGate(item *model.Item, gate config.GateConfig, cfg *config.Config, allItems map[string]*model.Item) GateResult {
	switch gate.Type {
	case "deps_resolved":
		return evalDepsResolved(item, cfg, allItems)
	case "field_nonempty":
		return evalFieldNonempty(item, gate)
	case "stage_reached":
		return evalStageReached(item, gate, cfg)
	case "testing_complete":
		return evalTestingComplete(item, cfg)
	case "agent_assigned":
		return evalAgentAssigned(item, cfg)
	case "manifest_exists":
		return evalManifestExists(item)
	default:
		return GateResult{Passed: false, Gate: gate.Type, Message: fmt.Sprintf("unknown gate type: %s", gate.Type)}
	}
}

func evalDepsResolved(item *model.Item, cfg *config.Config, allItems map[string]*model.Item) GateResult {
	for _, depID := range item.DependsOn {
		dep, ok := allItems[depID]
		if !ok {
			return GateResult{Passed: false, Gate: "deps_resolved",
				Message: fmt.Sprintf("dependency %s not found", depID)}
		}
		tc, ok := cfg.Types[dep.Type]
		if !ok {
			continue
		}
		isTerminal := false
		for _, ts := range tc.TerminalStatuses {
			if dep.Status == ts {
				isTerminal = true
				break
			}
		}
		if !isTerminal {
			return GateResult{Passed: false, Gate: "deps_resolved",
				Message: fmt.Sprintf("dependency %s is %s (not terminal)", depID, dep.Status)}
		}
	}
	return GateResult{Passed: true, Gate: "deps_resolved"}
}

func evalFieldNonempty(item *model.Item, gate config.GateConfig) GateResult {
	for _, field := range gate.Fields {
		val := ""
		if item.Doc != nil {
			val, _ = item.Doc.GetField(field)
		}
		if val == "" || val == "null" || val == "~" {
			return GateResult{Passed: false, Gate: "field_nonempty",
				Message: fmt.Sprintf("field %q is empty — set it before closing", field)}
		}
	}
	return GateResult{Passed: true, Gate: "field_nonempty"}
}

func evalStageReached(item *model.Item, gate config.GateConfig, cfg *config.Config) GateResult {
	currentStage := ""
	if s, ok := item.Delivery["stage"]; ok {
		if str, ok := s.(string); ok {
			currentStage = str
		}
	}

	if currentStage == "" {
		return GateResult{Passed: false, Gate: "stage_reached",
			Message: fmt.Sprintf("no delivery stage set — must reach %s", gate.Stage)}
	}

	// Check stage ordering from config or fallback default
	stages := defaultStages()
	if cfg.Delivery != nil && len(cfg.Delivery.Stages) > 0 {
		stages = cfg.Delivery.Stages
	}

	currentIdx := indexOf(stages, currentStage)
	targetIdx := indexOf(stages, gate.Stage)

	if currentIdx < 0 {
		return GateResult{Passed: false, Gate: "stage_reached",
			Message: fmt.Sprintf("unknown stage %q", currentStage)}
	}
	if targetIdx < 0 {
		return GateResult{Passed: false, Gate: "stage_reached",
			Message: fmt.Sprintf("unknown target stage %q", gate.Stage)}
	}

	if currentIdx < targetIdx {
		return GateResult{Passed: false, Gate: "stage_reached",
			Message: fmt.Sprintf("at stage %s, must reach %s", currentStage, gate.Stage)}
	}

	return GateResult{Passed: true, Gate: "stage_reached"}
}

func evalTestingComplete(item *model.Item, cfg *config.Config) GateResult {
	if cfg.Testing == nil {
		return GateResult{Passed: true, Gate: "testing_complete"}
	}

	// I-776: pick which required-suite set applies to this item. When the
	// item declares scope_class, that class's required suites are the
	// canonical set for the testing-complete gate (workspace-config items
	// get workspace_test instead of api/web Tier 1). Unknown scope_class
	// fails fast — silent fallback to the default class would silently
	// re-impose api/web requirements on an item that explicitly opted out.
	requiredSuites := cfg.Testing.RequiredSuites
	if item.ScopeClass != "" {
		class, ok := cfg.Testing.ScopeClasses[item.ScopeClass]
		if !ok {
			return GateResult{Passed: false, Gate: "testing_complete",
				Message: fmt.Sprintf("unknown scope_class %q — declare in config.testing.scope_classes or remove from item", item.ScopeClass)}
		}
		requiredSuites = class.RequiredSuites
	}

	// Check required suites: every configured required suite must have a "pass" record
	for name := range requiredSuites {
		val := getTestingEvidence(item, name)
		if val == "" || val == "null" {
			return GateResult{Passed: false, Gate: "testing_complete",
				Message: fmt.Sprintf("required suite %q not recorded — run `st test %s %s`", name, item.ID, name)}
		}
		if !strings.HasPrefix(val, "pass") {
			return GateResult{Passed: false, Gate: "testing_complete",
				Message: fmt.Sprintf("required suite %q did not pass: %s", name, val)}
		}
	}

	// Check scope suites: only those marked "required" (by st pr) must pass
	for name := range cfg.Testing.ScopeSuites {
		val := getTestingEvidence(item, name)
		if val == "required" {
			return GateResult{Passed: false, Gate: "testing_complete",
				Message: fmt.Sprintf("scope suite %q required but not recorded — run `st test %s %s`", name, item.ID, name)}
		}
	}

	return GateResult{Passed: true, Gate: "testing_complete"}
}

// getTestingEvidence reads a suite value from the item's TestingEvidence map.
// The parser stores these flat: TestingEvidence["api_unit"] not TestingEvidence["required_suites"]["api_unit"].
func getTestingEvidence(item *model.Item, suite string) string {
	if item.TestingEvidence == nil {
		return ""
	}
	v, ok := item.TestingEvidence[suite]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func evalAgentAssigned(item *model.Item, cfg *config.Config) GateResult {
	agentID := cfg.AgentID()
	if agentID == "" {
		return GateResult{Passed: true, Gate: "agent_assigned"} // no agent identity = skip check
	}
	if item.AssignedTo != agentID {
		return GateResult{Passed: false, Gate: "agent_assigned",
			Message: fmt.Sprintf("assigned to %s, not you (%s)", item.AssignedTo, agentID)}
	}
	return GateResult{Passed: true, Gate: "agent_assigned"}
}

func evalManifestExists(item *model.Item) GateResult {
	// Check if manifest has any PR entries
	if prs, ok := item.Manifest["prs"]; ok {
		if str, ok := prs.(string); ok && str != "" && str != "[]" {
			return GateResult{Passed: true, Gate: "manifest_exists"}
		}
	}
	return GateResult{Passed: false, Gate: "manifest_exists",
		Message: "no PR manifest recorded — use `as pr` to record one"}
}

func defaultStages() []string {
	return []string{
		"coding", "committed", "pushed", "pr_open", "reviewed",
		"merged", "deployed_dev", "smoke_passed", "closed",
	}
}

func indexOf(slice []string, val string) int {
	for i, v := range slice {
		if v == val {
			return i
		}
	}
	return -1
}
