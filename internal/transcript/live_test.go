package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestLiveThisSession is the contract §13-finding-1 ground-truth check:
// the resolver + reader must work against a REAL Claude Code session on
// disk, not just curated fixtures (worker self-narrative ≠ ground truth —
// so the renderer's substrate is verified against the substrate itself).
//
// It is skipped in the normal suite (it reads $HOME and is therefore
// machine-specific / non-deterministic) and runs only when explicitly
// asked, so it is repeatable and agent-verifiable without making
// `go test ./...` flaky:
//
//	TRANSCRIPT_LIVE=1 \
//	  TRANSCRIPT_LIVE_PROJECT_DIR=/Users/.../theraprac-agent-b \
//	  go test ./internal/transcript/ -run TestLiveThisSession -count=1 -v
//
// Asserts the most-recently-modified session resolves and yields ≥1
// assistant text row and ≥1 tool_use row.
func TestLiveThisSession(t *testing.T) {
	if os.Getenv("TRANSCRIPT_LIVE") == "" {
		t.Skip("set TRANSCRIPT_LIVE=1 to run the live ground-truth check")
	}
	projectDir := os.Getenv("TRANSCRIPT_LIVE_PROJECT_DIR")
	if projectDir == "" {
		t.Fatal("TRANSCRIPT_LIVE_PROJECT_DIR must be set to the agent workspace path")
	}

	base := filepath.Join(ClaudeProjectsDir(), ProjectSlug(projectDir))
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("no project transcripts at %s: %v", base, err)
	}
	type cand struct {
		sid string
		mod int64
	}
	var cands []cand
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".jsonl" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{sid: n[:len(n)-len(".jsonl")], mod: fi.ModTime().UnixNano()})
	}
	if len(cands) == 0 {
		t.Fatalf("no *.jsonl session files under %s", base)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod > cands[j].mod })

	sid := cands[0].sid
	paths := ResolveSessionJSONL(projectDir, sid)
	if len(paths) == 0 {
		t.Fatalf("ResolveSessionJSONL(%q,%q) resolved no files", projectDir, sid)
	}

	var assistantText, toolUse, raw, total int
	for _, p := range paths {
		rows, err := ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", p, err)
		}
		for _, r := range rows {
			total++
			switch {
			case r.Kind == KindText && r.Role == "assistant":
				assistantText++
			case r.Kind == KindToolUse:
				toolUse++
			case r.Kind == KindRaw:
				raw++
			}
		}
	}
	t.Logf("live session %s: %d files, %d rows (%d assistant-text, %d tool_use, %d raw)",
		sid, len(paths), total, assistantText, toolUse, raw)
	if assistantText < 1 {
		t.Errorf("expected ≥1 assistant text row from real session, got %d", assistantText)
	}
	if toolUse < 1 {
		t.Errorf("expected ≥1 tool_use row from real session, got %d", toolUse)
	}
}
