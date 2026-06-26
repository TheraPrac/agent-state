package command

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/plan"
	"github.com/theraprac/agent-state/internal/registry"
	"github.com/theraprac/agent-state/internal/store"
)

// TestRelativePlanPath verifies the I-512 helper that records the plan
// sidecar path on item.LinkedPlans. Round-tripability requires the
// relative form so the value doesn't drift across machines / agents.
func TestRelativePlanPath(t *testing.T) {
	tests := []struct {
		name      string
		plansDir  string
		root      string
		itemID    string
		want      string
		wantAbs   bool // expect absolute (helper falls back when Rel fails)
	}{
		{
			name:     "simple subdir",
			plansDir: "/repo/.plans",
			root:     "/repo",
			itemID:   "I-509",
			want:     filepath.Join(".plans", "I-509.md"),
		},
		{
			name:     "nested plans dir",
			plansDir: "/repo/agent-state/.plans",
			root:     "/repo",
			itemID:   "T-100",
			want:     filepath.Join("agent-state", ".plans", "T-100.md"),
		},
		{
			name:     "empty root falls back to abs",
			plansDir: "/some/abs/.plans",
			root:     "",
			itemID:   "I-200",
			want:     filepath.Join("/some/abs/.plans", "I-200.md"),
			wantAbs:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativePlanPath(tt.plansDir, tt.root, tt.itemID)
			if got != tt.want {
				t.Errorf("relativePlanPath(%q,%q,%q) = %q, want %q",
					tt.plansDir, tt.root, tt.itemID, got, tt.want)
			}
			// Sanity: relative cases shouldn't start with / on POSIX.
			if !tt.wantAbs && strings.HasPrefix(got, "/") {
				t.Errorf("expected relative path; got absolute %q", got)
			}
		})
	}
}

// --- I-558: maybeAutoApproveSprintPlan ---

// menuRecorder counts how many times the auto-approve sprint-plan
// gate was invoked, distinguished from per-item review menus by the
// "Sprint Plan Review" string in the prompt the gate renders. Tests
// assert on `sprintGateCalls` to verify the helper fired (or didn't).
type menuRecorder struct {
	mu              sync.Mutex
	sprintGateCalls int
	otherMenuCalls  int
	menuChoice      string // returned by SelectMenu when sprint gate fires
}

func (r *menuRecorder) engine() RunEngine {
	return RunEngine{
		RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
			return []byte(`{"type":"result","subtype":"success","result":""}`), 0, nil
		},
		PromptUser: func(p string) (string, error) {
			return "", nil
		},
		SelectMenu: func(p string, opts []menuOption, def int) string {
			r.mu.Lock()
			defer r.mu.Unlock()
			// showReviewGate calls SelectMenu with prompt="" — distinguish
			// the sprint-plan gate from the per-item plan-review gate by
			// inspecting the option labels. The sprint gate uses
			// "approve sprint for execution"; the per-item plan-review
			// gate uses "save plan and proceed".
			isSprintGate := false
			for _, o := range opts {
				if strings.Contains(o.Label, "approve sprint for execution") {
					isSprintGate = true
					break
				}
			}
			if isSprintGate {
				r.sprintGateCalls++
				if r.menuChoice != "" {
					return r.menuChoice
				}
				if len(opts) > 0 {
					return opts[0].Key
				}
				return ""
			}
			// Non-sprint menus (per-item plan review, etc.): pick the
			// option whose label starts with "Reject" so a stray
			// prepItem invocation in a test fixture can't accidentally
			// stamp plan_approved on an item the test didn't intend to
			// approve, AND can't fall through to the "Interactive"
			// escape hatch (which would exec the real claude binary
			// and hang the test). We deliberately do NOT return the
			// last option — the per-item plan-review menu's last
			// option is Interactive, not Reject.
			r.otherMenuCalls++
			for _, o := range opts {
				if strings.HasPrefix(strings.TrimSpace(o.Label), "Reject") {
					return o.Key
				}
			}
			if len(opts) > 0 {
				return opts[0].Key
			}
			return ""
		},
		ConfirmPrompt: func(p string) bool { return false },
	}
}

