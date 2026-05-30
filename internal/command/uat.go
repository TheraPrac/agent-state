package command

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
	// Skipped is true when the value was an intentional `skip: <reason>`
	// — typically a scope suite the operator marked N/A for this change.
	// Skipped rows render as ⊘ and do NOT count as auto-fail. Check
	// before Passed in render/count branches so a Passed=true Skipped=true
	// row still renders ⊘ rather than ✓.
	Skipped bool `json:"skipped,omitempty"`
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
		// Determine best CWD: worktree base if exists, else project root.
		// I-407: WorktreeForItem prefers <agent-root>/worktrees/<id>,
		// falls back to legacy <workspace>/worktrees/<id> for old worktrees.
		runDir := cfg.Root()
		if wtBase := cfg.WorktreeForItem(id); wtBase != "" {
			if _, err := os.Stat(wtBase); err == nil {
				runDir = wtBase
			}
		}
		runCmd = func(cmd string) ([]byte, int, error) {
			cmd = rewriteACPaths(cfg, id, runDir, cmd)
			return runCmdInDirWithTimeout(runDir, cmd, 10*time.Minute)
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
	autoPass, autoFail, manual, skipped := 0, 0, 0, 0
	for _, r := range results {
		switch {
		case r.Pending:
			manual++
		case r.Skipped:
			skipped++
		case r.Passed:
			autoPass++
		default:
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
		// Skipped must be checked BEFORE Passed so a Passed=true
		// Skipped=true row still renders ⊘ instead of ✓.
		icon := fmt.Sprintf("%s✓%s", cGreen, cReset)
		switch {
		case r.Skipped:
			icon = fmt.Sprintf("%s⊘%s", cBlue, cReset)
		case !r.Passed:
			icon = fmt.Sprintf("%s✗%s", cRed, cReset)
		}
		fmt.Printf("  %s %s: %s\n", icon, r.Label, r.Detail)
	}
	fmt.Println()

	// AC section
	if len(acResults) > 0 {
		fmt.Printf("%sACCEPTANCE CRITERIA:%s\n", cBold, cReset)
		for i, r := range acResults {
			// Branch order: Pending → Skipped → !Passed → default (✓).
			icon := fmt.Sprintf("%s✓%s", cGreen, cReset)
			switch {
			case r.Pending:
				icon = fmt.Sprintf("%s⬜%s", cYellow, cReset)
			case r.Skipped:
				icon = fmt.Sprintf("%s⊘%s", cBlue, cReset)
			case !r.Passed:
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
	if skipped > 0 {
		fmt.Printf("%s%d skipped%s, ", cBlue, skipped, cReset)
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
	// I-776: use the item's class-scoped required-suite set so UAT agrees with
	// the gate. A workspace-config item should see workspace_test here, not
	// the global api/web Tier 1 that doesn't apply to it.
	requiredSuites, classOK := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
	if !classOK {
		// Surface the unknown-class error as a check failure rather than
		// silently iterating an empty set; matches the gate's fail-fast.
		results = append(results, checkResult{
			Label:  "scope_class",
			Mode:   "auto",
			Passed: false,
			Detail: "unknown scope_class " + item.ScopeClass,
		})
		return results
	}
	for name := range requiredSuites {
		val := ""
		if v, ok := item.TestingEvidence[name]; ok {
			if s, ok := v.(string); ok {
				val = s
			}
		}
		detail := val
		if detail == "" || detail == "null" {
			detail = "not recorded"
		}
		// auto-skip written by st test --auto: suite not applicable (no repo
		// changes) — render as ⊘ skipped, same as user-initiated skip:.
		if strings.HasPrefix(val, "auto-skip:") {
			results = append(results, checkResult{
				Label:   name,
				Mode:    "auto",
				Passed:  true,
				Skipped: true,
				Detail:  detail,
			})
			continue
		}
		passed := strings.HasPrefix(val, "pass")
		results = append(results, checkResult{
			Label:  name,
			Mode:   "auto",
			Passed: passed,
			Detail: detail,
		})
	}
	// Check triggered scope suites — I-776: only default-class items
	// observe scope-suite policy, mirroring the gate's behavior in
	// evalTestingComplete. Class items have a closed required-set
	// definition; stale `required` markers on class items must not
	// resurface as UAT failures the gate ignores.
	if item.ScopeClass != "" {
		return results
	}
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
		// I-540: `skip: <reason>` (written by `st test <id> <suite> --skip`)
		// is an explicit operator marker that this scope suite is N/A for
		// the current change. Render as ⊘ skipped, not auto-fail.
		if strings.HasPrefix(val, "skip:") {
			results = append(results, checkResult{
				Label:   name,
				Mode:    "auto",
				Passed:  true,
				Skipped: true,
				Detail:  val,
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

// ValidateACsyntax checks each cmd: AC for shell syntax errors using sh -n.
// Returns a list of ACs with syntax errors (index + error message).
// Call this at plan approval time to catch bad ACs before they enter the pipeline.
func ValidateACsyntax(acs []string) []string {
	var errors []string
	for i, ac := range acs {
		trimmed := strings.TrimSpace(ac)
		trimmed = strings.TrimPrefix(trimmed, "- ")
		if !strings.HasPrefix(trimmed, "cmd:") {
			continue
		}
		cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "cmd:"))
		if cmd == "" {
			errors = append(errors, fmt.Sprintf("AC #%d: empty command", i+1))
			continue
		}
		// Anti-pattern: st test --run re-runs full suites during UAT, bypassing
		// pre-recorded testing_evidence and causing 10-40 minute UAT runs.
		if reStTestRun.MatchString(cmd) {
			errors = append(errors, fmt.Sprintf(
				"AC #%d: anti-pattern — 'st test --run' re-runs the full suite during UAT; use a targeted 'go test -run TestFoo' instead. Suite pass/fail is already checked from testing_evidence.\n  cmd: %s",
				i+1, cmd,
			))
		}
		// Shell syntax check
		check := exec.Command("sh", "-n", "-c", cmd)
		if out, err := check.CombinedOutput(); err != nil {
			errMsg := strings.TrimSpace(string(out))
			if errMsg == "" {
				errMsg = err.Error()
			}
			errors = append(errors, fmt.Sprintf("AC #%d: shell syntax error: %s\n  cmd: %s", i+1, errMsg, cmd))
		}
	}
	return errors
}

// reStTestRun matches the anti-pattern `st test <id> <suite> --run` in a cmd: AC.
var reStTestRun = regexp.MustCompile(`\bst\s+test\b.+--run\b`)

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
		// I-776: use the item's class-scoped required-suite set so a workspace-config
		// item's AC "workspace_test green" resolves to testing_evidence.workspace_test
		// instead of falling through to the manifest/manual branch. classOK=false
		// (unknown class) lands the criterion in the manual branch as a fallback.
		requiredSuites, _ := cfg.Testing.RequiredSuitesFor(item.ScopeClass)
		for name := range requiredSuites {
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
		// I-776: only default-class items consult ScopeSuites. Class items
		// have a closed required-set definition — an AC mentioning a default
		// scope-suite name on a class item must fall through to the
		// manifest/manual evaluation branches instead of resolving against
		// evidence the gate doesn't read.
		if item.ScopeClass == "" {
			for name := range cfg.Testing.ScopeSuites {
				if strings.Contains(lower, strings.ReplaceAll(name, "_", " ")) || strings.Contains(lower, name) {
					val := ""
					if v, ok := item.TestingEvidence[name]; ok {
						if s, ok := v.(string); ok {
							val = s
						}
					}
					// I-540: a prose AC mentioning a scope suite marked
					// `skip: <reason>` must render as ⊘ skipped, not ✗ fail.
					// auto-skip: is a system-determined "not applicable" written
					// by st test --auto; render identically.
					if strings.HasPrefix(val, "skip:") || strings.HasPrefix(val, "auto-skip:") {
						return checkResult{Label: criterion, Mode: "auto", Passed: true, Skipped: true, Detail: val}
					}
					passed := strings.HasPrefix(val, "pass")
					return checkResult{Label: criterion, Mode: "auto", Passed: passed, Detail: val}
				}
			}
		}
	}

	// Check for manifest/PR keywords
	if strings.Contains(lower, "manifest") || strings.Contains(lower, "pr recorded") || strings.Contains(lower, "pr created") {
		prs := ""
		if item.Manifest != nil {
			if v, ok := item.Manifest["prs"]; ok {
				if s, ok := v.(string); ok {
					prs = s
				}
			}
		}
		passed := prs != "" && prs != "null"
		detail := prs
		if !passed {
			detail = "no PR manifest recorded"
		}
		return checkResult{Label: criterion, Mode: "auto", Passed: passed, Detail: detail}
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

	// No cmd: prefix and no keyword match — mark as pending manual rewrite.
	// Does NOT count as auto-fail (won't block the pipeline).
	return checkResult{Label: criterion, Mode: "manual", Passed: false, Pending: true, Detail: "needs rewrite as cmd: <test command>"}
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
