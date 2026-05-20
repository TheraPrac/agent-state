package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/quality"
	"github.com/jfinlinson/agent-state/internal/store"
)

// loadPlanForValidation loads the per-item plan sidecar and returns
// (a) the loaded plan (nil if missing), (b) ValidatePlan violations
// against the loaded plan, and (c) whether the sidecar was found.
// I-710 — the unified surface used by PlanApprove and PlanCheck.
// When the sidecar is missing, returns (nil, nil, false): the
// I-511-era carve-out is preserved so existing callers that
// approve without a sidecar (legacy items, tests) keep working.
// Closing this carve-out is tracked separately (see follow-up
// filed alongside I-710 review).
func loadPlanForValidation(cfg *config.Config, id string) (*plan.Plan, []quality.Violation, bool) {
	if cfg == nil {
		return nil, nil, false
	}
	p, err := plan.Load(cfg.PlansDir(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"plan-approve: could not load .plans/%s.md (%v); proceeding without plan validation. Investigate the sidecar.\n",
			id, err)
		return nil, nil, false
	}
	if p == nil {
		return nil, nil, false
	}
	return p, quality.ValidatePlan(p), true
}

// PlanApproveOpts holds flags for `st plan approve`. I-511 added
// Strict, which refuses approval if any linked plan sidecar contains
// an un-verifiable acceptance criterion (per plan.ValidateACs).
//
// I-589: Strict no longer governs the SBAR substance gate — the SBAR
// gate is now hard-blocking by default at every approval.
//
// I-710: Strict no longer governs the AC verifiability gate either —
// the AC gate fires unconditionally on every approval (analog of the
// I-589 SBAR flip). The `Strict` field is preserved as a no-op alias
// so existing CI invocations passing `--strict` keep working
// unchanged.
//
// I-710 also added `Engine`, a pointer to the RunEngine used to spawn
// a Claude plan-review sub-agent before the validator gates. A nil
// Engine skips the review entirely (preserves the in-process test
// surface, matching the I-588 pattern). The CLI binding at
// `cmd/as/app.go` sets Engine to `&command.DefaultRunEngine()`.
type PlanApproveOpts struct {
	Strict bool
	Engine *RunEngine
}

