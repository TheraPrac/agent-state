package command

import (
	"fmt"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/store"
)

// TestCreateFlushesCreatedLineBeforePush guards the I-797 ordering invariant:
// the human-readable "Created I-XXX — title" line must reach stdout BEFORE
// autoSync's git phase begins.
//
// Mechanism: inject a fake autoSyncGitFn that writes a sentinel marker to
// stdout, then capture the full output and verify that "Created I-" appears at
// an earlier line index than the sentinel. Because all writes happen in the
// same goroutine (no scheduling races), this is deterministic — a regression
// that moves the "Created" print below autoSync will flip the index order and
// fail the test.
func TestCreateFlushesCreatedLineBeforePush(t *testing.T) {
	t.Setenv("AS_INTERNAL_NO_REVIEW", "1")
	s, cfg := setupTestEnv(t)

	const gitMarker = "GIT_PHASE_STARTED"

	orig := autoSyncGitFn
	defer func() { autoSyncGitFn = orig }()
	autoSyncGitFn = func(_ *store.Store, _ string, _ ...string) error {
		fmt.Println(gitMarker)
		return nil
	}

	output := captureStdout(t, func() {
		if code := Create(s, cfg, "task", "flush test task", CreateOpts{
			Priority:       2,
			Situation:      "flush ordering test situation for I-797",
			Background:     "background for the flush ordering test",
			Assessment:     "assessment for the flush ordering test",
			Recommendation: "recommendation for the flush ordering test",
		}); code != 0 {
			t.Errorf("Create returned %d, want 0", code)
		}
	})

	lines := strings.Split(output, "\n")
	createdIdx := -1
	gitIdx := -1
	for i, l := range lines {
		if createdIdx == -1 && strings.HasPrefix(l, "Created ") {
			createdIdx = i
		}
		if gitIdx == -1 && l == gitMarker {
			gitIdx = i
		}
	}

	if createdIdx == -1 {
		t.Fatalf("no 'Created I-XXX' line in stdout; output: %q", output)
	}
	if gitIdx == -1 {
		t.Fatalf("git marker not found in stdout (autoSyncGitFn not called?); output: %q", output)
	}
	if createdIdx >= gitIdx {
		t.Errorf("ordering regression: 'Created' line (index %d) appears at or after git phase marker (index %d)\noutput:\n%s",
			createdIdx, gitIdx, output)
	}
}
