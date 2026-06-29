package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/plan"
	"github.com/theraprac/agent-state/internal/store"
)

// I-178: PlanApprove flips PlanApproved + sets audit fields + writes a
// changelog entry.
func TestPlanApproveHappyPath(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve returned %d", code)
	}

	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("PlanApproved should be true")
	}
	if item.PlanApprovedBy == "" {
		t.Error("PlanApprovedBy should be set")
	}
	if item.PlanApprovedAt == "" {
		t.Error("PlanApprovedAt should be set")
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "plan_approve" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected plan_approve entry in changelog")
	}
}

// I-832: re-approving an already-approved plan is idempotent — exits 0,
// preserves the original audit fields, and does not append a second
// changelog entry. This prevents the agent retry loop that occurred when
// autoSync silently failed and the next `st plan approve` call exited 1.
func TestPlanApproveIsIdempotentWhenAlreadyApproved(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("first approve: %d", code)
	}
	firstItem, _ := s.Get("T-001")
	firstBy := firstItem.PlanApprovedBy
	firstAt := firstItem.PlanApprovedAt

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Errorf("second approve (idempotent re-run) should return 0; got %d", code)
	}

	// Audit fields must be unchanged — the second call must not overwrite
	// the original approver/timestamp.
	secondItem, _ := s.Get("T-001")
	if secondItem.PlanApprovedBy != firstBy {
		t.Errorf("PlanApprovedBy changed on idempotent re-run: %q → %q", firstBy, secondItem.PlanApprovedBy)
	}
	if secondItem.PlanApprovedAt != firstAt {
		t.Errorf("PlanApprovedAt changed on idempotent re-run: %q → %q", firstAt, secondItem.PlanApprovedAt)
	}

	// Exactly one plan_approve changelog entry — the idempotent re-run
	// must not append a duplicate.
	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	var count int
	for _, e := range entries {
		if e.Op == "plan_approve" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 plan_approve changelog entry; got %d", count)
	}
}

// I-178: PlanReset clears the audit + flips Approved=false. Refuses if
// already not-approved (no-op safety).
func TestPlanResetRevertsApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := PlanReset(s, cfg, "T-001"); code != 0 {
		t.Fatalf("reset: %d", code)
	}
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should be false after reset")
	}
	if item.PlanApprovedBy != "" || item.PlanApprovedAt != "" {
		t.Error("audit fields should be cleared on reset")
	}
}

func TestPlanResetRefusesIfNotApproved(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanReset(s, cfg, "T-001"); code != 1 {
		t.Errorf("reset on unapproved item should fail; got %d", code)
	}
}

// I-178: PlanCheck exits 0 when approved, 1 when not — the hook contract.
func TestPlanCheckExitCodes(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanCheck(s, cfg, "T-001"); code != 1 {
		t.Errorf("check on unapproved should exit 1, got %d", code)
	}
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := PlanCheck(s, cfg, "T-001"); code != 0 {
		t.Errorf("check on approved should exit 0, got %d", code)
	}
}

func TestPlanCheckMissingItem(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanCheck(s, cfg, "T-999"); code != 1 {
		t.Errorf("check on missing item should exit 1, got %d", code)
	}
}

// I-178: round-trip the new schema fields through Mutate + reload so the
// hook can read PlanApprovedBy/At after the operator runs `st plan
// approve` from a different process.
func TestPlanApprovalPersistsAcrossReload(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}

	// Force a reload by walking the store fresh.
	out := captureStdout(t, func() { PlanShow(s, cfg, "T-001") })
	if !strings.Contains(out, "approved") {
		t.Errorf("show output missing approval line: %q", out)
	}
	if !strings.Contains(out, "user") {
		t.Errorf("show output missing approver: %q", out)
	}
}

