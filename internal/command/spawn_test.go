package command

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/testutil"
)

// captureSpawnIO swaps stdout/stderr while fn runs and returns
// captured output + exit code. Used by every TestSpawnChild* case.
func captureSpawnIO(t *testing.T, fn func() int) (stdout, stderr string, code int) {
	t.Helper()
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr

	doneOut := make(chan string)
	doneErr := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rOut)
		doneOut <- b.String()
	}()
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rErr)
		doneErr <- b.String()
	}()

	code = fn()
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	stdout = <-doneOut
	stderr = <-doneErr
	return
}

// stampParentClaim writes claimed_by directly so the spawn test
// doesn't need to drive the full Start path (which fans out worktree
// creation we don't care about here). Mirrors claim_race_test.go's
// stampClaim helper.
func stampParentClaim(t *testing.T, env *testutil.Env, id, sessionID string) {
	t.Helper()
	if err := env.S.Mutate(id, func(it *model.Item) error {
		it.ClaimedBy = sessionID
		it.ClaimedAt = "2026-04-28T10:00:00-06:00"
		it.Doc.SetField("claimed_by", sessionID)
		it.Doc.SetField("claimed_at", "2026-04-28T10:00:00-06:00")
		return nil
	}); err != nil {
		t.Fatalf("stampParentClaim: %v", err)
	}
}

// TestSpawnChild_HappyPath — root agent claims T-001, spawns child
// for same item. Stdout names the child id + PID; the registration
// file on disk has parent + root populated.
func TestSpawnChild_HappyPath(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_AGENT_ROOT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-parent")

	env := testutil.NewEnv(t)
	stampParentClaim(t, env, "T-001", "session-parent")

	stdout, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code != 0 {
		t.Fatalf("SpawnChild exit=%d stderr=%q", code, stderr)
	}
	if stdout == "" {
		t.Fatalf("expected stdout to name child id + PID, got empty")
	}

	// Stdout format: "<id>\t<pid>\n"
	parts := strings.Fields(stdout)
	if len(parts) < 2 {
		t.Fatalf("stdout doesn't look like '<id>\\t<pid>', got %q", stdout)
	}
	childID := parts[0]
	if !strings.HasPrefix(childID, "agent-a-") {
		t.Errorf("child id should be suffixed under parent (agent-a-N); got %q", childID)
	}

	// Registration file on disk reflects the heritage.
	reg, err := agent.LoadRegistration(env.Cfg, childID)
	if err != nil {
		t.Fatalf("LoadRegistration(%s): %v", childID, err)
	}
	if reg == nil {
		t.Fatalf("registration file missing for %s", childID)
	}
	if reg.Parent != "agent-a" {
		t.Errorf("reg.Parent = %q, want agent-a", reg.Parent)
	}
	if reg.Root != "agent-a" {
		t.Errorf("reg.Root = %q, want agent-a", reg.Root)
	}
	if reg.Scope != "item:T-001" {
		t.Errorf("reg.Scope = %q, want item:T-001", reg.Scope)
	}
	if reg.Role != "child" {
		t.Errorf("reg.Role = %q, want child", reg.Role)
	}
}

// TestSpawnChild_DepthLimitFromChildRejected — caller's Identity
// already names a parent (i.e. the caller is itself a child).
// Spawning would reach grandchild depth; rejected per T-312 policy.
func TestSpawnChild_DepthLimitFromChildRejected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a-1")
	t.Setenv("AS_AGENT_PARENT_ID", "agent-a")
	t.Setenv("AS_AGENT_ROOT_ID", "agent-a")
	t.Setenv("AS_SESSION_ID", "session-child")

	env := testutil.NewEnv(t)
	stampParentClaim(t, env, "T-001", "session-child")

	_, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code == 0 {
		t.Errorf("expected rejection, got exit 0")
	}
	if !strings.Contains(stderr, "depth-2") {
		t.Errorf("expected depth-2 message in stderr, got %q", stderr)
	}
}

// TestSpawnChild_NoIdentityRejected — caller has no AS_AGENT_ID and
// no local-agent.yaml. SpawnChild can't construct a meaningful child
// without a parent identity; reject loudly.
func TestSpawnChild_NoIdentityRejected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_AGENT_ROOT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-x")

	env := testutil.NewEnv(t)

	_, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code == 0 {
		t.Errorf("expected rejection without identity, got exit 0")
	}
	if !strings.Contains(stderr, "no agent identity") {
		t.Errorf("expected 'no agent identity' in stderr, got %q", stderr)
	}
}

// TestSpawnChild_ItemClaimedByOtherSessionRejected — parent's session
// doesn't own the requested item. Refuses rather than silently
// spawning a child whose scope is wrong.
func TestSpawnChild_ItemClaimedByOtherSessionRejected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-parent")

	env := testutil.NewEnv(t)
	stampParentClaim(t, env, "T-001", "someone-else")

	_, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code == 0 {
		t.Errorf("expected rejection on foreign claim, got exit 0")
	}
	if !strings.Contains(stderr, "claimed by session") {
		t.Errorf("expected 'claimed by session' in stderr, got %q", stderr)
	}
}

// TestSpawnChild_UnclaimedItemAccepted — item has no claim yet
// (parent hasn't run Start). SpawnChild accepts because no foreign
// session owns it; the parent's session is implicitly the owner.
func TestSpawnChild_UnclaimedItemAccepted(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-parent")

	env := testutil.NewEnv(t)
	// T-001 stays unclaimed.

	stdout, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code != 0 {
		t.Errorf("expected acceptance on unclaimed item, got exit %d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "agent-a-") {
		t.Errorf("expected child id in stdout, got %q", stdout)
	}
}

// TestSpawnChild_TwoChildrenGetDistinctSuffixes — calling SpawnChild
// twice from the same parent allocates agent-a-1 and agent-a-2 (the
// nextSuffix scan over the agents dir).
func TestSpawnChild_TwoChildrenGetDistinctSuffixes(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-parent")

	env := testutil.NewEnv(t)
	stampParentClaim(t, env, "T-001", "session-parent")

	stdout1, _, code1 := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})
	stdout2, _, code2 := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-001"})
	})

	if code1 != 0 || code2 != 0 {
		t.Fatalf("both spawns must succeed, got codes %d and %d", code1, code2)
	}
	id1 := strings.Fields(stdout1)[0]
	id2 := strings.Fields(stdout2)[0]
	if id1 == id2 {
		t.Errorf("both spawns produced same id %q", id1)
	}
	for _, want := range []string{"agent-a-1", "agent-a-2"} {
		path := filepath.Join(env.Cfg.AgentsDir(), want+".yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected registration %s, missing: %v", path, err)
		}
	}
}

// TestSpawnChild_MissingItemRejected — bad item id. Reject early.
func TestSpawnChild_MissingItemRejected(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("AS_AGENT_PARENT_ID", "")
	t.Setenv("AS_SESSION_ID", "session-parent")

	env := testutil.NewEnv(t)

	_, stderr, code := captureSpawnIO(t, func() int {
		return SpawnChild(env.S, env.Cfg, SpawnChildOpts{Item: "T-NONEXISTENT"})
	})

	if code == 0 {
		t.Errorf("expected rejection on missing item, got exit 0")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("expected 'not found' in stderr, got %q", stderr)
	}
}
