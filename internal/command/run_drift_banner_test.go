package command

import (
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
)

// I-429: every `st run` entrypoint must surface the binary-drift banner
// at startup, before any tokens are spent. Three entrypoints — Run
// (sprint), RunItem (single item), RunInteractive (no args) — must each
// invoke printBinaryDriftWarning. Tests fixture two live agents with
// distinct st commits, then call each entrypoint with the pipeline
// disabled so the function returns early after the banner.

func TestRunDriftBanner_RunInteractiveSurfacesBanner(t *testing.T) {
	// Clear AS_AGENT_ID — lifecycle_test.go uses os.Setenv (vs t.Setenv)
	// for "test-agent" and the leak makes registerRunProcess succeed and
	// write a registration we don't want in the fixture.
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-x", "deadbeef00", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-y", "cafebabe11", os.Getpid())
	cfg.Run = nil // no pipeline → RunInteractive bails after the banner

	out := captureStdout(t, func() {
		_ = RunInteractive(s, cfg, RunOpts{}, RunEngine{})
	})
	if !containsDriftBanner(out) {
		t.Errorf("RunInteractive did not emit drift banner; output:\n%s", out)
	}
}

func TestRunDriftBanner_RunItemSurfacesBanner(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-x", "deadbeef00", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-y", "cafebabe11", os.Getpid())
	cfg.Run = nil

	out := captureStdout(t, func() {
		_ = RunItem(s, cfg, "T-001", RunOpts{}, RunEngine{})
	})
	if !containsDriftBanner(out) {
		t.Errorf("RunItem did not emit drift banner; output:\n%s", out)
	}
}

func TestRunDriftBanner_RunSprintSurfacesBanner(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-x", "deadbeef00", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-y", "cafebabe11", os.Getpid())
	cfg.Run = nil

	out := captureStdout(t, func() {
		_ = Run(s, cfg, "nonexistent-sprint", RunOpts{}, RunEngine{})
	})
	if !containsDriftBanner(out) {
		t.Errorf("Run did not emit drift banner; output:\n%s", out)
	}
}

// /code-review finding: RunItem with a sprint delegates to Run, so the
// banner must NOT fire twice along that path. Originally I-429's first
// cut had RunItem fire the banner unconditionally, then Run fire again
// after delegation — this test pins the once-per-invocation guarantee.
func TestRunDriftBanner_SprintItemFiresOnce(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-x", "deadbeef00", os.Getpid())
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-y", "cafebabe11", os.Getpid())

	// Mark T-001 as belonging to a sprint so RunItem delegates to Run.
	if err := s.Mutate("T-001", func(it *model.Item) error {
		it.Doc.SetField("sprint", "test-sprint")
		it.Sprint = "test-sprint"
		return nil
	}); err != nil {
		t.Fatalf("seed sprint: %v", err)
	}
	cfg.Run = nil // Run() bails after the banner

	out := captureStdout(t, func() {
		_ = RunItem(s, cfg, "T-001", RunOpts{}, RunEngine{})
	})
	count := strings.Count(out, "binary drift")
	if count != 1 {
		t.Errorf("expected exactly 1 drift banner on sprint-delegating RunItem, got %d. Output:\n%s", count, out)
	}
}

func TestRunDriftBanner_SilentOnSingleAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	s, cfg := setupTestEnv(t)
	writeAgentRegistrationYAML(t, cfg.AgentsDir(), "agent-x", "deadbeef00", os.Getpid())
	cfg.Run = nil

	out := captureStdout(t, func() {
		_ = RunInteractive(s, cfg, RunOpts{}, RunEngine{})
	})
	if containsDriftBanner(out) {
		t.Errorf("expected silent drift on single-agent fixture, got banner in:\n%s", out)
	}
}

// containsDriftBanner: the banner shape may evolve, so match on the two
// stable substrings that any legible drift warning will carry.
func containsDriftBanner(s string) bool {
	return strings.Contains(s, "binary drift") || strings.Contains(s, "different st commits")
}