// prepStampSprintItems adds items to a sprint via the registry and
// marks the subset in `approved` as plan_approved=true on both the item
// (item.PlanApproved field) and the plan sidecar (so the Prep loop's
// `unplanned` filter skips them and the `maybeAutoApproveSprintPlan`
// helper sees a fully-prepped sprint). Returns nothing — tests reload
// the store via the existing s parameter.
func prepStampSprintItems(t *testing.T, s *store.Store, cfg interface {
	cfgPathProvider
	PlansDir() string
	Root() string
}, sprintID string, items, approved []string) {
	t.Helper()

	reg, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	idx := -1
	for i := range reg.Sprints {
		if reg.Sprints[i].ID == sprintID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("sprint not found: %s", sprintID)
	}
	reg.Sprints[idx].Items = append(reg.Sprints[idx].Items, items...)
	if err := reg.Save(cfg.EpicsPath()); err != nil {
		t.Fatalf("registry.Save: %v", err)
	}

	approvedSet := make(map[string]bool, len(approved))
	for _, id := range approved {
		approvedSet[id] = true
	}
	for _, id := range items {
		thisID := id
		approveThis := approvedSet[thisID]
		if err := s.Mutate(thisID, func(item *model.Item) error {
			item.Sprint = sprintID
			item.Doc.SetField("sprint", sprintID)
			if approveThis {
				item.PlanApproved = true
				item.PlanApprovedAt = "2026-05-09T10:00:00-06:00"
				item.PlanApprovedBy = "test"
				item.Doc.SetField("plan_approved", "true")
				item.Doc.SetField("plan_approved_at", "2026-05-09T10:00:00-06:00")
				item.Doc.SetField("plan_approved_by", "test")
			}
			return nil
		}); err != nil {
			t.Fatalf("store.Mutate(%s): %v", thisID, err)
		}

		// For approved items, also drop a sidecar plan with Approved=true
		// so the Prep main loop's `unplanned` filter skips them — without
		// this, prepItem would run for every item and the auto-approve
		// helper never sees a fully-prepped sprint in the
		// "All items in sprint are already planned" early-return path.
		if approveThis {
			p := &plan.Plan{
				Approach:   "test approach",
				ScopeRepos: []string{"as"},
				ACs:        []string{"cmd: echo ok"},
				Approved:   true,
				ApprovedAt: "2026-05-09T10:00:00-06:00",
			}
			if err := plan.Save(cfg.PlansDir(), thisID, p); err != nil {
				t.Fatalf("plan.Save(%s): %v", thisID, err)
			}
		}
	}
}

// cfgPathProvider lets test helpers accept either *config.Config or any
// shim with the same shape — just narrows surface to what we actually
// touch.
type cfgPathProvider interface {
	EpicsPath() string
}

// I-558: when every non-terminal item in the sprint is plan_approved
// and the operator chooses Accept on the gate, the sprint flips to
// PlanApproved=true and the registry is saved.
func TestPrepAutoApprovesSprintWhenAllItemsPlanned(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	rec := &menuRecorder{menuChoice: "1"}
	code := Prep(s, cfg, sprintID, PrepOpts{}, rec.engine())
	if code != 0 {
		t.Fatalf("Prep returned %d, want 0", code)
	}
	if rec.sprintGateCalls == 0 {
		t.Error("SelectMenu should have been invoked for the auto-approve gate")
	}

	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if !sp.PlanApproved {
		t.Error("sprint PlanApproved should be true after Accept")
	}
	if sp.PlanApprovedBy == "" {
		t.Error("sprint PlanApprovedBy should be set")
	}
	if sp.PlanApprovedAt == "" {
		t.Error("sprint PlanApprovedAt should be set")
	}
}

// I-558: when at least one non-terminal item lacks plan_approved, the
// auto-approve helper short-circuits before showing the gate and the
// sprint stays unapproved.
func TestPrepSkipsAutoApproveWhenItemUnprepped(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001"})

	rec := &menuRecorder{menuChoice: "1"}
	_ = Prep(s, cfg, sprintID, PrepOpts{}, rec.engine())

	if rec.sprintGateCalls != 0 {
		t.Errorf("SelectMenu should not be invoked when items are unprepped; got %d", rec.sprintGateCalls)
	}
	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if sp.PlanApproved {
		t.Error("sprint should not be approved when items remain unprepped")
	}
}

