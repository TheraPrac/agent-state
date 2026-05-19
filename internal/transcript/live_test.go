package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// TestLiveRenderThisSession is the Phase-2 ground-truth check: pipe a
// real on-disk session through Render and confirm it produces the
// readable shape (collapsed tool lines + prose) and drops nothing.
// Same env gate / CI-skip discipline as TestLiveThisSession.
func TestLiveRenderThisSession(t *testing.T) {
	if os.Getenv("TRANSCRIPT_LIVE") == "" {
		t.Skip("set TRANSCRIPT_LIVE=1 to run the live render check")
	}
	projectDir := os.Getenv("TRANSCRIPT_LIVE_PROJECT_DIR")
	if projectDir == "" {
		t.Fatal("TRANSCRIPT_LIVE_PROJECT_DIR must be set")
	}
	base := filepath.Join(ClaudeProjectsDir(), ProjectSlug(projectDir))
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("no project transcripts at %s: %v", base, err)
	}
	var newest string
	var newestMod int64
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".jsonl" {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().UnixNano() > newestMod {
			newestMod, newest = fi.ModTime().UnixNano(), n[:len(n)-len(".jsonl")]
		}
	}
	if newest == "" {
		t.Fatalf("no *.jsonl under %s", base)
	}

	var tagged []TaggedRow
	useIDs := map[string]bool{} // tool_use ids present (for fold accounting)
	for i, p := range ResolveSessionJSONL(projectDir, newest) {
		rows, err := ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", p, err)
		}
		tag := "A"
		if i > 0 {
			tag = "a-" + strconv.Itoa(i+1)
		}
		for _, r := range rows {
			if r.Kind == KindToolUse && r.ToolUse != nil && r.ToolUse.ID != "" {
				useIDs[r.ToolUse.ID] = true
			}
			tagged = append(tagged, TaggedRow{Tag: tag, Row: r})
		}
	}
	out := Render(tagged, RenderOpts{})

	// EXACT no-drop oracle (independently derived from the Phase 1 rows
	// + the documented contract, not from Render's output — so a
	// dropped prose row is caught, not hidden by multi-line expansion).
	// Per-row line count: text/thinking → splitProse segments (default
	// opts show thinking); tool_use/raw → 1; the FIRST result per
	// matched id → 0 (folded), every other result (orphan/dup) → 1.
	foldedSeen := map[string]bool{}
	expected, folded := 0, 0
	for _, tr := range tagged {
		r := tr.Row
		switch r.Kind {
		case KindText, KindThinking:
			expected += len(splitProse(r.Text))
		case KindToolUse:
			expected++
		case KindToolResult:
			id := ""
			if r.ToolResult != nil {
				id = r.ToolResult.ToolUseID
			}
			if useIDs[id] && !foldedSeen[id] {
				foldedSeen[id] = true
				folded++ // folded → 0 lines
			} else {
				expected++ // orphan or dup → rendered
			}
		default: // KindRaw
			expected++
		}
	}

	var bashLine, prose bool
	for _, l := range out {
		if strings.Contains(l, "Bash: ") && strings.Contains(l, " → ") {
			bashLine = true
		}
		// a prose line: tagged, and not a tool/orphan/raw-marker line
		if !prose && !strings.Contains(l, " → ") && !strings.Contains(l, "⟵ tool_result") && !strings.Contains(l, " ⏎ ") {
			if idx := strings.Index(l, "] "); idx > 0 && strings.TrimSpace(l[idx+2:]) != "" {
				prose = true
			}
		}
	}
	t.Logf("live render: %d input rows → %d output lines (%d folded, expected %d)",
		len(tagged), len(out), folded, expected)
	if !bashLine {
		t.Error("expected ≥1 collapsed `Bash: … → …` line from real session")
	}
	if !prose {
		t.Error("expected ≥1 prose line from real session")
	}
	if len(out) != expected {
		t.Errorf("rendered %d lines, expected exactly %d (independent per-row oracle) — a row was dropped or double-counted", len(out), expected)
	}
}
