package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/plan"
	"github.com/theraprac/agent-state/internal/quality"
	"github.com/theraprac/agent-state/internal/store"
)

// applyACs deduplicates rawACs and writes them onto the item's
// AcceptanceCriteria field (unprefixed) and Doc list (with "- " prefix).
// Shared by PlanApprove, the idempotent guard, and prep.go's accept path.
func applyACs(it *model.Item, rawACs []string) {
	if len(rawACs) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(rawACs))
	deduped := make([]string, 0, len(rawACs))
	for _, ac := range rawACs {
		if _, exists := seen[ac]; !exists {
			seen[ac] = struct{}{}
			deduped = append(deduped, ac)
		}
	}
	it.AcceptanceCriteria = deduped
	// ReplaceList expects "- " prefixed raw strings; model layer (parseList)
	// strips that prefix on read.
	prefixed := make([]string, len(deduped))
	for i, ac := range deduped {
		prefixed[i] = "- " + ac
	}
	it.Doc.ReplaceList("acceptance_criteria", prefixed)
}

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
	// BypassReview skips the I-710 plan-review sub-agent (operator escape
	// hatch when the sub-agent is broken or the plan has been manually
	// reviewed). I-752: added after I-738 hung 53min on the unbounded
	// sub-agent. The validator gates (SBAR, AC verifiability) still run.
	//
	// I-933: the sub-agent is now OFF by default, so BypassReview is a
	// no-op back-compat alias — it only triggers a deprecation notice.
	BypassReview bool
	// Review opts INTO the I-710 plan-review sub-agent (I-933). A full-corpus
	// audit showed the mandatory cold re-explore never vetoed a plan and that
	// ~half its value is now covered by the deterministic hollow-AC linter, so
	// the slow LLM re-exploration moved from default-on to this explicit
	// opt-in — reserved for genuinely thin/exploratory SBARs where scope is
	// uncertain. The static gates (SBAR, AC verifiability incl. the hollow-AC
	// linter) still fire on every approval regardless of this flag.
	Review bool
	// I-591: RequireEstimate blocks approval when the item has no
	// time_tracking.estimated_hours set (or the value is zero/missing).
	RequireEstimate bool
}

