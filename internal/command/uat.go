package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// UATOpts holds injectable functions for the uat command.
type UATOpts struct {
	RunCmd  func(cmd string) ([]byte, int, error)
	Backend evidence.Backend
}

// checkResult holds the outcome of evaluating one criterion or cross-cutting check.
type checkResult struct {
	Label   string `json:"label"`
	Mode    string `json:"mode"` // "auto", "cmd", "manual"
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail"`
	Pending bool   `json:"pending,omitempty"` // manual review needed
}

// UAT runs automated acceptance criteria verification and produces a report.
func UAT(s *store.Store, cfg *config.Config, id string, opts UATOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	runCmd := opts.RunCmd
	if runCmd == nil {
		root := cfg.Root()
		runCmd = func(cmd string) ([]byte, int, error) {
			return runCmdInDir(root, cmd)
		}
	}

	var results []checkResult

	// --- Cross-cutting checks ---
	results = append(results, checkTestSuites(item, cfg)...)
	results = append(results, checkManifest(item))
	results = append(results, checkDeliveryStage(item))
	results = append(results, checkEvidenceURIs(item, cfg))

	// --- Acceptance criteria ---
	acResults := evaluateAcceptanceCriteria(item, cfg, runCmd)
	results = append(results, acResults...)

	// --- Render report ---
	autoPass, autoFail, manual := 0, 0, 0
	for _, r := range results {
		if r.Pending {
			manual++
		} else if r.Passed {
			autoPass++
		} else {
			autoFail++
		}
	}

	fmt.Printf("\n%s═══ UAT Report: %s ═══%s\n", cBoldW, id, cReset)
	fmt.Printf("%s\n\n", item.Title)

	// Cross-cutting section
	fmt.Printf("%sAUTOMATED CHECKS:%s\n", cBold, cReset)
	for _, r := range results {
		if r.Mode == "manual" || r.Mode == "cmd" {
			continue // show in AC section
		}
		icon := fmt.Sprintf("%s✓%s", cGreen, cReset)
		if !r.Passed {
			icon = fmt.Sprintf("%s✗%s", cRed, cReset)
		}
		fmt.Printf("  %s %s: %s\n", icon, r.Label, r.Detail)
	}
	fmt.Println()

	// AC section
	if len(acResults) > 0 {
		fmt.Printf("%sACCEPTANCE CRITERIA:%s\n", cBold, cReset)
		for i, r := range acResults {
			icon := fmt.Sprintf("%s✓%s", cGreen, cReset)
			if r.Pending {
				icon = fmt.Sprintf("%s⬜%s", cYellow, cReset)
			} else if !r.Passed {
				icon = fmt.Sprintf("%s✗%s", cRed, cReset)
			}
			mode := r.Mode
			detail := ""
			if r.Detail != "" {
				detail = fmt.Sprintf(" — %s", r.Detail)
			}
			fmt.Printf("  %d. [%s] %s %s%s\n", i+1, mode, icon, r.Label, detail)
		}
		fmt.Println()
	}

	// Summary
	fmt.Printf("%sSUMMARY:%s %s%d auto-pass%s, ", cBold, cReset, cGreen, autoPass, cReset)
	if autoFail > 0 {
		fmt.Printf("%s%d auto-fail%s, ", cRed, autoFail, cReset)
	} else {
		fmt.Printf("0 auto-fail, ")
	}
	if manual > 0 {
		fmt.Printf("%s%d manual review%s\n", cYellow, manual, cReset)
	} else {
		fmt.Printf("0 manual review\n")
	}
	fmt.Println()

	// Upload report to evidence backend
	uploadUATReport(cfg, id, item, results, opts)

	if autoFail > 0 {
		return 1
	}
	return 0
}

// --- Cross-cutting checks ---

func checkTestSuites(item *model.Item, cfg *config.Config) []checkResult {
	if cfg.Testing == nil {
		return nil
	}
	var results []checkResult
	for name := range cfg.Testing.RequiredSuites {
		val := ""
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				val = s
			}
		}
		passed := strings.HasPrefix(val, "pass")
		detail := val
		if detail == "" || detail == "null" {
			detail = "not recorded"
		}
		results = append(results, checkResult{
			Label:  name,
			Mode:   "auto",
			Passed: passed,
			Detail: detail,
		})
	}
	// Check triggered scope suites
	for name := range cfg.Testing.ScopeSuites {
		val := ""
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				val = s
			}
		}
		if val == "" || val == "null" {
			continue // not triggered, skip
		}
		if val == "required" {
			results = append(results, checkResult{
				Label:  name,
				Mode:   "auto",
				Passed: false,
				Detail: "required but not recorded",
			})
			continue
		}
		passed := strings.HasPrefix(val, "pass")
		results = append(results, checkResult{
			Label:  name,
			Mode:   "auto",
			Passed: passed,
			Detail: val,
		})
	}
	return results
}

func checkManifest(item *model.Item) checkResult {
	prs := ""
	if item.Manifest != nil {
		if v, ok := item.Manifest["prs"]; ok {
			if s, ok := v.(string); ok {
				prs = s
			}
		}
	}
	if prs != "" && prs != "null" {
		return checkResult{Label: "PR manifest", Mode: "auto", Passed: true, Detail: prs}
	}
	return checkResult{Label: "PR manifest", Mode: "auto", Passed: false, Detail: "not recorded"}
}

