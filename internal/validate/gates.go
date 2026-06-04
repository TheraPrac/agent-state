package validate

import (
	"fmt"
	"sort"
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

	// I-776: route through the central helper so every required-suite reader
	// (gate, st test, st uat, st run auto-runner, queue advisor, canonical
	// emit) agrees on what counts as required for this item.
	requiredSuites, classOK := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	if !classOK {
		// Unknown scope_class: fail fast. Silent fallback would re-impose the
		// default class's suites on an item that explicitly opted out, which
		// defeats the point of declaring scope_class.
		return GateResult{Passed: false, Gate: "testing_complete",
			Message: fmt.Sprintf("unknown scope_class %q — declare in config.testing.scope_classes or remove from item", item.ScopeClass)}
	}
	// I-776: empty class is a config error, not a free pass. A class declared
	// in config with zero required suites would trivially pass the gate with
	// no evidence recorded — exactly the silent-bypass anti-pattern I-776 is
	// meant to retire. The default class (no scope_class declared) is allowed
	// to be empty for back-compat with configs that have no required_suites.
	if item.ScopeClass != "" && len(requiredSuites) == 0 {
		return GateResult{Passed: false, Gate: "testing_complete",
			Message: fmt.Sprintf("scope_class %q has no required suites declared — fix config.testing.scope_classes.%s", item.ScopeClass, item.ScopeClass)}
	}

	// Iterate required suites in sorted order so the failure message is
	// deterministic when multiple are missing — agents see the same suite
	// named first across runs instead of map-iteration-order roulette.
	suiteNames := make([]string, 0, len(requiredSuites))
	for name := range requiredSuites {
		suiteNames = append(suiteNames, name)
	}
	sort.Strings(suiteNames)
	for _, name := range suiteNames {
		val := getTestingEvidence(item, name)
		if val == "" || val == "null" {
			msg := fmt.Sprintf("required suite %q not recorded — run `st test %s %s --run` or `st test %s --auto`", name, item.ID, name, item.ID)
			// I-831: when goal tags map to a scope class, the missing evidence is
			// expected — add an actionable hint instead of a bare "run st test".
			if item.ScopeClass == "" {
				if suggestedClass := cfg.Testing.ScopeClassForGoalTags(item.Tags); suggestedClass != "" {
					msg = fmt.Sprintf("required suite %q not recorded (hint: goal tags suggest scope_class %q — run `st update %s scope_class %s` to use the correct suite set)", name, suggestedClass, item.ID, suggestedClass)
				}
			}
			return GateResult{Passed: false, Gate: "testing_complete", Message: msg}
		}
		// auto-skip: written by st test --auto when the suite's repo had no
		// changed files. Treated as "not applicable" — a system determination,
		// not a user bypass. Users still cannot --skip required suites.
		if strings.HasPrefix(val, "auto-skip:") {
			continue
		}
		if !strings.HasPrefix(val, "pass") {
			return GateResult{Passed: false, Gate: "testing_complete",
				Message: fmt.Sprintf("required suite %q did not pass: %s", name, val)}
		}
	}

	// Scope suites: only items WITHOUT a scope_class observe the default
	// scope-suite policy (st pr triggers + post-record marker). When an
	// item declares a scope_class, the class IS the complete required-set
	// definition — scope suites are not part of its gate model. If a class
	// needs additional suites (api_integration, web_e2e, etc.) the operator
	// declares them inside the class's required-suite set.
	if item.ScopeClass == "" {
		scopeNames := make([]string, 0, len(cfg.Testing.ScopeSuites))
		for name := range cfg.Testing.ScopeSuites {
			scopeNames = append(scopeNames, name)
		}
		sort.Strings(scopeNames)
		for _, name := range scopeNames {
			val := getTestingEvidence(item, name)
			if val == "required" {
				return GateResult{Passed: false, Gate: "testing_complete",
					Message: fmt.Sprintf("scope suite %q required but not recorded — run `st test %s %s`", name, item.ID, name)}
			}
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
