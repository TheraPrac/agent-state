package command

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
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
	// I-831: when a default-class item carries goal tags that map to a
	// scope class, surface an advisory hint. Rendered as ⊘ (Skipped) so it
	// does not count as auto-fail — it is informational, not a gate.
	if item.ScopeClass == "" {
		if suggestedClass := cfg.Testing.ScopeClassForGoalTags(item.Tags); suggestedClass != "" {
			results = append(results, checkResult{
				Label:   "scope_class_hint",
				Mode:    "auto",
				Passed:  true,
				Skipped: true,
				Detail:  fmt.Sprintf("goal tags suggest scope_class %q — run `st update %s scope_class %s` then re-run", suggestedClass, item.ID, suggestedClass),
			})
		}
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
		// I-1479: `auto-skip:` is a system-determined "not applicable" written
		// by st test --auto (same semantics; mirroring required-suite handling
		// at line 213 and the evalTestingComplete gate).
		if strings.HasPrefix(val, "skip:") || strings.HasPrefix(val, "auto-skip:") {
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
		cmd, ok := extractACcmd(ac)
		if !ok {
			continue
		}
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
			continue
		}
		// Anti-pattern: bare `go test` without -run runs the full suite.
		if reGoTestSuite.MatchString(cmd) && !reGoTestRunFilter.MatchString(cmd) {
			errors = append(errors, fmt.Sprintf(
				"AC #%d: anti-pattern — 'go test' without -run re-runs the full suite during UAT; use 'go test -run TestFoo' for a targeted check. Suite pass/fail is already checked from testing_evidence.\n  cmd: %s",
				i+1, cmd,
			))
			continue
		}
		// Anti-pattern: make test-* runs the full suite for that target.
		if reMakeTestTarget.MatchString(cmd) {
			errors = append(errors, fmt.Sprintf(
				"AC #%d: anti-pattern — 'make test-*' re-runs the full suite during UAT; use a targeted 'go test -run TestFoo' or verify via testing_evidence instead.\n  cmd: %s",
				i+1, cmd,
			))
			continue
		}
		// Anti-pattern: npm run test without --testPathPattern runs all JS tests.
		if reNpmRunTest.MatchString(cmd) && !strings.Contains(cmd, "--testPathPattern") {
			errors = append(errors, fmt.Sprintf(
				"AC #%d: anti-pattern — 'npm run test' without --testPathPattern re-runs the full suite during UAT; use 'npm run test -- --testPathPattern=ComponentName' instead. Suite pass/fail is already checked from testing_evidence.\n  cmd: %s",
				i+1, cmd,
			))
			continue
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

// reGoTestSuite matches any `go test` invocation; paired with reGoTestRunFilter
// to detect full-suite runs (no -run filter).
var reGoTestSuite = regexp.MustCompile(`\bgo\s+test\b`)

// reGoTestRunFilter matches the -run flag that scopes a `go test` to specific functions.
// Handles -run with space, = separator, or quoted env var (GOFLAGS="-run=...").
var reGoTestRunFilter = regexp.MustCompile(`(?:^|[\s"=])-run\b`)

// reMakeTestTarget matches `make test-<anything>` suite targets.
var reMakeTestTarget = regexp.MustCompile(`\bmake\s+test-\S`)

// reNpmRunTest matches bare `npm run test` (not test:unit, test:e2e, etc.).
var reNpmRunTest = regexp.MustCompile(`\bnpm\s+run\s+test(?:\s|$)`)

// extractACcmd strips "- " and "cmd:" prefixes from an AC string and returns
// (cmd, true) when the AC is a cmd: entry, or ("", false) otherwise.
// Used by both ValidateACsyntax and CleanACs to keep trimming in sync.
func extractACcmd(ac string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ac), "- "))
	if !strings.HasPrefix(trimmed, "cmd:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "cmd:")), true
}

// isSuiteRunCmd returns true if cmd is an anti-pattern full-suite invocation.
func isSuiteRunCmd(cmd string) bool {
	if reStTestRun.MatchString(cmd) {
		return true
	}
	if reGoTestSuite.MatchString(cmd) && !reGoTestRunFilter.MatchString(cmd) {
		return true
	}
	if reMakeTestTarget.MatchString(cmd) {
		return true
	}
	if reNpmRunTest.MatchString(cmd) && !strings.Contains(cmd, "--testPathPattern") {
		return true
	}
	return false
}

