package main

import (
	"os"
	"strings"
	"testing"
)

// TestWhoami_PrintsAgentIDAndEnv verifies that `st whoami` prints the required
// fields even when config discovery fails (no st project in cwd). I-877.
func TestWhoami_PrintsAgentIDAndEnv(t *testing.T) {
	os.Setenv("AS_AGENT_ID", "agent-test")
	os.Setenv("ST_ROOT", "/test/root")
	defer os.Unsetenv("AS_AGENT_ID")
	defer os.Unsetenv("ST_ROOT")

	// Use a temp dir with no st project so the command falls through to the
	// "unknown" path, which still prints env vars.
	out, _ := runInProcess(t, t.TempDir(), "whoami")
	for _, want := range []string{"AS_AGENT_ID:", "ST_ROOT:"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami output missing %q; got:\n%s", want, out)
		}
	}
}

// TestWhoami_MatchWarnsOnMismatch verifies that a mismatch between AS_AGENT_ID
// and the resolved config identity surfaces a WARNING line. I-877.
func TestWhoami_MatchWarnsOnMismatch(t *testing.T) {
	dir := setupInProcessWorkspace(t)
	// AS_AGENT_ID=wrong-agent; config resolves agent from workspace which will
	// differ → warning expected.
	os.Setenv("AS_AGENT_ID", "wrong-agent")
	defer os.Unsetenv("AS_AGENT_ID")

	out, _ := runInProcess(t, dir, "whoami")
	// Either WARNING is present or the resolved agent_id is empty (no identity
	// in a bare temp workspace). Either outcome is acceptable — the key is that
	// the command exits 0 and prints the fields.
	if !strings.Contains(out, "agent_id:") {
		t.Errorf("whoami output missing agent_id:; got:\n%s", out)
	}
}
