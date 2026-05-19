package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func tu(name string, input map[string]any) *ToolUse {
	b, _ := json.Marshal(input)
	return &ToolUse{Name: name, Input: b}
}

// TestRender is the Phase-2 acceptance test: a deterministic multi-agent
// stream → exact golden output. Covers interleaved tags, tool_use↔
// tool_result collapse + suppression, pending tool_use, orphan result,
// thinking gating, multi-line prose, and raw passthrough.
func TestRender(t *testing.T) {
	bashUse := tu("Bash", map[string]any{"command": "make build", "description": "build"})
	bashUse.ID = "tu1"
	editUse := tu("Edit", map[string]any{
		"file_path": "/x/y/handler.go", "old_string": "a\nb", "new_string": "a\nb\nc\nd",
	})
	editUse.ID = "tu2" // no matching result → pending

	rows := []TaggedRow{
		{"A", Row{Kind: KindText, Role: "assistant", Text: "Starting the build."}},
		{"A", Row{Kind: KindToolUse, ToolUse: bashUse}},
		{"a-2", Row{Kind: KindText, Role: "assistant", Text: "Working in parallel."}},
		{"A", Row{Kind: KindToolResult, ToolResult: &ToolResult{ToolUseID: "tu1", Content: "build ok\nmore"}}},
		{"A", Row{Kind: KindThinking, Role: "assistant", Text: "deciding next step"}},
		{"a-2", Row{Kind: KindToolUse, ToolUse: editUse}},
		{"A", Row{Kind: KindToolResult, ToolResult: &ToolResult{ToolUseID: "ORPHAN", Content: "stale", IsError: true}}},
		{"a-2", Row{Kind: KindRaw, Text: `{"type":"permission-mode"}`}},
		{"A", Row{Kind: KindText, Role: "assistant", Text: "line1\nline2"}},
	}

	want := []string{
		"[A] Starting the build.",
		"[A] Bash: make build → build ok",
		"[a-2] Working in parallel.",
		"[A] deciding next step",
		"[a-2] Edit: handler.go +4 −2 → …",
		"[A] ⟵ tool_result(orphan id=ORPHAN, error): stale",
		`[a-2] {"type":"permission-mode"}`,
		"[A] line1",
		"[A] line2",
	}
	got := Render(rows, RenderOpts{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Render mismatch:\n got=%#v\nwant=%#v", got, want)
	}

	// HideThinking drops exactly the reasoning line, nothing else.
	gotHidden := Render(rows, RenderOpts{HideThinking: true})
	if len(gotHidden) != len(want)-1 {
		t.Fatalf("HideThinking: %d lines, want %d", len(gotHidden), len(want)-1)
	}
	for _, l := range gotHidden {
		if strings.Contains(l, "deciding next step") {
			t.Error("HideThinking did not suppress the thinking line")
		}
	}

	// No silent drop, precisely: the golden DeepEqual above already
	// pins exact output. The only folded row is the single matched
	// tu1 result; the ORPHAN result still renders. Assert that exact
	// fold count so a future regression that drops an orphan/dup is
	// caught (the weak `< len-1` form could not).
	const foldedMatchedResults = 1 // tu1 only
	nonFolded := len(rows) - foldedMatchedResults
	wantLines := nonFolded + 1 // last KindText "line1\nline2" → 2 lines
	if len(got) != wantLines {
		t.Errorf("rendered %d lines, want exactly %d (rows=%d, folded=%d, +1 multi-line prose)",
			len(got), wantLines, len(rows), foldedMatchedResults)
	}
}

// TestRender_DuplicateResultNotDropped locks the Phase-2 review fix: a
// second tool_result for an already-folded id must render as a dup
// line, never be silently swallowed (silent-failure principle).
func TestRender_DuplicateResultNotDropped(t *testing.T) {
	use := tu("Bash", map[string]any{"command": "go test"})
	use.ID = "d1"
	rows := []TaggedRow{
		{"A", Row{Kind: KindToolUse, ToolUse: use}},
		{"A", Row{Kind: KindToolResult, ToolResult: &ToolResult{ToolUseID: "d1", Content: "first"}}},
		{"A", Row{Kind: KindToolResult, ToolResult: &ToolResult{ToolUseID: "d1", Content: "second", IsError: true}}},
	}
	got := Render(rows, RenderOpts{})
	if len(got) != 2 {
		t.Fatalf("want 2 lines (tool_use+dup), got %#v", got)
	}
	if got[0] != "[A] Bash: go test → first" {
		t.Errorf("tool_use line = %q", got[0])
	}
	if got[1] != "[A] ⟵ tool_result(dup id=d1, error): second" {
		t.Errorf("dup line not rendered/labelled: %q", got[1])
	}
}

func TestRender_MultiEditAndNullArgAndNilToolUse(t *testing.T) {
	// MultiEdit: schema is {file_path, edits:[{old_string,new_string}]}.
	me := tu("MultiEdit", map[string]any{
		"file_path": "/p/q/svc.go",
		"edits": []map[string]any{
			{"old_string": "a", "new_string": "x\ny"},
			{"old_string": "b\nc", "new_string": "z"},
		},
	})
	me.ID = "m1"
	got := Render([]TaggedRow{{"A", Row{Kind: KindToolUse, ToolUse: me}}}, RenderOpts{})
	if got[0] != "[A] MultiEdit: svc.go ×2 +3 −3 → …" {
		t.Errorf("MultiEdit summary wrong: %q", got[0])
	}

	// null-valued arg is skipped, not surfaced as literal "null".
	mys := tu("Mystery", map[string]any{"a": nil, "b": "real"})
	mys.ID = "y1"
	got = Render([]TaggedRow{{"A", Row{Kind: KindToolUse, ToolUse: mys}}}, RenderOpts{})
	if got[0] != "[A] Mystery: real → …" {
		t.Errorf("null arg not skipped: %q", got[0])
	}

	// KindToolUse with nil ToolUse → visible marker, never a blank line.
	got = Render([]TaggedRow{{"A", Row{Kind: KindToolUse}}}, RenderOpts{})
	if got[0] != "[A] <tool_use: missing ToolUse payload>" {
		t.Errorf("nil ToolUse not surfaced visibly: %q", got[0])
	}
}

func TestRender_Summarizers(t *testing.T) {
	cases := []struct {
		name string
		use  *ToolUse
		res  *ToolResult
		want string
	}{
		{"bash-ok", tu("Bash", map[string]any{"command": "go test"}),
			&ToolResult{Content: "PASS\nok"}, "[?] Bash: go test → PASS"},
		{"bash-err", tu("Bash", map[string]any{"command": "go vet"}),
			&ToolResult{IsError: true}, "[?] Bash: go vet → error"},
		{"bash-pending", tu("Bash", map[string]any{"command": "sleep 1"}),
			nil, "[?] Bash: sleep 1 → …"},
		{"write", tu("Write", map[string]any{"file_path": "/a/b/x.go", "content": "p\nq\nr"}),
			&ToolResult{}, "[?] Write: x.go (3 lines) → ok"},
		{"read", tu("Read", map[string]any{"file_path": "/a/b/x.go"}),
			&ToolResult{}, "[?] Read: x.go → ok"},
		{"grep", tu("Grep", map[string]any{"pattern": "func .*Render"}),
			&ToolResult{}, "[?] Grep: func .*Render → ok"},
		{"glob", tu("Glob", map[string]any{"pattern": "**/*.go"}),
			&ToolResult{}, "[?] Glob: **/*.go → ok"},
		{"task", tu("Task", map[string]any{"description": "review diff", "subagent_type": "x"}),
			&ToolResult{}, "[?] Task: review diff → ok"},
		{"unknown-firstscalar", tu("Mystery", map[string]any{"zeta": "last", "alpha": "first"}),
			&ToolResult{}, "[?] Mystery: first → ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.use.ID = "X"
			in := []TaggedRow{{"", Row{Kind: KindToolUse, ToolUse: c.use}}}
			if c.res != nil {
				c.res.ToolUseID = "X"
				in = append(in, TaggedRow{"", Row{Kind: KindToolResult, ToolResult: c.res}})
			}
			got := Render(in, RenderOpts{})
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("got %#v, want [%q]", got, c.want)
			}
		})
	}
}