// I-558: --item filter implies a single-item retry; never trigger the
// sprint-level auto-approve gate in that mode.
func TestPrepItemFilterSkipsAutoApprove(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	rec := &menuRecorder{menuChoice: "1"}
	_ = Prep(s, cfg, sprintID, PrepOpts{ItemFilter: "T-001"}, rec.engine())

	if rec.sprintGateCalls != 0 {
		t.Errorf("SelectMenu should not be invoked when --item is set; got %d", rec.sprintGateCalls)
	}
	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if sp.PlanApproved {
		t.Error("sprint should not be auto-approved under --item filter")
	}
}

// I-558: --dry-run is read-only; never write a sprint plan-approval.
func TestPrepDryRunSkipsAutoApprove(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	rec := &menuRecorder{menuChoice: "1"}
	_ = Prep(s, cfg, sprintID, PrepOpts{DryRun: true}, rec.engine())

	if rec.sprintGateCalls != 0 {
		t.Errorf("SelectMenu should not be invoked under --dry-run; got %d", rec.sprintGateCalls)
	}
	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if sp.PlanApproved {
		t.Error("sprint should not be approved under --dry-run")
	}
}

// I-558: idempotent — when the sprint is already plan-approved, the
// helper should be a no-op (no menu, no double-write).
func TestPrepSkipsWhenSprintAlreadyApproved(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	// Pre-approve the sprint.
	reg, _ := registry.Load(cfg.EpicsPath())
	for i := range reg.Sprints {
		if reg.Sprints[i].ID == sprintID {
			reg.Sprints[i].PlanApproved = true
			reg.Sprints[i].PlanApprovedBy = "previous"
			reg.Sprints[i].PlanApprovedAt = "2026-05-08T10:00:00-06:00"
		}
	}
	if err := reg.Save(cfg.EpicsPath()); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := &menuRecorder{menuChoice: "1"}
	_ = Prep(s, cfg, sprintID, PrepOpts{}, rec.engine())

	if rec.sprintGateCalls != 0 {
		t.Errorf("already-approved sprint should not show menu; got %d", rec.sprintGateCalls)
	}
	reg2, _ := registry.Load(cfg.EpicsPath())
	sp2, _ := reg2.SprintByID(sprintID)
	if sp2.PlanApprovedBy != "previous" {
		t.Errorf("approver should be unchanged; got %q", sp2.PlanApprovedBy)
	}
}

// I-558: --write-only must not trigger the interactive sprint-plan
// gate (it's the agent-drivable path; approval is a separate step).
func TestPrepWriteOnlySkipsAutoApprove(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	rec := &menuRecorder{menuChoice: "1"}
	_ = Prep(s, cfg, sprintID, PrepOpts{WriteOnly: true}, rec.engine())

	if rec.sprintGateCalls != 0 {
		t.Errorf("--write-only must not invoke the auto-approve gate; got %d", rec.sprintGateCalls)
	}
	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if sp.PlanApproved {
		t.Error("--write-only must not set sprint PlanApproved")
	}
}

// I-558: the standalone `st sprint plan` command remains advisory —
// it must not flip PlanApproved on its own.
func TestSprintPlanStandaloneDoesNotApprove(t *testing.T) {
	s, cfg, _, sprintID := setupSprintTestEnv(t)
	prepStampSprintItems(t, s, cfg, sprintID, []string{"T-001", "T-002"}, []string{"T-001", "T-002"})

	if code := SprintPlan(s, cfg, sprintID); code != 0 {
		t.Fatalf("SprintPlan returned %d, want 0", code)
	}
	reg, _ := registry.Load(cfg.EpicsPath())
	sp, _ := reg.SprintByID(sprintID)
	if sp.PlanApproved {
		t.Error("standalone SprintPlan should remain advisory and not set PlanApproved")
	}
}
