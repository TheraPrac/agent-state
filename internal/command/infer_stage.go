package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// InferStageOpts configures the infer-stage command. Mirrors ReconcileOpts'
// injection pattern so tests can stub branch / PR signals without exec'ing
// git or gh.
//
// Note: the BranchCheck / PRFetch fields and their defaulting accessors
// duplicate ReconcileOpts in reconcile.go. A future refactor could extract
// a shared `probeOpts` type embedded by both — left out of this PR to keep
// the diff small. See PR-104 review finding #4.
type InferStageOpts struct {
	BranchCheck func(*config.Config, string) bool
	PRFetch     func(*config.Config, string) (string, []string)
}

func (o *InferStageOpts) branchCheck() func(*config.Config, string) bool {
	if o.BranchCheck != nil {
		return o.BranchCheck
	}
	return branchExistsOnRemote
}

func (o *InferStageOpts) prFetch() func(*config.Config, string) (string, []string) {
	if o.PRFetch != nil {
		return o.PRFetch
	}
	return getPRState
}

// InferStage probes filesystem / GitHub state for one item and forward-only
// advances delivery.stage. Resolution: explicit id arg, else stack-top.
//
// Probe order (cheapest first):
//
//	1. branch-on-remote                  => ensure >= "pushed"
//	2. gh pr list --state OPEN/MERGED    => "pr_open" / "merged"
//
// Leaves deployed_dev / uat_approved / closed alone — those require AWS
// state or explicit operator action and remain in `st reconcile`'s scope.
//
// Returns 0 on every "nothing to do" path so the stop hook never blocks.
func InferStage(s *store.Store, cfg *config.Config, id string, opts InferStageOpts) int {
	if id == "" {
		entries := LoadStack(cfg)
		if len(entries) == 0 {
			return 0
		}
		id = entries[0].ID
	}

	item, ok := s.Get(id)
	if !ok {
		return 0
	}

	branch, _ := getNestedField(item, "work_tracking", "branch")
	if branch == "" || branch == "null" {
		return 0
	}

	branchExists := opts.branchCheck()(cfg, branch)
	state, _ := opts.prFetch()(cfg, branch)

	// The PR signal (when present) wins over branch existence — a deleted
	// upstream branch with a still-OPEN PR (e.g. force-push churn) keeps
	// pr_open as the inference target. The forward-only guard in
	// advanceDeliveryStage prevents this from regressing a later stage.
	target := ""
	if branchExists {
		target = "pushed"
	}
	switch state {
	case "OPEN":
		target = "pr_open"
	case "MERGED":
		target = "merged"
	case "CLOSED":
		// Symmetric with reconcile.go:181-183 — surface CLOSED PRs so the
		// operator notices a branch whose PR was closed without merging.
		// Stop hooks discard stderr so this is informational-only when
		// invoked from session-stop.sh; explicit `st infer-stage <id>`
		// surfaces it.
		fmt.Fprintf(os.Stderr, "infer-stage: %s PR closed without merging (branch: %s)\n", id, branch)
	}

	if target == "" {
		return 0
	}

	if err := s.Mutate(id, func(it *model.Item) error {
		advanceDeliveryStage(it, target)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "infer-stage: %v\n", err)
		return 1
	}
	return 0
}
