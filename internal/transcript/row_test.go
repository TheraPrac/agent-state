package transcript

import (
	"path/filepath"
	"testing"
)

// TestRowReader parses the captured-and-redacted fixture and asserts the
// typed projection of every Claude Code row shape, plus the
// graceful-degradation contract (never crash, never drop, never error on
// a malformed line — contract §13 finding 1 / operator silent-failure
// principle). This is the Phase-1 acceptance test referenced in
// as/.plans/T-353.md.
func TestRowReader(t *testing.T) {
	rows, err := ReadFile(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var nText, nThinking, nToolUse, nToolResult, nRaw int
	for _, r := range rows {
		switch r.Kind {
		case KindText:
			nText++
		case KindThinking:
			nThinking++
		case KindToolUse:
			nToolUse++
		case KindToolResult:
			nToolResult++
		case KindRaw:
			nRaw++
		default:
			t.Errorf("unexpected Kind %q", r.Kind)
		}
	}

	// fixture content: user prose + "Running..." + "Tests passed." +
	// "undated but valid" = 4 text rows.
	if nText != 4 {
		t.Errorf("text rows = %d, want 4", nText)
	}
	if nThinking != 1 {
		t.Errorf("thinking rows = %d, want 1", nThinking)
	}
	if nToolUse != 2 {
		t.Errorf("tool_use rows = %d, want 2", nToolUse)
	}
	if nToolResult != 3 {
		t.Errorf("tool_result rows = %d, want 3", nToolResult)
	}
	// raw bucket: last-prompt, permission-mode metadata rows + the
	// non-JSON garbage line = 3. The trailing blank line is NOT a row
	// (whitespace is not content).
	if nRaw != 3 {
		t.Errorf("raw rows = %d, want 3 (2 metadata + 1 non-json)", nRaw)
	}

	// Spot-check the first decomposed assistant line: thinking, then
	// text, then tool_use — same timestamp, same role, in order.
	thinking := findKind(rows, KindThinking)
	if thinking == nil || thinking.Text != "I should run make test-unit." {
		t.Fatalf("thinking row = %+v", thinking)
	}
	if thinking.Role != "assistant" {
		t.Errorf("thinking role = %q, want assistant", thinking.Role)
	}
	if thinking.Timestamp.IsZero() {
		t.Error("thinking row lost its timestamp")
	}

	// tool_use typing.
	tu := findKind(rows, KindToolUse)
	if tu == nil || tu.ToolUse == nil {
		t.Fatal("first tool_use row missing ToolUse")
	}
	if tu.ToolUse.ID != "toolu_001" || tu.ToolUse.Name != "Bash" {
		t.Errorf("tool_use = %+v, want id=toolu_001 name=Bash", tu.ToolUse)
	}
	if len(tu.ToolUse.Input) == 0 {
		t.Error("tool_use input not preserved as raw json")
	}

	// tool_result correlation id + error flag + array-content flatten.
	var trOK, trErr, trArr bool
	for _, r := range rows {
		if r.Kind != KindToolResult || r.ToolResult == nil {
			continue
		}
		switch r.ToolResult.ToolUseID {
		case "toolu_001":
			trOK = r.ToolResult.Content == "ok 1 - all tests passed" && !r.ToolResult.IsError
		case "toolu_002":
			trArr = r.ToolResult.Content == "edit applied" // flattened from array form
		case "toolu_003":
			trErr = r.ToolResult.IsError && r.ToolResult.Content == "command failed: exit 1"
		}
	}
	if !trOK {
		t.Error("string tool_result for toolu_001 not flattened/typed correctly")
	}
	if !trArr {
		t.Error("array-form tool_result for toolu_002 not flattened to text")
	}
	if !trErr {
		t.Error("error tool_result for toolu_003 not typed correctly")
	}

	// Every row must keep its original line in Raw (no info loss).
	for i, r := range rows {
		if len(r.Raw) == 0 {
			t.Errorf("row %d (%s) dropped Raw bytes", i, r.Kind)
		}
	}

	// A valid-JSON row with an unparseable timestamp still parses; it
	// just has a zero Timestamp (renders, unordered — never dropped).
	var sawUndated bool
	for _, r := range rows {
		if r.Kind == KindText && r.Text == "undated but valid" {
			sawUndated = true
			if !r.Timestamp.IsZero() {
				t.Error("expected zero timestamp for unparseable RFC3339")
			}
		}
	}
	if !sawUndated {
		t.Error("row with unparseable timestamp was dropped")
	}
}

func TestParseLine_Degradation(t *testing.T) {
	if got := ParseLine([]byte("")); got != nil {
		t.Errorf("blank line → %v, want nil (whitespace is not content)", got)
	}
	if got := ParseLine([]byte("   \t  ")); got != nil {
		t.Errorf("whitespace-only line → %v, want nil", got)
	}
	got := ParseLine([]byte("{not json"))
	if len(got) != 1 || got[0].Kind != KindRaw || got[0].Text != "{not json" {
		t.Errorf("non-json → %+v, want one raw row preserving the line", got)
	}
	// Valid JSON, unrecognized shape → raw, line preserved, not dropped.
	got = ParseLine([]byte(`{"type":"file-history-snapshot","snapshot":{}}`))
	if len(got) != 1 || got[0].Kind != KindRaw {
		t.Errorf("unknown row type → %+v, want one raw row", got)
	}
}

// Code-review hardening: an unknown content block must surface its OWN
// bytes raw (not the whole multi-block enclosing line), and a mixed
// tool_result array must not silently drop its non-text blocks.
func TestParseLine_UnknownBlockAndMixedResult(t *testing.T) {
	line := []byte(`{"type":"assistant","timestamp":"2026-05-18T17:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"image","source":{"data":"BIGBLOB"}}]}}`)
	rows := ParseLine(line)
	var rawRows, textRows int
	for _, r := range rows {
		switch r.Kind {
		case KindRaw:
			rawRows++
			if r.Text == string(line) {
				t.Error("unknown block raw Text is the whole enclosing line, want just the block")
			}
			if got := r.Text; got != `{"type":"image","source":{"data":"BIGBLOB"}}` {
				t.Errorf("unknown block Text = %q, want the block's own JSON", got)
			}
		case KindText:
			textRows++
		}
	}
	if textRows != 1 || rawRows != 1 {
		t.Fatalf("want 1 text + 1 raw row, got %d text %d raw", textRows, rawRows)
	}

	// Mixed tool_result array: text + non-text. The non-text block must
	// appear as a visible marker, not vanish.
	tr := []byte(`{"type":"user","timestamp":"2026-05-18T17:00:01Z","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":[{"type":"text","text":"saw"},{"type":"image"}],"is_error":false}]}}`)
	rows = ParseLine(tr)
	if len(rows) != 1 || rows[0].ToolResult == nil {
		t.Fatalf("want 1 tool_result row, got %+v", rows)
	}
	got := rows[0].ToolResult.Content
	if got != "saw\n[image block]" {
		t.Errorf("mixed-array tool_result Content = %q, want non-text block surfaced as a marker", got)
	}

	// "content":[] is genuinely no output — must flatten to "", not the
	// literal raw "[]" (a Phase 2 renderer keys "no output" off "").
	empty := []byte(`{"type":"user","timestamp":"2026-05-18T17:00:02Z","message":{"role":"user","content":[{"tool_use_id":"t2","type":"tool_result","content":[],"is_error":false}]}}`)
	rows = ParseLine(empty)
	if len(rows) != 1 || rows[0].ToolResult == nil {
		t.Fatalf("want 1 tool_result row, got %+v", rows)
	}
	if rows[0].ToolResult.Content != "" {
		t.Errorf("empty-array tool_result Content = %q, want \"\"", rows[0].ToolResult.Content)
	}
}

func findKind(rows []Row, k Kind) *Row {
	for i := range rows {
		if rows[i].Kind == k {
			return &rows[i]
		}
	}
	return nil
}
