package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// writeFixtureSession lays down a real-shaped Claude Code session JSONL
// for projectDir/sid under a temp CLAUDE_PROJECTS_DIR and returns the sid.
func writeFixtureSession(t *testing.T, projectDir, sid string) {
	t.Helper()
	root := os.Getenv("CLAUDE_PROJECTS_DIR")
	if root == "" {
		t.Fatal("CLAUDE_PROJECTS_DIR must be set by the caller")
	}
	dir := filepath.Join(root, transcript.ProjectSlug(projectDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","timestamp":"2026-05-18T10:00:00Z","message":{"role":"user","content":"run tests"}}
{"type":"assistant","timestamp":"2026-05-18T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Running tests."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","timestamp":"2026-05-18T10:00:05Z","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"ok all passed","is_error":false}]}}
`
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranscript_SessionSelectorRendersReadably(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	sid := "sess-render-1"
	writeFixtureSession(t, "/tmp/tp-fixture", sid)

	out := captureStdout(t, func() {
		if code := Transcript(s, cfg, sid, TranscriptOpts{}); code != 0 {
			t.Fatalf("Transcript exit %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Bash: go test ./... → ok all passed") {
		t.Errorf("missing collapsed Bash line:\n%s", out)
	}
	if !strings.Contains(out, "run tests") || !strings.Contains(out, "Running tests.") {
		t.Errorf("missing prose lines:\n%s", out)
	}
	if !strings.Contains(out, "[A] 10:00:00 ") {
		t.Errorf("expected tag + timestamp prefix:\n%s", out)
	}
}

func TestTranscript_NotFoundIsReportedNotSwallowed(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	if code := Transcript(s, cfg, "no-such-session", TranscriptOpts{}); code != 1 {
		t.Errorf("unknown selector exit %d, want 1 (absence must be reported)", code)
	}
	if code := Transcript(s, cfg, "", TranscriptOpts{}); code != 1 {
		t.Errorf("empty selector exit %d, want 1", code)
	}
}

func TestTranscript_JSONAndGrep(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	sid := "sess-json-1"
	writeFixtureSession(t, "/tmp/tp-fixture", sid)

	jsonOut := captureStdout(t, func() {
		if code := Transcript(s, cfg, sid, TranscriptOpts{JSON: true}); code != 0 {
			t.Fatalf("--json exit %d", code)
		}
	})
	var rows []transcript.Row
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("--json output is not valid []Row JSON: %v\n%s", err, jsonOut)
	}
	if len(rows) == 0 {
		t.Error("--json produced no rows")
	}

	grepOut := captureStdout(t, func() {
		Transcript(s, cfg, sid, TranscriptOpts{Grep: "Bash:"})
	})
	for _, l := range strings.Split(strings.TrimSpace(grepOut), "\n") {
		if l != "" && !strings.Contains(l, "Bash:") {
			t.Errorf("--grep leaked a non-matching line: %q", l)
		}
	}
	if !strings.Contains(grepOut, "Bash:") {
		t.Errorf("--grep dropped the matching line:\n%s", grepOut)
	}
}

func TestTranscript_ReviewFixes(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	sid := "sess-fixes-1"
	writeFixtureSession(t, "/tmp/tp-fixture", sid)

	// --json + --grep is rejected, not silently no-op'd.
	if code := Transcript(s, cfg, sid, TranscriptOpts{JSON: true, Grep: "x"}); code != 1 {
		t.Errorf("--json+--grep exit %d, want 1 (must be rejected, not silently ignored)", code)
	}

	// A typo'd --agent that filters everything is reported (non-zero),
	// not a silent exit-0 that looks like "nothing happened".
	if code := Transcript(s, cfg, sid, TranscriptOpts{Agent: "agnet-typo"}); code != 1 {
		t.Errorf("--agent typo exit %d, want 1 (post-filter emptiness must be reported)", code)
	}

	// (No --since assertion here: it compares against time.Now(), which
	// would make the result wall-clock-dependent / flaky. The --agent
	// case above already covers the post-filter-empty → exit-1 path
	// deterministically.)

	// Sanity: unfiltered still works.
	if code := Transcript(s, cfg, sid, TranscriptOpts{}); code != 0 {
		t.Errorf("unfiltered exit %d, want 0", code)
	}
}

func TestTranscript_ItemSelectorResolvesBySession(t *testing.T) {
	s, cfg := setupTestEnv(t)
	t.Setenv("CLAUDE_PROJECTS_DIR", t.TempDir())
	const projectDir = "/tmp/tp-item-fixture"
	sid := "sess-item-1"
	writeFixtureSession(t, projectDir, sid)

	// Attach a by_session entry to a seeded item so the item selector
	// resolves to the fixture session.
	if err := s.Mutate("T-001", func(item *model.Item) error {
		upsertBySession(item, sid, projectDir, "2026-05-18T10:00:00Z", realTokens{})
		return nil
	}); err != nil {
		t.Fatalf("Mutate T-001: %v", err)
	}

	out := captureStdout(t, func() {
		if code := Transcript(s, cfg, "T-001", TranscriptOpts{}); code != 0 {
			t.Fatalf("item Transcript exit %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Bash: go test ./... → ok all passed") {
		t.Errorf("item selector did not resolve/render its session:\n%s", out)
	}
}
