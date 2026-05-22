package agentprogress

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProgressLoad(t *testing.T) {
	dir := t.TempDir()

	// agent-a — a clean record
	mustWrite(t, filepath.Join(dir, "agent-a.yaml"),
		"agent_id: agent-a\n"+
			"session_id: sess-1\n"+
			"updated: 2026-05-22T20:00:00Z\n"+
			"progress: \"hello world\"\n")

	// agent-b — exercise the YAML-metacharacter round-trip in the same
	// double-quoted form the workspace hook writes: `\"` for `"` and
	// `\\` for `\`.
	mustWrite(t, filepath.Join(dir, "agent-b.yaml"),
		"agent_id: agent-b\n"+
			"session_id: sess-2\n"+
			"updated: 2026-05-22T20:01:00Z\n"+
			`progress: "has \"quotes\" and \\backslash and # hash and : colon"`+"\n")

	// A garbled file (no parseable fields) must NOT drop the others and
	// must NOT register an empty record under its filename stem.
	mustWrite(t, filepath.Join(dir, "garbage.yaml"), "\x00\x01not yaml at all\x00")

	// A non-yaml file must be ignored entirely.
	mustWrite(t, filepath.Join(dir, "README.md"), "# notes")

	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", dir, err)
	}

	if got := len(out); got != 2 {
		t.Fatalf("expected 2 records, got %d (%v)", got, out)
	}

	a, ok := out["agent-a"]
	if !ok {
		t.Fatalf("agent-a record missing from %v", out)
	}
	if a.SessionID != "sess-1" {
		t.Errorf("agent-a SessionID = %q; want %q", a.SessionID, "sess-1")
	}
	if a.Progress != "hello world" {
		t.Errorf("agent-a Progress = %q; want %q", a.Progress, "hello world")
	}
	if want := time.Date(2026, 5, 22, 20, 0, 0, 0, time.UTC); !a.Updated.Equal(want) {
		t.Errorf("agent-a Updated = %v; want %v", a.Updated, want)
	}

	b, ok := out["agent-b"]
	if !ok {
		t.Fatalf("agent-b record missing from %v", out)
	}
	if want := `has "quotes" and \backslash and # hash and : colon`; b.Progress != want {
		t.Errorf("agent-b Progress round-trip = %q; want %q", b.Progress, want)
	}

	if _, ok := out["garbage"]; ok {
		t.Errorf("garbage.yaml should NOT register a record; got %v", out["garbage"])
	}
}

func TestLoad_MissingDirIsEmpty(t *testing.T) {
	out, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("missing dir should not error; got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("missing dir should be empty; got %v", out)
	}
}

func TestLoad_EmptyDirArgIsEmpty(t *testing.T) {
	out, err := Load("")
	if err != nil {
		t.Errorf("empty dir arg should not error; got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty dir arg should be empty; got %v", out)
	}
}

func TestProgressDir_NilCfg(t *testing.T) {
	if got := ProgressDir(nil); got != "" {
		t.Errorf("ProgressDir(nil) = %q; want \"\"", got)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
