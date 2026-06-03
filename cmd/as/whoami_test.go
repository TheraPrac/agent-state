package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWhoami_PrintsAgentIDAndEnv verifies that `st whoami` prints the required
// fields even when config discovery fails (no st project in cwd). I-877.
func TestWhoami_PrintsAgentIDAndEnv(t *testing.T) {
	// runInProcess sets AS_AGENT_ID=test-agent internally; just verify the
	// fields appear in output.
	out, _ := runInProcess(t, t.TempDir(), "whoami")
	for _, want := range []string{"AS_AGENT_ID:", "ST_ROOT:"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami output missing %q; got:\n%s", want, out)
		}
	}
}

// TestWhoami_WarnsOnMismatch verifies that a mismatch between AS_AGENT_ID and
// the config-resolved identity surfaces a WARNING line. I-877.
//
// runInProcess sets AS_AGENT_ID=test-agent unconditionally. We set up a
// workspace whose local-agent.yaml declares id=real-agent so the resolved
// identity differs, triggering the warning.
func TestWhoami_WarnsOnMismatch(t *testing.T) {
	dir := setupInProcessWorkspace(t)
	// Write a local-agent.yaml with a known id that differs from
	// AS_AGENT_ID=test-agent (set by runInProcess).
	os.MkdirAll(filepath.Join(dir, ".as"), 0o755)
	os.WriteFile(filepath.Join(dir, ".as", "local-agent.yaml"),
		[]byte("id: real-agent\n"), 0o644)

	out, _ := runInProcess(t, dir, "whoami")
	// The config resolves id=real-agent; AS_AGENT_ID=test-agent → mismatch.
	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING in output for identity mismatch; got:\n%s", out)
	}
}
