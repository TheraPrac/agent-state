package command

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

// TestPrepGitSyncNonFatal verifies that when GitSync fails during
// prepItemWriteOnly (because the temp dir has no git repo), prep still
// returns "accepted" — the GitSync error is logged but never propagated
// as a "rejected" result. Exercises the I-982 fix in prepItemWriteOnly.
func TestPrepGitSyncNonFatal(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)

	// Enable AutoCommit so GitSync actually executes instead of no-op'ing.
	// The temp dir has no .git repo, so GitSync will fail on the git-lock
	// or diff step. The fix must treat this as a warning, not a fatal error.
	cfg.Git = &config.GitConfig{AutoCommit: true}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found in store")
	}

	engine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 0)

	var result string
	suppressStdout(t, func() {
		result = prepItemWriteOnly(s, cfg, "T-001", item, PrepOpts{WriteOnly: true}, engine, "")
	})

	if result != "accepted" {
		t.Errorf("prepItemWriteOnly = %q, want \"accepted\" — GitSync failure must be non-fatal", result)
	}
}

// TestPrepGitSyncNonFatalInteractive verifies the same non-fatal property
// for the interactive prepItem path (I-982 fix in prepItem).
// Uses a stub engine that auto-accepts the plan review to avoid interactive I/O.
func TestPrepGitSyncNonFatalInteractive(t *testing.T) {
	s, cfg := setupPrepWriteOnlyEnv(t)
	cfg.Git = &config.GitConfig{AutoCommit: true}

	item, ok := s.Get("T-001")
	if !ok {
		t.Fatal("T-001 not found in store")
	}

	// Engine that returns canned plan on prep and canned review on plan_review.
	// SelectMenu returns "1" (Accept) so the review loop terminates.
	baseEngine, _, _ := makeWriteOnlyEngine(nil, nil, nil, 0)
	engine := RunEngine{
		RunClaude: baseEngine.RunClaude,
		PromptUser: func(_ string) (string, error) { return "", nil },
		SelectMenu: func(_ string, opts []menuOption, _ int) string {
			if len(opts) > 0 {
				return opts[0].Key // "1" = Accept
			}
			return "1"
		},
		ConfirmPrompt: func(_ string) bool { return false },
	}

	var result string
	suppressStdout(t, func() {
		result = prepItem(s, cfg, "T-001", item, PrepOpts{}, engine, "")
	})

	if result != "accepted" {
		t.Errorf("prepItem = %q, want \"accepted\" — GitSync failure must be non-fatal", result)
	}
}
