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

	// No input row is silently dropped: every row maps to output except
	// the one matched tool_result (folded into its tool_use line).
	if len(got) < len(rows)-1 {
		t.Errorf("rendered %d lines from %d rows — a row was dropped", len(got), len(rows))
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
