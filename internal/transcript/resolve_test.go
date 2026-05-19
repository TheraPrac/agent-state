package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProjectSlug is the canonical slug test. It was previously duplicated
// in cmd/reconcile-tokens; the logic is promoted here so there is one
// source of truth (the reconcile copy was deleted in the same Phase-1
// refactor).
func TestProjectSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/jfinlinson/Dev/foo", "-Users-jfinlinson-Dev-foo"},
		{"/a", "-a"},
		{"", ""},
		{"relative/path", "relative-path"},
		{"/", "-"},
		// Spaces in a workspace path must survive verbatim — only "/"
		// is transformed (carry-over guard for the PR#86 space-in-path
		// class; the slug fn itself is space-safe, the upstream
		// by-session tokenizer is the separate tracked issue).
		{"/Users/john doe/Dev/foo", "-Users-john doe-Dev-foo"},
	}
	for _, c := range cases {
		if got := ProjectSlug(c.in); got != c.want {
			t.Errorf("ProjectSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeProjectsDir_EnvOverride(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "/tmp/override-projects")
	if got := ClaudeProjectsDir(); got != "/tmp/override-projects" {
		t.Errorf("ClaudeProjectsDir() = %q, want the env override", got)
	}
}

func TestResolveSessionJSONL(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", root)

	projectDir := "/Users/x/Dev/proj"
	sid := "sess-abc"
	slug := ProjectSlug(projectDir)
	base := filepath.Join(root, slug)

	// Empty inputs / nothing on disk yet → nil, never an error.
	if got := ResolveSessionJSONL("", sid); got != nil {
		t.Errorf("empty projectDir → %v, want nil", got)
	}
	if got := ResolveSessionJSONL(projectDir, ""); got != nil {
		t.Errorf("empty sid → %v, want nil", got)
	}
	if got := ResolveSessionJSONL(projectDir, sid); got != nil {
		t.Errorf("no files on disk → %v, want nil", got)
	}

	// Lay down a parent transcript + two subagent transcripts + a
	// decoy that must NOT be matched.
	if err := os.MkdirAll(filepath.Join(base, sid, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, sid+".jsonl")
	subB := filepath.Join(base, sid, "subagents", "agent-b.jsonl")
	subA := filepath.Join(base, sid, "subagents", "agent-a.jsonl")
	decoy := filepath.Join(base, sid, "subagents", "notes.txt")
	for _, p := range []string{parent, subB, subA, decoy} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := ResolveSessionJSONL(projectDir, sid)
	want := []string{parent, subA, subB} // parent first, subagents sorted
	if len(got) != len(want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resolved[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Parent absent but subagents present → subagents only (a worker
	// session whose parent rotated away must still resolve).
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	got = ResolveSessionJSONL(projectDir, sid)
	if len(got) != 2 || got[0] != subA || got[1] != subB {
		t.Errorf("parent-absent resolve = %v, want [%s %s]", got, subA, subB)
	}
}

func TestResolveSessionByID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", root)

	// Empty / not-found → nil, never an error.
	if got := ResolveSessionByID(""); got != nil {
		t.Errorf("empty sid → %v, want nil", got)
	}
	if got := ResolveSessionByID("ghost"); got != nil {
		t.Errorf("unknown sid → %v, want nil", got)
	}

	// Two project slugs; the sid lives under one, with a subagent.
	sid := "sess-xyz"
	slugA := filepath.Join(root, "-proj-a")
	slugB := filepath.Join(root, "-proj-b")
	if err := os.MkdirAll(filepath.Join(slugB, sid, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(slugA, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(slugB, sid+".jsonl")
	sub := filepath.Join(slugB, sid, "subagents", "agent-x.jsonl")
	other := filepath.Join(slugA, "different.jsonl") // must NOT match
	for _, p := range []string{parent, sub, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := ResolveSessionByID(sid)
	if len(got) != 2 {
		t.Fatalf("resolved %v, want parent+subagent only", got)
	}
	if got[0] != parent || got[1] != sub {
		t.Errorf("resolved %v, want [%s %s]", got, parent, sub)
	}
}