// PlanApprove marks an item's plan as approved. Sets PlanApproved=true,
// PlanApprovedAt=now, PlanApprovedBy=cfg.AgentID() (or "user" if empty).
// Idempotent on re-approval (I-832): a second call on an already-approved
// item emits a "no-op (idempotent re-run)" notice, calls autoSync
// defensively, and returns 0 without touching audit fields. To force
// re-validation, call PlanReset first. Writes a changelog entry on the
// first approval so the approval is auditable.
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
		// I-832: idempotent re-run — a retry after a silent autoSync failure
		// must not exit 1 ("already approved"), which would close the agent
		// into an infinite retry loop. Emit a no-op notice, call autoSync
		// defensively (gives a stuck-uncommitted approval a second chance to
		// land in git), and return 0. Audit fields are deliberately NOT
		// re-written — the original approver/timestamp belong to the first call.
		//
		// I-1649: also run AC replacement so a follow-up `st plan approve`
		// after an interactive-prep accept refreshes stale ACs. The Mutate
		// error is ignored — an idempotent re-run must not fail closed when
		// the only difference is AC freshness.
		if p, _ := plan.Load(cfg.PlansDir(), id); p != nil && len(p.ACs) > 0 {
			_ = s.Mutate(id, func(it *model.Item) error {
				applyACs(it, p.ACs)
				return nil
			})
		}
		fmt.Fprintf(os.Stderr,
			"%s plan is already approved (by %s at %s) — no-op (idempotent re-run)\n",
			id, fallback(item.PlanApprovedBy, "?"), fallback(item.PlanApprovedAt, "?"))
		if err := autoSync(s, fmt.Sprintf("st plan approve: %s (idempotent re-run)", id)); err != nil {
			return 1
		}
		return 0
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
	// I-591: estimate gate — block approval when RequireEstimate is set and the
	// item has no estimated_hours (or the value is zero).
	if opts.RequireEstimate && readFloatField(item, "time_tracking", "estimated_hours") <= 0 {
		fmt.Fprintf(os.Stderr,
			"%s: estimate gate failed — no time_tracking.estimated_hours set; "+
				"run `st update %s time_tracking estimated_hours <hours>` or re-create with --estimate, then re-approve.\n",
			id, id)
		return 2
	}

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

	// I-710 / I-933: the plan-review sub-agent is now OFF by default and runs
	// only when explicitly opted in via --review (Review=true). The audit in
	// I-933 found the mandatory cold re-explore never vetoed a plan; its
	// mechanizable value is now the deterministic hollow-AC linter, which runs
	// in the unconditional ValidatePlan gate below. When opted in, review runs
	// BEFORE the AC validator gates and a Reject/Feedback/engine error fails
	// closed (approval refused) — the gate is load-bearing for the
	// plan-before-code hook. Engine-nil (in-process tests) skips review.
	if opts.Engine != nil && opts.Review {
		if code := runPlanReview(s, cfg, id, item, *opts.Engine); code != 0 {
			return code
		}
		// Reload item in case the sub-agent's auto-fix mutated fields.
		if refreshed, ok := s.Get(id); ok {
			item = refreshed
		}
	} else if opts.Engine != nil && opts.BypassReview {
		// I-933: --bypass-review is a no-op now that review is off by default.
		// Keep a one-line deprecation notice so old CI/agent invocations are
		// nudged toward dropping the flag (or using --review to opt in).
		fmt.Fprintf(os.Stderr,
			"%s: --bypass-review is deprecated — the plan-review sub-agent is off by default (I-933); pass --review to opt in.\n", id)
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
		// I-991: always replace ACs from the canonical sidecar source rather
		// than write-once (old guard: len==0). Auto-fix sub-agents or retried
		// approvals may have left stale/duplicate ACs; the sidecar is the
		// truth. Dedup so N identical lines from repeated writes collapse.
		applyACs(it, draftACs)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	// Pre-warm model recommendation so `st start` model check resolves instantly.
	if opts.Engine != nil {
		stampModelRec(s, cfg, id, *opts.Engine)
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
	if err := autoSync(s, fmt.Sprintf("st plan approve: %s", id)); err != nil {
		return 1
	}
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
	if err := autoSync(s, fmt.Sprintf("st plan reset: %s", id)); err != nil {
		return 1
	}
	return 0
}

// PlanInvalidate discards the plan sidecar for `id` so the next
// `st plan prep <id>` re-authors it from scratch. This is the STRONG
// form of plan invalidation, distinct from PlanReset's weak form:
//
//   - PlanReset    — revokes the I-178 approval stamp; the plan body
//                    stays and just needs another `st plan approve`.
//   - PlanInvalidate — deletes the plan body (and report) entirely;
//                    the item becomes genuinely unplanned and
//                    `st plan prep` re-runs Claude against an empty
//                    slate.
//
// Used when an item's implementation approach fundamentally changes
// (I-767 — motivated by I-733's bash-hook → Go-subcommand
// re-architecture, where the old plan body was obsolete, not just
// pending re-approval).
//
// Refuses (exit 1) when there is nothing to invalidate — no sidecar,
// no report, and not plan-approved — mirroring PlanReset's
// "nothing to reset" guard.
func PlanInvalidate(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	plansDir := cfg.PlansDir()
	hadSidecar := plan.Exists(plansDir, id)
	hadReport := plan.ReportExists(plansDir, id)

	if !hadSidecar && !hadReport && !item.PlanApproved {
		fmt.Fprintf(os.Stderr, "%s has no plan sidecar, no report, and is not plan-approved — nothing to invalidate\n", id)
		return 1
	}

	sidecarRel := relativePlanPath(plansDir, cfg.Root(), id)

	if err := plan.Delete(plansDir, id); err != nil {
		fmt.Fprintf(os.Stderr, "deleting plan sidecar for %s: %v\n", id, err)
		return 1
	}
	if err := plan.DeleteReport(plansDir, id); err != nil {
		fmt.Fprintf(os.Stderr, "deleting plan report for %s: %v\n", id, err)
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
		// Drop the now-dangling sidecar path from linked_plans so the
		// I-512 invariant (linked_plans points at live plan content)
		// is not left referencing a deleted file.
		if len(it.LinkedPlans) > 0 {
			kept := it.LinkedPlans[:0]
			for _, lp := range it.LinkedPlans {
				if lp != sidecarRel {
					kept = append(kept, lp)
				}
			}
			it.LinkedPlans = kept
			it.Doc.ReplaceList("linked_plans", it.LinkedPlans)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	reason := "I-767 plan invalidate (sidecar discarded for re-authoring)"
	if priorBy != "" {
		reason = fmt.Sprintf("%s — was approved by %s at %s", reason, priorBy, priorAt)
	}
	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "plan_invalidate",
		OldValue: priorBy,
		Reason:   reason,
	})

	fmt.Printf("Invalidated plan for %s — sidecar discarded. Run `st plan prep %s` to author a fresh plan.\n", id, id)
	if err := autoSync(s, fmt.Sprintf("st plan invalidate: %s", id)); err != nil {
		return 1
	}
	return 0
}

// PlanCheck prints the approval state for `id` and exits with one of
// three distinct codes so callers (the plan-before-code-guard.sh hook)
// can emit targeted error messages:
//
//	0 → approved and all substance gates pass — allow Edit/Write.
//	1 → plan was never approved — "run st plan prep / st plan write".
//	3 → plan was approved but a substance gate is now failing — "fix
//	    the sidecar or run st plan reset" (I-897 distinguishes this
//	    from the "never approved" case so the hook message is accurate).
//
// Designed for `st plan check $ITEM_ID > /dev/null` so the hook can
// inspect only the exit code; stdout/stderr carry the human detail.
//
// I-589: re-validates the SBAR substance gate alongside PlanApproved.
// I-710 / I-716: re-validates plan substance and sidecar existence.
func PlanCheck(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if !item.PlanApproved {
		fmt.Fprintf(os.Stderr, "not approved\n")
		return 1
	}
	// I-897: all substance failures below return 3 (approved but failing)
	// so the hook can distinguish them from exit 1 (never approved).
	if item.Type == "task" || item.Type == "issue" {
		if vios := quality.ValidateSBAR(item); quality.HasError(vios) {
			fmt.Fprintf(os.Stderr,
				"approved but SBAR substance gate now failing — re-fill SBAR or run `st plan reset %s`\n", id)
			for _, v := range vios {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
			return 3
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
		return 3
	}
	if quality.HasError(vios) {
		fmt.Fprintf(os.Stderr,
			"approved but %d plan substance violation(s) — fix .plans/%s.md or run `st plan reset %s`\n",
			len(vios), id, id)
		for _, v := range vios {
			fmt.Fprintf(os.Stderr, "  %s\n", v)
		}
		return 3
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

// PlanWrite writes a plan body directly to .plans/<id>.md without
// spawning an exploration agent. This is the lightweight alternative
// to `st plan prep` for items where the SBAR already describes the
// implementation precisely and the agent has already explored the
// relevant source files (I-917).
//
// When selfApprove is true (--self-approve CLI flag), PlanWrite also
// calls PlanApprove with Engine:nil after writing. Engine:nil runs the
// SBAR substance gate and the AC verifiability gate (fast static
// checks, <1s each) and — if both pass — stamps PlanApproved on the
// item. The I-710 plan-review sub-agent is NOT spawned: that
// sub-agent is calibrated for plan-prep outputs (blind exploration);
// an agent using PlanWrite has already done that exploration itself.
// If any static gate fails, PlanApprove prints the specific gaps and
// returns a non-zero exit code so the agent can fix the plan inline
// and re-run (I-1092).
func PlanWrite(s *store.Store, cfg *config.Config, id string, body string, selfApprove bool) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	// Refuse to overwrite an already-approved plan sidecar without an
	// explicit reset first. The approval stamp (approver + timestamp)
	// would otherwise certify a plan body that no longer exists on disk,
	// creating a silent integrity violation. The --self-approve path has
	// the same problem: PlanApprove's idempotent guard would return 0
	// without re-validating the new body against any gate.
	// Resolution: if the item is already approved, require the caller to
	// run `st plan reset <id>` first, which clears the stamp and makes
	// PlanApprove re-run all gates normally.
	if item.PlanApproved {
		fmt.Fprintf(os.Stderr,
			"%s: plan is already approved (by %s at %s) — refusing overwrite.\n"+
				"Run `st plan reset %s` first to revoke approval, then re-run `st plan write`.\n",
			id, fallback(item.PlanApprovedBy, "?"), fallback(item.PlanApprovedAt, "?"), id)
		return 1
	}

	plansDir := cfg.PlansDir()
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "creating plans dir for %s: %v\n", id, err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(plansDir, id+".md"), []byte(body), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "writing plan for %s: %v\n", id, err)
		return 1
	}

	// Stamp linked_plans so the sidecar is discoverable by st plan show,
	// st resume, and the plan-before-code hook regardless of whether
	// --self-approve is used. PlanApprove's own back-fill is idempotent
	// (checks for duplicates) so calling it afterward is safe.
	sidecarRel := relativePlanPath(plansDir, cfg.Root(), id)
	if err := s.Mutate(id, func(it *model.Item) error {
		for _, lp := range it.LinkedPlans {
			if lp == sidecarRel {
				return nil
			}
		}
		it.LinkedPlans = append(it.LinkedPlans, sidecarRel)
		it.Doc.ReplaceList("linked_plans", it.LinkedPlans)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "linking plan for %s: %v\n", id, err)
		return 1
	}

	agentID := cfg.AgentID()
	if agentID == "" {
		agentID = "user"
	}
	_ = changelog.Append(cfg, id, changelog.Entry{
		Op:       "plan_write",
		NewValue: agentID,
		Reason:   "plan written directly via st plan write (no exploration agent)",
	})

	fmt.Printf("Wrote plan for %s (%d bytes)\n", id, len(body))

	if selfApprove {
		fmt.Fprintf(os.Stderr, "st plan write: running static gates (SBAR + AC verifiability; no review sub-agent)…\n")
		// Engine:nil skips the I-710 review sub-agent; SBAR substance
		// and AC verifiability gates still run. I-1092.
		// PlanApprove's idempotent guard is not triggered here because
		// we refused already-approved items above.
		return PlanApprove(s, cfg, id, PlanApproveOpts{Engine: nil})
	}

	if err := autoSync(s, fmt.Sprintf("st plan write: %s", id)); err != nil {
		return 1
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
