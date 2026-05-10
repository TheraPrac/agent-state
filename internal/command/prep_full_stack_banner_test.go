package command

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/plan"
	"github.com/jfinlinson/agent-state/internal/store"
)

// fullStackPlanText is what the mock claude returns to populate the
// in-prep plan: full-stack scope + 6 ACs (above the I-180 threshold
// of 5) so DetectFullStack fires.
const fullStackPlanText = `## Approach
Cross-cutting api + web feature.

## Scope
Repos: theraprac-api, theraprac-web

## Implementation Steps
1. Backend handler
2. Frontend page

## Files to Create
- theraprac-api/internal/handlers/foo.go
- theraprac-web/src/app/foo/page.tsx

## Files to Modify
- theraprac-api/api/openapi/api.yaml
- theraprac-web/src/lib/api/foo.ts

## Acceptance Criteria
- cmd: cd ../theraprac-api && make integration-local
- cmd: cd ../theraprac-api && make test-unit
- cmd: go test ./internal/handlers/...
- cmd: cd ../theraprac-web && npm run type-check
- cmd: cd ../theraprac-web && npm run test:unit
- cmd: cd ../theraprac-web && npx playwright test foo.spec.ts
`

// setupBannerEnv stages a fixture sprint with a single unplanned
// full-stack item so Prep takes the prepItem path.
//
// setupPrepWriteOnlyEnv already adds T-001 and T-002 to the sprint,
// so we don't re-add (prepStampSprintItems would duplicate). We just
// stamp T-002 as plan_approved so it's filtered out of `unplanned`,
// leaving only T-001 for prepItem.
func setupBannerEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	s, cfg := setupPrepWriteOnlyEnv(t)
	// Stamp T-002 as plan_approved + sidecar so prep skips it.
	prepStampSprintItems(t, s, cfg, "wo-sprint", []string{}, []string{"T-002"})
	if err := plan.Save(cfg.PlansDir(), "T-002", &plan.Plan{
		Approach: "noop", ScopeRepos: []string{"as"},
		ACs: []string{"cmd: echo ok"}, Approved: true,
	}); err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

// bannerEngine returns a RunEngine that:
//   - returns the canned full-stack plan text from RunClaude (prep step),
//   - returns a benign review narrative for the plan_review step,
//   - drives the Plan Review menu via the supplied `gateChoice` (e.g.
//     "1" for Accept, "5" for Split),
//   - asserts no other interactive helpers were invoked.
//
// Output capture: stdout drives through suppressStdout; the test
// re-captures via separate calls.
func bannerEngine(gateChoice string) RunEngine {
	return RunEngine{
		RunClaude: func(cwd string, args, env []string) ([]byte, int, error) {
			step := "prep"
			for _, e := range env {
				if strings.HasPrefix(e, "ST_RUN_STEP=") {
					step = strings.TrimPrefix(e, "ST_RUN_STEP=")
				}
			}
			result := ClaudeResult{Type: "result", Subtype: "success"}
			if step == "plan_review" {
				result.Result = "## Recommendation\nAccept\n"
			} else {
				result.Result = fullStackPlanText
			}
			data, _ := json.Marshal(result)
			return data, 0, nil
		},
		PromptUser: func(p string) (string, error) { return "", nil },
		SelectMenu: func(p string, opts []menuOption, def int) string {
			// The Plan Review gate has 4 or 5 options. Only return the
			// gate choice when it's the Plan Review menu.
			for _, o := range opts {
				if strings.Contains(o.Label, "save plan and proceed") {
					return gateChoice
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

// I-180: choosing "Split" on the SPLIT RECOMMENDATION gate creates
// two new linked child items (allocated via NextID — NOT literal -a/-b
// suffixes), marks the parent scope_flags.split_decision = "split",
// and rejects the plan.
func TestPrepFullStackBanner_AcceptSplitCreatesChildren(t *testing.T) {
	s, cfg := setupBannerEnv(t)

	// Snapshot the item count before Prep so we can detect the two
	// new children created by Split (the fixture seeds T-001..T-004,
	// so absolute id assertions would be brittle — use a delta).
	preCount := len(s.All())

	engine := bannerEngine("5")
	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{}, engine)
	})

	fresh, _ := store.New(cfg)
	parent, ok := fresh.Get("T-001")
	if !ok {
		t.Fatal("parent T-001 missing after split")
	}
	val, _ := parent.Doc.GetField("scope_flags.split_decision")
	if val != "split" {
		t.Errorf("parent scope_flags.split_decision = %q, want %q", val, "split")
	}

	postCount := len(fresh.All())
	if postCount-preCount != 2 {
		t.Errorf("expected 2 new items after split, got delta=%d (pre=%d post=%d)",
			postCount-preCount, preCount, postCount)
	}

	// The parent's plan sidecar should NOT be approved.
	loadedPlan, _ := plan.Load(cfg.PlansDir(), "T-001")
	if loadedPlan != nil && loadedPlan.Approved {
		t.Error("parent plan should remain unapproved after split")
	}
}

// I-180: choosing "Accept" while a SPLIT RECOMMENDATION is shown
// stamps scope_flags.split_decision = "kept-unified" so retrospective
// analysis can correlate split-vs-unified outcomes.
func TestPrepFullStackBanner_DeclineRecordsKeptUnified(t *testing.T) {
	s, cfg := setupBannerEnv(t)

	preCount := len(s.All())

	engine := bannerEngine("1")
	suppressStdout(t, func() {
		_ = Prep(s, cfg, "wo-sprint", PrepOpts{}, engine)
	})

	fresh, _ := store.New(cfg)
	parent, ok := fresh.Get("T-001")
	if !ok {
		t.Fatal("parent T-001 missing")
	}
	val, _ := parent.Doc.GetField("scope_flags.split_decision")
	if val != "kept-unified" {
		t.Errorf("parent scope_flags.split_decision = %q, want %q", val, "kept-unified")
	}

	postCount := len(fresh.All())
	if postCount != preCount {
		t.Errorf("decline should not create new items; got delta=%d", postCount-preCount)
	}
}