func TestRender_TruncationIsVisible(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := Render([]TaggedRow{{"A", Row{Kind: KindRaw, Text: long}}}, RenderOpts{MaxLen: 50})
	if len(got) != 1 {
		t.Fatalf("want 1 line, got %v", got)
	}
	if !strings.Contains(got[0], "…[+150b]") {
		t.Errorf("truncation marker not visible: %q", got[0])
	}
	if strings.HasPrefix(got[0], "[A] ") == false {
		t.Errorf("tag prefix lost: %q", got[0])
	}
}

func TestRender_CollapseAndTimestamps(t *testing.T) {
	r := Row{Kind: KindRaw, Text: "first\nsecond\nthird"}
	got := Render([]TaggedRow{{"a-2", r}}, RenderOpts{})
	if got[0] != "[a-2] first ⏎ second ⏎ third" {
		t.Errorf("collapse wrong: %q", got[0])
	}

	ts := parseTS("2026-05-18T17:04:05Z")
	if ts.IsZero() {
		t.Fatal("bad fixture time")
	}
	got = Render([]TaggedRow{{"A", Row{Kind: KindText, Timestamp: ts, Text: "hi"}}},
		RenderOpts{Timestamps: true})
	if got[0] != "[A] 17:04:05 hi" {
		t.Errorf("timestamp prefix wrong: %q", got[0])
	}
}