// I-589: the SBAR substance gate is hard-blocking by default — no
// `--strict` opt-in. Seed a fresh empty-SBAR item to exercise the
// default-branch rejection. (The shared T-001 fixture now carries a
// populated SBAR per I-589, so it can't be used for the empty-branch
// assertion.)
func TestPlanApproveBlocksEmptySBARByDefault(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	seedEmptySBARTask(t, cfg, "T-901")
	s, _ = reloadStore(t, cfg) // pick up the new file

	if code := PlanApprove(s, cfg, "T-901", PlanApproveOpts{}); code != 2 {
		t.Errorf("default approve with empty SBAR should exit 2, got %d", code)
	}
	item, _ := s.Get("T-901")
	if item != nil && item.PlanApproved {
		t.Error("default approve should not have flipped PlanApproved on rejected item")
	}
}

// I-589: scaffold SBAR (the I-492 TODO placeholder strings) is treated
// the same as empty — the gate blocks. Without this, an author could
// `st create` a task and immediately `st plan approve` it before the
// I-588 sub-agent review has filled the SBAR, leaking scaffold through.
func TestPlanApproveBlocksScaffoldSBARByDefault(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	seedScaffoldSBARTask(t, cfg, "T-902")
	s, _ = reloadStore(t, cfg)

	if code := PlanApprove(s, cfg, "T-902", PlanApproveOpts{}); code != 2 {
		t.Errorf("default approve with scaffold SBAR should exit 2, got %d", code)
	}
}

// I-589: default approve with a fully populated SBAR succeeds. The
// baseline T-001 fixture now carries a real SBAR per the same item.
func TestPlanApprovePassesWithPopulatedSBARByDefault(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Errorf("default approve with populated SBAR should exit 0, got %d", code)
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("default approve with populated SBAR should flip PlanApproved")
	}
}

// I-589: `--strict` is preserved for backward compatibility but is now
// a no-op for SBAR (the SBAR gate runs unconditionally). The flag still
// governs the I-511 AC verifiability gate. Here we verify that --strict
// behaves identically to default for the SBAR rejection — same exit code.
func TestPlanApproveStrictFlagAcceptedAsNoopForSBAR(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	seedEmptySBARTask(t, cfg, "T-903")
	seedEmptySBARTask(t, cfg, "T-904")
	s, _ = reloadStore(t, cfg)

	defaultCode := PlanApprove(s, cfg, "T-903", PlanApproveOpts{})
	strictCode := PlanApprove(s, cfg, "T-904", PlanApproveOpts{Strict: true})
	if defaultCode != strictCode || defaultCode != 2 {
		t.Errorf("--strict should be a no-op for SBAR: default=%d strict=%d (both should be 2)", defaultCode, strictCode)
	}
}

// I-589: ideas and promotions don't carry SBAR per the I-487 schema,
// so the substance gate is skipped entirely for those types. Seed a
// fresh idea with no SBAR and verify the approve succeeds.
func TestPlanApproveSkipsSBARGateForIdeaAndPromotion(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	seedIdeaWithoutSBAR(t, cfg, "ID-901")
	s, _ = reloadStore(t, cfg)

	if code := PlanApprove(s, cfg, "ID-901", PlanApproveOpts{}); code != 0 {
		t.Errorf("idea approve with no SBAR should exit 0 (gate skipped); got %d", code)
	}
}

// I-589 / hook contract: `st plan check` (called by
// plan-before-code-guard.sh) blocks when an approved item's SBAR has
// been knocked back to the I-492 scaffold (e.g., by a direct-file
// edit). Without this re-validation, the hook would keep allowing
// Edit/Write after the SBAR was emptied.
func TestPlanCheckBlocksOnIncompleteSBAR(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("baseline approve: %d", code)
	}
	if code := PlanCheck(s, cfg, "T-001"); code != 0 {
		t.Fatalf("check on approved+populated should exit 0; got %d", code)
	}

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SBAR = model.SBAR{
			Situation:      model.SBARPlaceholders["situation"],
			Background:     model.SBARPlaceholders["background"],
			Assessment:     model.SBARPlaceholders["assessment"],
			Recommendation: model.SBARPlaceholders["recommendation"],
		}
		it.Doc.SetSBARBlock(it.SBAR)
		return nil
	}); err != nil {
		t.Fatalf("revert SBAR: %v", err)
	}
	s, _ = reloadStore(t, cfg) // re-read from disk so PlanCheck sees the new SBAR

	// I-897: distinct exit 3 (approved but failing) vs exit 1 (never approved)
	if code := PlanCheck(s, cfg, "T-001"); code != 3 {
		t.Errorf("check after SBAR knocked back to scaffold should exit 3; got %d", code)
	}
}

