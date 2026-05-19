package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTailReader(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")

	// Missing file → nil, never a crash (agent may not have started).
	miss := NewTailReader(filepath.Join(dir, "nope.jsonl"))
	if got := miss.Read(); got != nil {
		t.Errorf("missing file Read = %v, want nil", got)
	}

	line1 := `{"type":"assistant","timestamp":"2026-05-18T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}

	// FromStart reads existing content; a second Read with no new data
	// returns nil.
	tr := NewTailReaderFromStart(p)
	rows := tr.Read()
	if len(rows) != 1 || rows[0].Kind != KindText || rows[0].Text != "first" {
		t.Fatalf("first Read = %+v, want one 'first' text row", rows)
	}
	if got := tr.Read(); got != nil {
		t.Errorf("no-new-data Read = %v, want nil", got)
	}

	// Append a complete line + a PARTIAL line (no trailing newline):
	// only the complete line is consumed; the partial is held.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line2 := `{"type":"assistant","timestamp":"2026-05-18T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}` + "\n"
	partial := `{"type":"assistant","timestamp":"2026-05-18T10:00:02Z","message":{"role":"ass`
	if _, err := f.WriteString(line2 + partial); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rows = tr.Read()
	if len(rows) != 1 || rows[0].Text != "second" {
		t.Fatalf("after append Read = %+v, want only the complete 'second' row (partial held)", rows)
	}
	if got := tr.Read(); got != nil {
		t.Errorf("partial-only Read = %v, want nil (still incomplete)", got)
	}

	// Complete the partial line → now it parses, exactly once.
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`istant","content":[{"type":"text","text":"third"}]}}` + "\n")
	f.Close()
	rows = tr.Read()
	if len(rows) != 1 || rows[0].Text != "third" {
		t.Fatalf("completed-line Read = %+v, want 'third'", rows)
	}

	// Truncation/rotation: file shrinks below the offset → restart from
	// 0, re-read whatever is now there (nothing missed silently).
	if err := os.WriteFile(p, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}
	rows = tr.Read()
	if len(rows) != 1 || rows[0].Text != "first" {
		t.Fatalf("post-truncation Read = %+v, want re-read 'first'", rows)
	}
}