func checkDeliveryStage(item *model.Item) checkResult {
	stage := deliveryStage(item)
	if stage == "" {
		return checkResult{Label: "Delivery stage", Mode: "auto", Passed: false, Detail: "not set"}
	}
	return checkResult{Label: "Delivery stage", Mode: "auto", Passed: true, Detail: stage}
}

func checkEvidenceURIs(item *model.Item, cfg *config.Config) checkResult {
	count := 0
	if item.TestingEvidence != nil {
		for _, v := range item.TestingEvidence {
			if s, ok := v.(string); ok && strings.Contains(s, "evidence:") {
				count++
			}
		}
	}
	if item.Delivery != nil {
		for k, v := range item.Delivery {
			if strings.HasSuffix(k, "_evidence") {
				if s, ok := v.(string); ok && s != "" && s != "null" {
					count++
				}
			}
		}
	}
	if count > 0 {
		return checkResult{Label: "Evidence URIs", Mode: "auto", Passed: true, Detail: fmt.Sprintf("%d evidence link(s)", count)}
	}
	return checkResult{Label: "Evidence URIs", Mode: "auto", Passed: false, Detail: "no evidence URIs found"}
}

// --- Acceptance criteria evaluation ---

func evaluateAcceptanceCriteria(item *model.Item, cfg *config.Config, runCmd func(string) ([]byte, int, error)) []checkResult {
	var results []checkResult
	for _, ac := range item.AcceptanceCriteria {
		results = append(results, evaluateCriterion(ac, item, cfg, runCmd))
	}
	return results
}

func evaluateCriterion(criterion string, item *model.Item, cfg *config.Config, runCmd func(string) ([]byte, int, error)) checkResult {
	// cmd: prefix — execute command
	if strings.HasPrefix(criterion, "cmd:") {
		cmd := strings.TrimSpace(strings.TrimPrefix(criterion, "cmd:"))
		output, exitCode, err := runCmd(cmd)
		if err != nil && exitCode == 0 {
			return checkResult{Label: criterion, Mode: "cmd", Passed: false, Detail: fmt.Sprintf("exec error: %v", err)}
		}
		detail := "exit 0"
		if exitCode != 0 {
			detail = fmt.Sprintf("exit %d: %s", exitCode, truncate(string(output), 100))
		}
		return checkResult{Label: criterion, Mode: "cmd", Passed: exitCode == 0, Detail: detail}
	}

	// Check if criterion mentions a known suite name
	lower := strings.ToLower(criterion)
	if cfg.Testing != nil {
		for name := range cfg.Testing.RequiredSuites {
			if strings.Contains(lower, strings.ReplaceAll(name, "_", " ")) || strings.Contains(lower, name) {
				val := ""
				if v, ok := item.TestingEvidence[name]; ok {
					if s, ok := v.(string); ok {
						val = s
					}
				}
				passed := strings.HasPrefix(val, "pass")
				return checkResult{Label: criterion, Mode: "auto", Passed: passed, Detail: val}
			}
		}
		for name := range cfg.Testing.ScopeSuites {
			if strings.Contains(lower, strings.ReplaceAll(name, "_", " ")) || strings.Contains(lower, name) {
				val := ""
				if v, ok := item.TestingEvidence[name]; ok {
					if s, ok := v.(string); ok {
						val = s
					}
				}
				passed := strings.HasPrefix(val, "pass")
				return checkResult{Label: criterion, Mode: "auto", Passed: passed, Detail: val}
			}
		}
	}

	// Check for keywords that match cross-cutting checks
	if strings.Contains(lower, "ci") || strings.Contains(lower, "checks") || strings.Contains(lower, "pipeline") {
		stage := deliveryStage(item)
		if stage != "" {
			return checkResult{Label: criterion, Mode: "auto", Passed: true, Detail: fmt.Sprintf("delivery at %s", stage)}
		}
	}

	if strings.Contains(lower, "coverage") || strings.Contains(lower, ">=90%") || strings.Contains(lower, "90%") {
		// Check if any evidence mentions coverage
		for _, v := range item.TestingEvidence {
			if s, ok := v.(string); ok && strings.Contains(s, "evidence:") {
				return checkResult{Label: criterion, Mode: "auto", Passed: true, Detail: "coverage evidence recorded"}
			}
		}
	}

	// Default: manual review
	return checkResult{Label: criterion, Mode: "manual", Pending: true, Detail: "needs human verification"}
}

// --- Report upload ---

func uploadUATReport(cfg *config.Config, id string, item *model.Item, results []checkResult, opts UATOpts) {
	backend := opts.Backend
	if backend == nil {
		var err error
		backend, err = evidence.New(evidenceConfigFromCfg(cfg))
		if err != nil {
			return
		}
	}

	report := map[string]interface{}{
		"id":          id,
		"title":       item.Title,
		"results":     results,
		"recorded_at": time.Now().Format(time.RFC3339),
	}
	data, _ := json.MarshalIndent(report, "", "  ")

	sha := getSHA(TestRecordOpts{})
	ts := time.Now().Format("20060102T150405")
	key := fmt.Sprintf("%s/uat/%s/%s/report.json", id, sha, ts)
	evidence.GzipUpload(backend, key, data)
}