// I-897: PlanCheck exits 3 (not 1) when plan_approved=true but plan
// sidecar is missing — "approved but substance now failing".
func TestPlanCheckExits3WhenSidecarMissingAfterApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("baseline approve: %d", code)
	}
	if code := PlanCheck(s, cfg, "T-001"); code != 0 {
		t.Fatalf("check on approved+populated should exit 0; got %d", code)
	}

	// Delete the sidecar to trigger the missing-sidecar substance path.
	sidecarPath := cfg.PlansDir() + "/T-001.md"
	if err := os.Remove(sidecarPath); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	s, _ = reloadStore(t, cfg)

	// I-897: must be 3, not 1 — item is approved but gate now failing.
	if code := PlanCheck(s, cfg, "T-001"); code != 3 {
		t.Errorf("check after sidecar deletion should exit 3; got %d", code)
	}
}

// I-589: PlanCheck on an approved idea succeeds even with no SBAR.
func TestPlanCheckSkipsSBARGateForIdeaAndPromotion(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	seedIdeaWithoutSBAR(t, cfg, "ID-902")
	s, _ = reloadStore(t, cfg)

	if code := PlanApprove(s, cfg, "ID-902", PlanApproveOpts{}); code != 0 {
		t.Fatalf("idea approve: %d", code)
	}
	if code := PlanCheck(s, cfg, "ID-902"); code != 0 {
		t.Errorf("check on approved idea (no SBAR) should exit 0; got %d", code)
	}
}

// reloadStore rebuilds the store from disk so newly-written fixture
// files are picked up. Used after seedEmpty/Scaffold/Idea helpers that
// drop raw YAML files outside the store's mutate path.
func reloadStore(t *testing.T, cfg *config.Config) (*store.Store, error) {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	return s, nil
}

// seedEmptySBARTask drops a task file at <id>-empty.md with no SBAR
// block. Used by the negative-path approve/check tests.
func seedEmptySBARTask(t *testing.T, cfg *config.Config, id string) {
	t.Helper()
	path := filepath.Join(cfg.Root(), "tasks", id+"-empty.md")
	writeFile(t, path, `id: `+id+`
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Empty SBAR fixture

depends_on:
- []

next_actions:
- []
`)
}

// seedScaffoldSBARTask drops a task file whose SBAR sub-fields all
// hold the literal I-492 TODO placeholder strings. The substance gate
// must treat these as equivalent to empty.
func seedScaffoldSBARTask(t *testing.T, cfg *config.Config, id string) {
	t.Helper()
	path := filepath.Join(cfg.Root(), "tasks", id+"-scaffold.md")
	writeFile(t, path, `id: `+id+`
type: task
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

completed: null

title: Scaffold SBAR fixture

depends_on:
- []

next_actions:
- []

sbar:
  situation: |-
    `+model.SBARPlaceholders["situation"]+`
  background: |-
    `+model.SBARPlaceholders["background"]+`
  assessment: |-
    `+model.SBARPlaceholders["assessment"]+`
  recommendation: |-
    `+model.SBARPlaceholders["recommendation"]+`
`)
}