// PlanApprove marks an item's plan as approved. Sets PlanApproved=true,
// PlanApprovedAt=now, PlanApprovedBy=cfg.AgentID() (or "user" if empty).
// Refuses re-approval — the operator must `st plan reset` first if a
// previously-approved plan needs re-validation. Writes a changelog entry
// so the approval is auditable.
//
// I-178 Phase A: this is the as-side primitive that the
// `plan-before-code-guard.sh` hook (Phase B, separate per-agent install)
// will gate Edit/Write tool use against. Items not yet approved cannot
// have application code written for them.
func PlanApprove(s *store.Store, cfg *config.Config, id string, opts PlanApproveOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.PlanApproved {
		fmt.Fprintf(os.Stderr,
			"%s plan is already approved (by %s at %s) — run `st plan reset %s` first if it needs re-validation\n",
			id, fallback(item.PlanApprovedBy, "?"), fallback(item.PlanApprovedAt, "?"), id)
		return 1
	}

	// I-589: SBAR substance is hard-blocked by default on every
	// `st plan approve` (no `--strict` opt-in, no `--allow-incomplete-sbar`
	// escape hatch). Plans approved against a placeholder SBAR were the
	// "shallow item → shallow plan" failure mode I-149 was filed to
	// prevent; warn-only mode was load-bearing on author goodwill and
	// the author kept skipping. Triage items get a light-but-substantive
	// SBAR (one or two sentences per field is fine; raw `TODO:` scaffold
	// or single-word "TBD" is not). SBAR is only required on tasks/issues
	// (per the I-487 schema); ideas and promotions skip the gate entirely.
	//
	// I-511: --strict additionally refuses approval when the plan
	// sidecar's acceptance criteria fail the verifiability check. The
	// flag is preserved here purely for that AC gate — its SBAR role
	// from I-149 is gone (the SBAR check now runs unconditionally).
	sbarApplies := item.Type == "task" || item.Type == "issue"
	if sbarApplies {
		if vios := quality.ValidateSBAR(item); quality.HasError(vios) {
			fmt.Fprintf(os.Stderr,
				"%s: SBAR substance gate failed (%d section(s) empty or still on the I-492 scaffold); refusing approval:\n",
				id, len(vios))
			for _, v := range vios {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
			fmt.Fprintf(os.Stderr,
				"Run `st update %s sbar` to fill the four sections (one or two sentences per field), then re-run `st plan approve %s`.\n",
				id, id)
			return 2
		}
	}

	// I-710: plan-review sub-agent runs BEFORE the AC validator gates.
	// Engine-nil path (in-process tests) skips the review entirely.
	// Review failure (Reject / Feedback / claude error) = approval
	// refused (fail closed) — the gate is load-bearing for the
	// plan-before-code hook.
	if opts.Engine != nil {
		if code := runPlanReview(s, cfg, id, item, *opts.Engine); code != 0 {
			return code
		}
		// Reload item in case the sub-agent's auto-fix mutated fields.
		if refreshed, ok := s.Get(id); ok {
			item = refreshed
		}
	}

	// I-710: plan substance gate fires unconditionally (lifted out
	// of the `if opts.Strict` branch — analog of the I-589 SBAR flip).
	// Runs `quality.ValidatePlan`, which checks (a) Approach is
	// non-empty and non-scaffold (TODO/TBD/N/A/None), (b) ScopeRepos
	// is non-empty, and (c) every AC is verifiable per
	// `plan.ValidateACs`. `--strict` is preserved as a no-op alias
	// for backward compatibility with existing CI invocations.
	//
	// I-716: closes the missing-sidecar carve-out inherited from
	// I-511. When the item is task/issue and the sidecar is absent,
	// refuse approval with exit 2 pointing at `st plan prep <id>`.
	// Ideas/promotions skip per the I-487 type carve-out (no SBAR,
	// no enforced plan-body).
	_, vios, sidecarFound := loadPlanForValidation(cfg, id)
	if !sidecarFound && (item.Type == "task" || item.Type == "issue") {
		fmt.Fprintf(os.Stderr,
			"%s: missing .plans/%s.md — refusing approval. Run `st plan prep %s` to author a sidecar before re-running `st plan approve %s`. (I-716 closes the I-511/I-710 carve-out — no plan body, no approval.)\n",
			id, id, id, id)
		return 2
	}
	if quality.HasError(vios) {
		fmt.Fprintf(os.Stderr,
			"%s: plan substance gate failed (%d violation(s)); refusing approval:\n",
			id, len(vios))
		for _, v := range vios {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		fmt.Fprintf(os.Stderr,
			"Edit .plans/%s.md to fix the findings above, then re-run `st plan approve %s`.\n",
			id, id)
		return 2
	}

	approver := cfg.AgentID()
	if approver == "" {
		approver = "user"
	}
	approvedAt := time.Now().Format(time.RFC3339)

	// I-565: items prepped via `st prep --write-only` defer the
	// linked_plans stamp (no s.Mutate runs in prepItemWriteOnly), so
	// approval here must back-fill the sidecar path the same way the
	// interactive prepItem accept branch does — otherwise the I-512
	// invariant (linked_plans points at the active plan content) is
	// permanently broken for write-only items.
	var sidecarRel, scopeRepos, planApproach string
	var draftACs []string
	if p, _ := plan.Load(cfg.PlansDir(), id); p != nil {
		sidecarRel = relativePlanPath(cfg.PlansDir(), cfg.Root(), id)
		if len(p.ScopeRepos) > 0 {
			scopeRepos = strings.Join(p.ScopeRepos, ", ")
		}
		draftACs = append(draftACs, p.ACs...)
		// I-679 Phase B: the chosen approach is the real signal of the
		// decision (the "verdict"); capture a one-line gist so the
		// resume Decisions section carries content, not a bare pointer.
		planApproach = approachGist(p.Approach)
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		it.PlanApproved = true
		it.PlanApprovedAt = approvedAt
		it.PlanApprovedBy = approver
		it.Doc.SetField("plan_approved", "true")
		it.Doc.SetField("plan_approved_at", approvedAt)
		it.Doc.SetField("plan_approved_by", approver)
		if sidecarRel != "" {
			already := false
			for _, lp := range it.LinkedPlans {
				if lp == sidecarRel {
					already = true
					break
				}
			}
			if !already {
				it.LinkedPlans = append(it.LinkedPlans, sidecarRel)
				it.Doc.ReplaceList("linked_plans", it.LinkedPlans)
			}
		}
		if scopeRepos != "" {
			it.Doc.SetField("scope_repos", scopeRepos)
		}
		if len(it.AcceptanceCriteria) == 0 && len(draftACs) > 0 {
			it.Doc.ReplaceList("acceptance_criteria", draftACs)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "plan_approve",
		NewValue: approver,
		Reason:   "I-178 plan-before-code gate: plan approved",
	})

	// I-679 Phase B: a plan approval is a settled fork ("we will build it
	// this way") — capture it as a native-structured decision so a later
	// session's `st resume` surfaces the chosen approach without
	// re-deriving it. Carries the approach gist (the verdict — real
	// signal, not a bare pointer) and points at the live plan file for
	// full detail rather than snapshotting it (never store-and-trust).
	planDecision := fmt.Sprintf("plan approved by %s — full plan .plans/%s.md (live via `st resume %s`)", approver, id, id)
	if planApproach != "" {
		planDecision = fmt.Sprintf("approach approved by %s: %s — full plan .plans/%s.md (live via `st resume %s`)",
			approver, planApproach, id, id)
	}
	// Error intentionally not gated on here: recordStructuredDecision is
	// itself never-silent (emits the stderr warning), and a failed
	// decision capture must not abort an otherwise-successful plan
	// approval. The hook path (CaptureDecision) is what escalates the
	// error to a loud non-capture exit code.
	_ = recordStructuredDecision(cfg, id, "plan_approve", planDecision)

	fmt.Printf("Approved plan for %s (by %s at %s)\n", id, approver, approvedAt)
	autoSync(s, fmt.Sprintf("st plan approve: %s", id))
	return 0
}

// PlanReset reverts an item's plan-approval state. Used when the plan
// is rejected on review and needs to be regenerated, or when the
// approach changes mid-stream and the operator wants to re-validate.
// Writes a changelog entry so the reset is auditable.
func PlanReset(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if !item.PlanApproved {
		fmt.Fprintf(os.Stderr, "%s plan is not currently approved — nothing to reset\n", id)
		return 1
	}

	priorBy := item.PlanApprovedBy
	priorAt := item.PlanApprovedAt

	if err := s.Mutate(id, func(it *model.Item) error {
		it.PlanApproved = false
		it.PlanApprovedAt = ""
		it.PlanApprovedBy = ""
		it.Doc.SetField("plan_approved", "false")
		it.Doc.SetField("plan_approved_at", "")
		it.Doc.SetField("plan_approved_by", "")
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "plan_reset",
		OldValue: priorBy,
		Reason:   fmt.Sprintf("I-178 plan reset (was approved by %s at %s)", priorBy, priorAt),
	})

	fmt.Printf("Reset plan approval for %s (was approved by %s at %s)\n", id, fallback(priorBy, "?"), fallback(priorAt, "?"))
	autoSync(s, fmt.Sprintf("st plan reset: %s", id))
	return 0
}

// PlanCheck prints the approval state for `id` and exits 0 if approved,
// 1 if not. Designed for the `plan-before-code-guard.sh` hook to call as
// `st plan check $ITEM_ID > /dev/null` so the hook can deny Edit/Write
// when the gate is closed.
//
// I-589: the check now re-validates the SBAR substance gate alongside
// the PlanApproved flag, so a post-approval SBAR clear or direct-file
// edit that knocks an SBAR sub-field back to the I-492 scaffold closes
// the gate at the hook surface without requiring an explicit
// `st plan reset`. Ideas/promotions skip the SBAR check (they don't
// carry SBAR per the I-487 schema) and rely purely on PlanApproved.
func PlanCheck(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if !item.PlanApproved {
		fmt.Printf("not approved\n")
		return 1
	}
	if item.Type == "task" || item.Type == "issue" {
		if vios := quality.ValidateSBAR(item); quality.HasError(vios) {
			fmt.Printf("approved but SBAR substance gate now failing — re-fill SBAR or run `st plan reset %s`\n", id)
			for _, v := range vios {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
			return 1
		}
	}
	// I-710: re-validate plan substance at the hook surface so a
	// post-approval edit that knocks Approach/ScopeRepos/ACs back to
	// scaffold or non-verifiable closes the plan-before-code gate
	// without requiring an explicit `st plan reset`. Same posture as
	// the I-589 SBAR re-check above. All output to stderr so the
	// stdout verdict line stays parseable by hook callers.
	//
	// I-716: also re-validate sidecar EXISTENCE. A post-approval
	// deletion of `.plans/<id>.md` must close the hook gate (else
	// an agent could delete its plan after approval and continue
	// editing code).
	_, vios, sidecarFound := loadPlanForValidation(cfg, id)
	if !sidecarFound && (item.Type == "task" || item.Type == "issue") {
		fmt.Fprintf(os.Stderr,
			"approved but .plans/%s.md is missing — restore the sidecar or run `st plan reset %s`\n", id, id)
		return 1
	}
	if quality.HasError(vios) {
		fmt.Fprintf(os.Stderr,
			"approved but %d plan substance violation(s) — fix .plans/%s.md or run `st plan reset %s`\n",
			len(vios), id, id)
		for _, v := range vios {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		return 1
	}
	fmt.Printf("approved by %s at %s\n", fallback(item.PlanApprovedBy, "?"), fallback(item.PlanApprovedAt, "?"))
	return 0
}

// PlanShow renders a detailed view of an item's plan-approval state plus
// any linked plan-file paths. I-565 extends it to inline the plan body
// and the plan-review report (when sidecars exist) so an agent can read
// both artifacts in one call.
func PlanShow(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	fmt.Printf("Plan for %s — %s\n", id, item.Title)
	if item.PlanApproved {
		fmt.Printf("  Status:      approved\n")
		fmt.Printf("  Approved by: %s\n", fallback(item.PlanApprovedBy, "?"))
		fmt.Printf("  Approved at: %s\n", fallback(item.PlanApprovedAt, "?"))
	} else {
		fmt.Printf("  Status:      not approved\n")
	}
	if len(item.LinkedPlans) == 0 {
		fmt.Printf("  Linked:      (none)\n")
	} else {
		fmt.Printf("  Linked plans:\n")
		for _, p := range item.LinkedPlans {
			fmt.Printf("    - %s\n", p)
		}
	}

	// I-565: inline the plan body from .plans/<id>.md if it exists.
	plansDir := cfg.PlansDir()
	if loaded, err := plan.Load(plansDir, id); err == nil && loaded != nil {
		fmt.Printf("\n=== Plan: .plans/%s.md ===\n", id)
		if loaded.RawText != "" {
			fmt.Print(loaded.RawText)
			if !strings.HasSuffix(loaded.RawText, "\n") {
				fmt.Println()
			}
		} else {
			fmt.Print(plan.Render(loaded))
		}
	}

	// And the plan-review report (write-only prep produces this).
	// Mirror the plan-block guard above: only emit the section when a
	// report sidecar actually exists, so `st plan show` on items that
	// never used --write-only stays quiet.
	if plan.ReportExists(plansDir, id) {
		fmt.Printf("\n=== Report: .plans/%s.report.md ===\n", id)
		report, err := plan.LoadReport(plansDir, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "(error loading report: %v)\n", err)
		} else {
			fmt.Print(report)
			if !strings.HasSuffix(report, "\n") {
				fmt.Println()
			}
		}
	}
	return 0
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// loadStrictACFindings is retained as the legacy AC-only validator
// surface for any callers that need plan.ACFinding directly (e.g.,
// future tooling that wants the raw finding shape). The unified
// substance gate used by `PlanApprove`/`PlanCheck` now routes
// through `loadPlanForValidation` + `quality.ValidatePlan` so
// Approach + ScopeRepos + ACs are all checked together. Behavior on
// missing sidecar matches the I-511 carve-out: returns nil, the
// gate has no data to fire against. I-710.
func loadStrictACFindings(cfg *config.Config, id string) []plan.ACFinding {
	if cfg == nil {
		return nil
	}
	p, err := plan.Load(cfg.PlansDir(), id)
	if err != nil {
		// Surface non-IsNotExist errors so a corrupt sidecar doesn't
		// silently neutralize the gate. plan.Load already returns
		// (nil, nil) for IsNotExist, so any non-nil err here is a
		// real parse / read failure worth logging.
		fmt.Fprintf(os.Stderr,
			"plan-approve: could not load .plans/%s.md (%v); proceeding without AC validation. Investigate the sidecar.\n",
			id, err)
		return nil
	}
	if p == nil {
		// Sidecar doesn't exist — the gate has nothing to validate.
		return nil
	}
	return plan.ValidateACs(p.ACs)
}