// CleanACsOpts configures the CleanACs operation.
type CleanACsOpts struct {
	Apply bool   // if false, dry-run only (print what would be removed)
	Item  string // if set, only scan this one item ID
}

// CleanACs scans open items for suite-run ACs and removes them.
// Dry-run by default; pass Apply: true to commit changes.
func CleanACs(s *store.Store, cfg *config.Config, opts CleanACsOpts) int {
	// Build target item list.
	var items []*model.Item
	if opts.Item != "" {
		it, ok := s.Get(opts.Item)
		if !ok {
			fmt.Fprintf(os.Stderr, "item %s not found\n", opts.Item)
			return 1
		}
		if cfg.IsTerminalStatus(it.Type, it.Status) {
			fmt.Fprintf(os.Stderr, "item %s is terminal (status: %s) — skipping\n", opts.Item, it.Status)
			return 1
		}
		items = []*model.Item{it}
	} else {
		items = s.List(func(it *model.Item) bool {
			return !cfg.IsTerminalStatus(it.Type, it.Status) && len(it.AcceptanceCriteria) > 0
		})
	}

	// Collect removals grouped by item.
	type removal struct{ idx int; ac string }
	byItem := make(map[string][]removal)
	var order []string
	for _, item := range items {
		for i, ac := range item.AcceptanceCriteria {
			cmd, ok := extractACcmd(ac)
			if !ok {
				continue
			}
			if isSuiteRunCmd(cmd) {
				if _, seen := byItem[item.ID]; !seen {
					order = append(order, item.ID)
				}
				byItem[item.ID] = append(byItem[item.ID], removal{idx: i, ac: ac})
			}
		}
	}

	if len(order) == 0 {
		fmt.Println("No suite-run ACs found.")
		return 0
	}

	total := 0
	for _, id := range order {
		removals := byItem[id]
		total += len(removals)
		fmt.Printf("%s: %d suite-run AC(s) to remove\n", id, len(removals))
		for _, r := range removals {
			fmt.Printf("  [%d] %s\n", r.idx+1, r.ac)
		}
	}

	if !opts.Apply {
		fmt.Printf("\nDry run: %d AC(s) in %d item(s) would be removed. Re-run with --apply to commit.\n", total, len(order))
		return 0
	}

	// Apply removals: for each item, remove matching ACs in one Mutate call.
	// The Mutate closure re-scans from the disk-parsed item so it is safe
	// even when the in-memory cache differs from what ended up on disk.
	failed := 0
	for _, id := range order {
		var removedACs []string
		if err := s.Mutate(id, func(it *model.Item) error {
			var kept []string
			for _, ac := range it.AcceptanceCriteria {
				cmd, ok := extractACcmd(ac)
				if !ok || !isSuiteRunCmd(cmd) {
					kept = append(kept, ac)
				} else {
					removedACs = append(removedACs, ac)
				}
			}
			// Update both the struct field and the Doc (Doc drives serialization).
			it.AcceptanceCriteria = kept
			rawKept := make([]string, len(kept))
			for i, k := range kept {
				if strings.HasPrefix(k, "- ") {
					rawKept[i] = k
				} else {
					rawKept[i] = "- " + k
				}
			}
			it.Doc.ReplaceList("acceptance_criteria", rawKept)
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "%s: mutate failed: %v\n", id, err)
			failed++
			continue
		}
		for _, ac := range removedACs {
			changelog.Append(cfg, id, changelog.Entry{
				Op: "ac_purge", Field: "acceptance_criteria",
				OldValue: ac, NewValue: "",
			})
		}
		fmt.Printf("  %s: removed %d AC(s)\n", id, len(removedACs))
	}

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d item(s) failed to update\n", failed)
		return 1
	}

	if err := autoSync(s, fmt.Sprintf("st uat --clean-acs: purged suite-run ACs from %d item(s)", len(order))); err != nil {
		return 1
	}
	return 0
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
			// I-144: when the suite exits non-zero but the command carried a
			// test-name filter, check per-test PASS/FAIL lines to distinguish
			// the targeted test failing from an unrelated test failing.
			if override, warning := evaluateFilteredCmd(cmd, string(output)); override != nil && *override {
				return checkResult{Label: criterion, Mode: "cmd", Passed: true, Detail: warning + "; " + detail}
			}
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
