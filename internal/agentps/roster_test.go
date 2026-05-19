package agentps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRosterLoadAndResolve(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, ".as", "agent-workspaces")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(wsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-b.yaml", "agent_id: agent-b\npath: /Users/x/theraprac-agent-b\nbranch: main\n")
	write("agent-a.yaml", "agent_id: agent-a\npath: \"/Users/x/theraprac-agent-a\"\n")
	write("agent-z.yaml", "branch: main\n") // malformed: no agent_id → filename fallback
	write("notes.txt", "ignored")           // non-matching glob

	got, err := LoadRoster(wsDir)
	if err != nil {
		t.Fatalf("LoadRoster: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d roster agents, want 3: %+v", len(got), got)
	}
	// Sorted by AgentID; malformed file falls back to filename stem.
	want := []RosterAgent{
		{"agent-a", "/Users/x/theraprac-agent-a"},
		{"agent-b", "/Users/x/theraprac-agent-b"},
		{"agent-z", ""},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("roster[%d] = %+v, want %+v", i, got[i], w)
		}
	}

	// Missing dir → error (caller reports absence, never silent empty).
	if _, err := LoadRoster(filepath.Join(root, "nope")); err == nil {
		t.Error("LoadRoster(missing) should error")
	}
	if _, err := LoadRoster(""); err == nil {
		t.Error("LoadRoster(\"\") should error")
	}
	// Existing-but-empty dir → ([], nil); the caller turns len==0 into
	// a reported non-zero exit (distinct from a read error).
	emptyDir := filepath.Join(root, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err := LoadRoster(emptyDir); err != nil || len(r) != 0 {
		t.Errorf("LoadRoster(empty dir) = (%v,%v), want ([],nil)", r, err)
	}

	// Resolution: env override wins.
	t.Setenv("ST_AGENT_WORKSPACES_DIR", wsDir)
	if d := AgentWorkspacesDir(nil); d != wsDir {
		t.Errorf("AgentWorkspacesDir env override = %q, want %q", d, wsDir)
	}
	// Without override, searchUp finds the ancestor dir.
	os.Unsetenv("ST_AGENT_WORKSPACES_DIR")
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if d := searchUp(deep); d != wsDir {
		t.Errorf("searchUp(%q) = %q, want %q", deep, d, wsDir)
	}
	// No ancestor has .as/agent-workspaces → "" (fresh independent
	// temp tree; its /var/folders ancestors carry no such dir).
	if d := searchUp(t.TempDir()); d != "" {
		t.Errorf("searchUp with no roster ancestor = %q, want \"\"", d)
	}
}