// seedIdeaWithoutSBAR drops an idea file with no SBAR.
func seedIdeaWithoutSBAR(t *testing.T, cfg *config.Config, id string) {
	t.Helper()
	path := filepath.Join(cfg.Root(), "ideas", id+"-no-sbar.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `id: `+id+`
type: idea
status: captured
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00

title: Idea without SBAR
`)
}

// I-565: when a plan sidecar exists, PlanApprove back-fills the I-512
// linked_plans field on the item — even when the plan was generated
// via `st prep --write-only`, which doesn't mutate the item itself.
// Without this, write-only items would permanently have linked_plans:
// [], breaking the plan-before-code hook / st prime correlation.
func TestPlanApproveStampsLinkedPlansFromSidecar(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Drop a draft plan sidecar at .plans/T-001.md, the way prepItemWriteOnly does.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Test approach.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar: %v", err)
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}

	item, _ := s.Get("T-001")
	if len(item.LinkedPlans) == 0 {
		t.Fatal("PlanApprove should have stamped linked_plans from the sidecar")
	}
	want := relativePlanPath(cfg.PlansDir(), cfg.Root(), "T-001")
	found := false
	for _, lp := range item.LinkedPlans {
		if lp == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("linked_plans = %v; want it to contain %q", item.LinkedPlans, want)
	}
}

// I-149: once the SBAR is fully populated, --strict approves cleanly.
// AC verifiability is independent (I-511) and not exercised here —
// the test fixture has no plan sidecar so the I-511 path is a no-op.
func TestPlanApproveStrict_PassesWithPopulatedSBAR(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.SBAR = model.SBAR{
			Situation:      "real",
			Background:     "real",
			Assessment:     "real",
			Recommendation: "real",
		}
		it.Doc.SetSBARBlock(it.SBAR)
		return nil
	}); err != nil {
		t.Fatalf("seeding SBAR: %v", err)
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{Strict: true}); code != 0 {
		t.Errorf("strict approve with populated SBAR should exit 0, got %d", code)
	}
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Error("populated SBAR should pass strict gate and flip PlanApproved")
	}
}

// I-767: PlanInvalidate deletes the sidecar so the item becomes
// plan-prep-eligible again. setupTestEnv seeds T-001 with a sidecar.
func TestPlanInvalidateDeletesSidecar(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if !plan.Exists(cfg.PlansDir(), "T-001") {
		t.Fatal("fixture should seed a T-001 sidecar")
	}
	if code := PlanInvalidate(s, cfg, "T-001"); code != 0 {
		t.Fatalf("invalidate returned %d", code)
	}
	if plan.Exists(cfg.PlansDir(), "T-001") {
		t.Error("sidecar should be gone after invalidate")
	}
}

// I-767: invalidate clears the approval stamp + audit fields, the
// same fields PlanReset clears.
func TestPlanInvalidateClearsApproval(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if code := PlanInvalidate(s, cfg, "T-001"); code != 0 {
		t.Fatalf("invalidate: %d", code)
	}
	item, _ := s.Get("T-001")
	if item.PlanApproved {
		t.Error("PlanApproved should be false after invalidate")
	}
	if item.PlanApprovedBy != "" || item.PlanApprovedAt != "" {
		t.Error("audit fields should be cleared on invalidate")
	}
}

// I-767: invalidate refuses (exit 1) when there is nothing to
// invalidate — no sidecar, no report, not approved.
func TestPlanInvalidateRefusesWhenNothing(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Remove the seeded sidecar so T-001 has nothing to invalidate.
	if err := plan.Delete(cfg.PlansDir(), "T-001"); err != nil {
		t.Fatalf("pre-delete sidecar: %v", err)
	}
	if code := PlanInvalidate(s, cfg, "T-001"); code != 1 {
		t.Errorf("invalidate with nothing to do should exit 1; got %d", code)
	}
}

// I-767: invalidate also removes the .report.md sidecar.
func TestPlanInvalidateDeletesReport(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := plan.SaveReport(cfg.PlansDir(), "T-001", "review narrative"); err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	if code := PlanInvalidate(s, cfg, "T-001"); code != 0 {
		t.Fatalf("invalidate: %d", code)
	}
	if plan.ReportExists(cfg.PlansDir(), "T-001") {
		t.Error("report should be gone after invalidate")
	}
}

// I-767: invalidate writes an auditable plan_invalidate changelog entry.
func TestPlanInvalidateWritesChangelog(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanInvalidate(s, cfg, "T-001"); code != 0 {
		t.Fatalf("invalidate: %d", code)
	}
	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("read changelog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Op == "plan_invalidate" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected plan_invalidate entry in changelog")
	}
}

// I-767: invalidate drops the now-dangling sidecar path from
// linked_plans so the I-512 invariant is not left referencing a
// deleted file. PlanApprove stamps linked_plans.
func TestPlanInvalidateRemovesLinkedPlan(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}
	if item, _ := s.Get("T-001"); len(item.LinkedPlans) == 0 {
		t.Fatal("approve should stamp linked_plans")
	}
	if code := PlanInvalidate(s, cfg, "T-001"); code != 0 {
		t.Fatalf("invalidate: %d", code)
	}
	item, _ := s.Get("T-001")
	for _, lp := range item.LinkedPlans {
		if strings.Contains(lp, "T-001") {
			t.Errorf("linked_plans should not retain the invalidated sidecar; got %v", item.LinkedPlans)
		}
	}
}

// I-767: invalidate on an unknown item id exits 1.
func TestPlanInvalidateNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if code := PlanInvalidate(s, cfg, "T-999"); code != 1 {
		t.Errorf("invalidate on missing item should exit 1; got %d", code)
	}
}

// I-991: PlanApprove must replace an item's existing acceptance_criteria with
// the canonical sidecar ACs, not skip the write because ACs are non-empty.
// Regression: the old guard `if len(it.AcceptanceCriteria) == 0` meant a
// re-approve after an auto-fix sub-agent wrote bad ACs left the stale ACs in
// place permanently.
func TestPlanApproveReplacesExistingACs(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Pre-seed T-001 with stale ACs that differ from the sidecar.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.AcceptanceCriteria = []string{"cmd: old-stale-command"}
		it.Doc.ReplaceList("acceptance_criteria", it.AcceptanceCriteria)
		return nil
	}); err != nil {
		t.Fatalf("seeding stale ACs: %v", err)
	}
	item, _ := s.Get("T-001")
	if len(item.AcceptanceCriteria) != 1 || item.AcceptanceCriteria[0] != "cmd: old-stale-command" {
		t.Fatalf("fixture AC seed failed: %v", item.AcceptanceCriteria)
	}

	// The fixture sidecar has ACs: ["cmd: go test ./..."]. After approve,
	// the item must carry the sidecar ACs, not the pre-seeded stale ones.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}

	// Load sidecar ACs to use as the expected value — avoids hard-coding the
	// fixture string so the test stays correct if setupTestEnv's sidecar changes.
	fixturePlan, err := plan.Load(cfg.PlansDir(), "T-001")
	if err != nil {
		t.Fatalf("loading fixture sidecar: %v", err)
	}
	if len(fixturePlan.ACs) == 0 {
		t.Fatal("fixture sidecar has no ACs to assert against")
	}

	item, _ = s.Get("T-001")
	if len(item.AcceptanceCriteria) != len(fixturePlan.ACs) {
		t.Fatalf("expected %d AC(s) after approve; got %d: %v", len(fixturePlan.ACs), len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	if item.AcceptanceCriteria[0] != fixturePlan.ACs[0] {
		t.Errorf("AC should be the sidecar value %q; got %q", fixturePlan.ACs[0], item.AcceptanceCriteria[0])
	}

	// Reload from disk to verify the on-disk format is correct (not just in-memory).
	// This catches the ReplaceList "- " prefix bug where in-memory looks right
	// but the serialised YAML is malformed.
	s2, _ := reloadStore(t, cfg)
	item2, _ := s2.Get("T-001")
	if len(item2.AcceptanceCriteria) != len(fixturePlan.ACs) {
		t.Fatalf("disk reload: expected %d AC(s); got %d: %v", len(fixturePlan.ACs), len(item2.AcceptanceCriteria), item2.AcceptanceCriteria)
	}
	if item2.AcceptanceCriteria[0] != fixturePlan.ACs[0] {
		t.Errorf("disk reload: AC should be %q; got %q", fixturePlan.ACs[0], item2.AcceptanceCriteria[0])
	}
}

// I-991: PlanApprove deduplicates ACs from the sidecar so that N identical
// lines from repeated writes (e.g. timeout-retry loops) collapse to one.
func TestPlanApproveDeduplicatesACs(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Override the fixture sidecar with duplicate AC entries.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Fixture plan with duplicate ACs.",
		ScopeRepos: []string{"as"},
		ACs: []string{
			"cmd: go test ./...",
			"cmd: go test ./...",
			"cmd: go build ./...",
		},
		Tests:      "Covered by existing test suite.",
		OutOfScope: "None",
		Risks:      "Low risk.",
	}); err != nil {
		t.Fatalf("seeding sidecar with duplicate ACs: %v", err)
	}

	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("approve: %d", code)
	}

	item, _ := s.Get("T-001")
	if len(item.AcceptanceCriteria) != 2 {
		t.Fatalf("expected 2 deduplicated ACs; got %d: %v", len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	if item.AcceptanceCriteria[0] != "cmd: go test ./..." || item.AcceptanceCriteria[1] != "cmd: go build ./..." {
		t.Errorf("unexpected ACs after dedup: %v", item.AcceptanceCriteria)
	}
}

// I-821: PlanApprove must exit non-zero when the I-807 gate fires, including
// the I-832 idempotent re-run path (a retry must not silently succeed).
func TestPlanApproveExitsNonZeroOnGateRefusal(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	workspace, s, cfg := setupGateWorkspace(t)

	// Save a minimal plan sidecar so the missing-sidecar gate passes.
	if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
		Approach:   "Test plan for gate refusal test.",
		ScopeRepos: []string{"as"},
		ACs:        []string{"cmd: go test ./..."},
	}); err != nil {
		t.Fatalf("plan.Save: %v", err)
	}

	// Arm the I-807 gate by dirtying the tracked non-state file.
	if err := os.WriteFile(filepath.Join(workspace, "claude-config", "hooks", "foo.sh"),
		[]byte("#!/bin/sh\necho gate-armed\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// First call: must fail because the gate fires after approve writes the item.
	code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{})
	if code == 0 {
		t.Errorf("PlanApprove must return non-zero when the I-807 gate fires; got 0")
	}

	// I-832 idempotent re-run: even if PlanApproved was set on disk, a retry
	// while the gate is still firing must also return non-zero.
	code2 := PlanApprove(s, cfg, "T-001", PlanApproveOpts{})
	if code2 == 0 {
		t.Errorf("PlanApprove idempotent re-run must also fail while gate is active; got 0")
	}
}

// I-1649: idempotent guard in PlanApprove must refresh stale ACs from the sidecar.
//
// Scenario: an interactive `st plan prep` accept stamped plan_approved=true with the
// old write-once guard (len==0), leaving stale ACs on the item. A subsequent
// `st plan approve` must hit the idempotent guard AND still replace ACs from the
// sidecar before returning 0.
func TestPlanApproveIdempotentGuardRefreshesStaleACs(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)

	// Simulate an old interactive-accept: stamp plan_approved=true with stale ACs
	// (as if the len==0 guard ran when item already had ACs from a prior prep).
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.PlanApproved = true
		it.PlanApprovedAt = "2026-01-01T00:00:00Z"
		it.PlanApprovedBy = "agent-x"
		it.Doc.SetField("plan_approved", "true")
		it.Doc.SetField("plan_approved_at", "2026-01-01T00:00:00Z")
		it.Doc.SetField("plan_approved_by", "agent-x")
		it.AcceptanceCriteria = []string{"cmd: stale-old-command"}
		it.Doc.ReplaceList("acceptance_criteria", []string{"- cmd: stale-old-command"})
		return nil
	}); err != nil {
		t.Fatalf("seeding pre-approved stale state: %v", err)
	}

	// Confirm stale ACs are on the item before the idempotent re-run.
	item, _ := s.Get("T-001")
	if !item.PlanApproved {
		t.Fatal("fixture: expected plan_approved=true")
	}
	if len(item.AcceptanceCriteria) != 1 || item.AcceptanceCriteria[0] != "cmd: stale-old-command" {
		t.Fatalf("fixture: expected stale AC; got %v", item.AcceptanceCriteria)
	}

	// The sidecar has the canonical ACs; load them to know what to assert.
	fixturePlan, err := plan.Load(cfg.PlansDir(), "T-001")
	if err != nil {
		t.Fatalf("loading fixture sidecar: %v", err)
	}
	if len(fixturePlan.ACs) == 0 {
		t.Fatal("fixture sidecar has no ACs to assert against")
	}

	// Idempotent re-run: PlanApprove must return 0 AND replace the stale ACs.
	if code := PlanApprove(s, cfg, "T-001", PlanApproveOpts{}); code != 0 {
		t.Fatalf("idempotent re-run: expected exit 0; got %d", code)
	}

	item, _ = s.Get("T-001")
	if len(item.AcceptanceCriteria) != len(fixturePlan.ACs) {
		t.Fatalf("idempotent guard: expected %d sidecar AC(s); got %d: %v",
			len(fixturePlan.ACs), len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	if item.AcceptanceCriteria[0] == "cmd: stale-old-command" {
		t.Errorf("idempotent guard: stale AC was not replaced; want %q got %q",
			fixturePlan.ACs[0], item.AcceptanceCriteria[0])
	}
	if item.AcceptanceCriteria[0] != fixturePlan.ACs[0] {
		t.Errorf("idempotent guard: AC should be sidecar value %q; got %q",
			fixturePlan.ACs[0], item.AcceptanceCriteria[0])
	}

	// Reload from disk to verify on-disk serialisation is also correct.
	s2, _ := reloadStore(t, cfg)
	item2, _ := s2.Get("T-001")
	if len(item2.AcceptanceCriteria) != len(fixturePlan.ACs) {
		t.Fatalf("disk reload: expected %d AC(s); got %d: %v",
			len(fixturePlan.ACs), len(item2.AcceptanceCriteria), item2.AcceptanceCriteria)
	}
	if item2.AcceptanceCriteria[0] != fixturePlan.ACs[0] {
		t.Errorf("disk reload: AC should be %q; got %q", fixturePlan.ACs[0], item2.AcceptanceCriteria[0])
	}
}

// applyACs is the shared dedup helper used by PlanApprove, the idempotent guard,
// and prep.go's Accept path. These tests guard its contract so a format change in
// one site doesn't silently diverge from the others.
func TestApplyACs_DedupsAndPrefixes(t *testing.T) {
	s, _ := setupTestEnv(t)
	item, _ := s.Get("T-001")

	applyACs(item, []string{"cmd: echo a", "cmd: echo b", "cmd: echo a"})

	if len(item.AcceptanceCriteria) != 2 {
		t.Fatalf("expected 2 deduped ACs; got %d: %v", len(item.AcceptanceCriteria), item.AcceptanceCriteria)
	}
	if item.AcceptanceCriteria[0] != "cmd: echo a" || item.AcceptanceCriteria[1] != "cmd: echo b" {
		t.Errorf("unexpected ACs: %v", item.AcceptanceCriteria)
	}
}

func TestApplyACs_EmptyInputIsNoop(t *testing.T) {
	s, _ := setupTestEnv(t)
	item, _ := s.Get("T-001")
	before := append([]string(nil), item.AcceptanceCriteria...)

	applyACs(item, nil)

	if len(item.AcceptanceCriteria) != len(before) {
		t.Errorf("applyACs(nil) must not modify AcceptanceCriteria; before=%v after=%v", before, item.AcceptanceCriteria)
	}
}

func TestApplyACs_ReplacesExistingNonEmpty(t *testing.T) {
	s, _ := setupTestEnv(t)
	item, _ := s.Get("T-001")
	item.AcceptanceCriteria = []string{"cmd: stale"}

	applyACs(item, []string{"cmd: fresh"})

	if len(item.AcceptanceCriteria) != 1 || item.AcceptanceCriteria[0] != "cmd: fresh" {
		t.Errorf("applyACs must replace non-empty ACs; got %v", item.AcceptanceCriteria)
	}
}
