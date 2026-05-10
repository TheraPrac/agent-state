package command

import (
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// fullStackPlan returns a Plan classifying as full-stack with 6 ACs
// (above the default threshold of 5). Used by every TestSplit case.
func fullStackPlan() *plan.Plan {
	return &plan.Plan{
		Approach:   "Cross-cutting full-stack change",
		ScopeRepos: []string{"theraprac-api", "theraprac-web"},
		Approved:   false,
		ACs: []string{
			"cmd: cd ../theraprac-api && make integration-local",
			"cmd: cd ../theraprac-api && make test-unit",
			"cmd: go test ./internal/handlers/...",
			"cmd: cd ../theraprac-web && npm run type-check",
			"cmd: cd ../theraprac-web && npm run test:unit",
			"cmd: cd ../theraprac-web && npx playwright test foo.spec.ts",
		},
		FilesToCreate: []string{
			"theraprac-api/internal/handlers/foo.go",
			"theraprac-web/src/components/Foo.tsx",
		},
		FilesToModify: []string{
			"theraprac-api/api/openapi/api.yaml",
			"theraprac-web/src/lib/api/foo.ts",
		},
	}
}

// seedSplitParent writes a full-stack plan sidecar for T-001 and
// fills its SBAR so the split's lineage note has something to append
// to. Returns the store ready for Split().
func seedSplitParent(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupTestEnv(t)
	if err := plan.Save(cfg.PlansDir(), "T-001", fullStackPlan()); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.SBAR = model.SBAR{
			Situation:      "full-stack item",
			Background:     "spans api + web",
			Assessment:     "high cost",
			Recommendation: "split",
		}
		item.Doc.SetSBARBlock(item.SBAR)
		return nil
	}); err != nil {
		t.Fatalf("seed sbar: %v", err)
	}
	return s, cfg
}

func TestSplit(t *testing.T) {
	t.Run("part_b_depends_on_part_a", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		idA, idB, err := Split(s, cfg, "T-001")
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		fresh, _ := store.New(cfg)
		b, _ := fresh.Get(idB)
		if len(b.DependsOn) != 1 || b.DependsOn[0] != idA {
			t.Errorf("Part B depends_on = %v, want [%s]", b.DependsOn, idA)
		}
	})

	t.Run("parent_scope_flags_split", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		_, _, err := Split(s, cfg, "T-001")
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		fresh, _ := store.New(cfg)
		parent, _ := fresh.Get("T-001")
		val, ok := parent.Doc.GetField("scope_flags.split_decision")
		if !ok || val != "split" {
			t.Errorf("scope_flags.split_decision = %q (ok=%v), want %q", val, ok, "split")
		}
	})

	t.Run("parent_resolution_split", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		_, _, err := Split(s, cfg, "T-001")
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		fresh, _ := store.New(cfg)
		parent, _ := fresh.Get("T-001")
		if len(parent.Resolution) != 1 || parent.Resolution[0] != "split" {
			t.Errorf("parent resolution = %v, want [split]", parent.Resolution)
		}
	})

	t.Run("children_use_nextid", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		idA, idB, err := Split(s, cfg, "T-001")
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		// The children use NextID — sequential, not literal -a/-b suffixes.
		// Both should match the id pattern (T-NNN) and be different.
		if !strings.HasPrefix(idA, "T-") {
			t.Errorf("Part A id %q should start with T-", idA)
		}
		if !strings.HasPrefix(idB, "T-") {
			t.Errorf("Part B id %q should start with T-", idB)
		}
		if idA == idB {
			t.Errorf("Part A and Part B share id %q", idA)
		}
		if strings.HasSuffix(idA, "-a") || strings.HasSuffix(idB, "-b") {
			t.Errorf("ids must not use literal -a/-b suffixes: %q, %q", idA, idB)
		}
	})

	t.Run("acs_partitioned_by_layer", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		idA, idB, err := Split(s, cfg, "T-001")
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		fresh, _ := store.New(cfg)
		a, _ := fresh.Get(idA)
		b, _ := fresh.Get(idB)
		// Part A should have api-shaped ACs, Part B web-shaped.
		var aHasApi, aHasWeb, bHasApi, bHasWeb bool
		for _, ac := range a.AcceptanceCriteria {
			if strings.Contains(ac, "theraprac-api") || strings.Contains(ac, "make integration-local") {
				aHasApi = true
			}
			if strings.Contains(ac, "playwright") || strings.Contains(ac, "type-check") {
				aHasWeb = true
			}
		}
		for _, ac := range b.AcceptanceCriteria {
			if strings.Contains(ac, "theraprac-api") || strings.Contains(ac, "make integration-local") {
				bHasApi = true
			}
			if strings.Contains(ac, "playwright") || strings.Contains(ac, "type-check") {
				bHasWeb = true
			}
		}
		if !aHasApi {
			t.Errorf("Part A missing api-shaped ACs: %v", a.AcceptanceCriteria)
		}
		if aHasWeb {
			t.Errorf("Part A should not contain unambiguously-web ACs: %v", a.AcceptanceCriteria)
		}
		if !bHasWeb {
			t.Errorf("Part B missing web-shaped ACs: %v", b.AcceptanceCriteria)
		}
		if bHasApi {
			t.Errorf("Part B should not contain unambiguously-api ACs: %v", b.AcceptanceCriteria)
		}
	})

	t.Run("refuses_non_full_stack", func(t *testing.T) {
		s, cfg := setupTestEnv(t)
		// Save a backend-only plan; Split must refuse.
		if err := plan.Save(cfg.PlansDir(), "T-001", &plan.Plan{
			Approach:   "backend-only",
			ScopeRepos: []string{"theraprac-api"},
			ACs:        []string{"cmd: go test ./..."},
		}); err != nil {
			t.Fatalf("seed plan: %v", err)
		}
		_, _, err := Split(s, cfg, "T-001")
		if err == nil || !strings.Contains(err.Error(), "not full-stack") {
			t.Errorf("expected refusal on non-full-stack plan; got err=%v", err)
		}
	})

	t.Run("refuses_already_split", func(t *testing.T) {
		s, cfg := seedSplitParent(t)
		if _, _, err := Split(s, cfg, "T-001"); err != nil {
			t.Fatalf("first split: %v", err)
		}
		fresh, _ := store.New(cfg)
		if _, _, err := Split(fresh, cfg, "T-001"); err == nil {
			t.Errorf("second split should refuse; got nil error")
		}
	})

	t.Run("refuses_missing_plan_sidecar", func(t *testing.T) {
		s, cfg := setupTestEnv(t)
		_, _, err := Split(s, cfg, "T-001")
		if err == nil || !strings.Contains(err.Error(), "no plan sidecar") {
			t.Errorf("expected refusal when plan is missing; got err=%v", err)
		}
	})
}
