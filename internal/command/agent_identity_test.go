package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
)

func mustTouchConfig(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("project:\n  name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWriteLocalAgent(t *testing.T, root string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".as"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "local-agent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AS_AGENT_ID",
		"AS_AGENT_PARENT_ID",
		"AS_AGENT_ROOT_ID",
		"AS_AGENT_SPAWNED_BY_SESSION",
		"AS_AGENT_DELEGATED_ITEM",
		"AS_AGENT_ROLE",
	} {
		t.Setenv(k, "")
	}
}

// newCfgRootedAt loads a config from <root>/.as/config.yaml. Callers must
// have created that file first via mustTouchConfig — failure to load is a
// test-setup error, not a runtime fallback.
//
// I-1371: chdir into the temp root first. config.LoadFrom sets cfg.startDir =
// os.Getwd() (I-936, to immunize the agent-workspace.yaml marker lookup against
// ST_ROOT redirects), and Identity() walks startDir up for that marker before
// the per-root local-agent.yaml. Without the chdir, startDir stays inside
// theraprac-agent-a and the walk finds the real agent-a marker, leaking
// "agent-a" into these temp-rooted tests. t.Chdir restores the CWD at test end.
func newCfgRootedAt(t *testing.T, root string) *config.Config {
	t.Helper()
	t.Chdir(root)
	cfg, err := config.LoadFrom(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

func TestAgentIdentityShow_EnvSource(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-x")
	root := t.TempDir()
	mustTouchConfig(t, root)
	cfg := newCfgRootedAt(t, root)

	var buf bytes.Buffer
	if rc := agentIdentityShowTo(cfg, &buf); rc != 0 {
		t.Fatalf("exit = %d", rc)
	}
	out := buf.String()
	for _, want := range []string{"id:             agent-x", "source:         env"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestAgentIdentityShow_LocalConfigSource(t *testing.T) {
	resetIdentityEnv(t)
	root := t.TempDir()
	mustTouchConfig(t, root)
	mustWriteLocalAgent(t, root, "id: agent-from-config\ndisplay_name: Bob\nrole: explorer\n")
	cfg := newCfgRootedAt(t, root)

	var buf bytes.Buffer
	agentIdentityShowTo(cfg, &buf)
	out := buf.String()
	for _, want := range []string{
		"id:             agent-from-config",
		"display_name:   Bob",
		"source:         local-config",
		"role:           explorer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestAgentIdentityShow_InheritedSource(t *testing.T) {
	resetIdentityEnv(t)
	t.Setenv("AS_AGENT_ID", "agent-child")
	t.Setenv("AS_AGENT_PARENT_ID", "agent-a")
	t.Setenv("AS_AGENT_ROOT_ID", "agent-root")
	t.Setenv("AS_AGENT_ROLE", "reviewer")
	t.Setenv("AS_AGENT_SPAWNED_BY_SESSION", "sess-1")
	t.Setenv("AS_AGENT_DELEGATED_ITEM", "I-200")
	root := t.TempDir()
	mustTouchConfig(t, root)
	cfg := newCfgRootedAt(t, root)

	var buf bytes.Buffer
	agentIdentityShowTo(cfg, &buf)
	out := buf.String()
	for _, want := range []string{
		"id:             agent-child",
		"source:         inherited",
		"parent_id:      agent-a",
		"root_id:        agent-root",
		"role:           reviewer",
		"spawned_by:     sess-1",
		"delegated_item: I-200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestAgentIdentityShow_UnsetWithHint(t *testing.T) {
	resetIdentityEnv(t)
	root := t.TempDir()
	mustTouchConfig(t, root)
	cfg := newCfgRootedAt(t, root)

	var buf bytes.Buffer
	agentIdentityShowTo(cfg, &buf)
	out := buf.String()
	if !strings.Contains(out, "id:             (unset)") {
		t.Errorf("expected (unset) hint, got:\n%s", out)
	}
	if !strings.Contains(out, "AS_AGENT_ID") {
		t.Errorf("expected hint mentioning AS_AGENT_ID, got:\n%s", out)
	}
}
