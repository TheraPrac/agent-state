package parse

import "testing"

// I-1439: parse.File records duplicate CANONICAL top-level keys in
// item.DuplicateTopLevelKeys so `st check` can fail loud — the parser is
// last-value-wins, so a duplicate key silently drops the earlier value.
// Detection must NOT trip on keys that appear inside block-scalar or
// ```fenced bodies (the T-304 false positive), and is scoped to schema
// keys so the separate bare-`cmd:` legacy-AC format is not flagged.

func TestParseDetectsDuplicateTopLevelKey(t *testing.T) {
	content := `id: I-1
type: issue
status: queued
blocks:
- I-9
last_touched: 2026-06-01T10:00:00-06:00
blocks:
- I-8
`
	item, err := File(writeTempFile(t, content))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if got := item.DuplicateTopLevelKeys; len(got) != 1 || got[0] != "blocks" {
		t.Errorf("DuplicateTopLevelKeys = %v, want [blocks]", got)
	}
}

func TestParseIgnoresKeysInBlockScalarAndFence(t *testing.T) {
	// A canonical key name (`type:`, `status:`) appearing inside an SBAR
	// block-scalar body — including a ```yaml code fence (the T-304 shape:
	// the body is dedented to column 0) — must NOT be counted as a
	// duplicate top-level key.
	content := `id: I-2
type: task
status: done
sbar:
  recommendation: |-
    Mail message format:
` + "```yaml" + `
type: warning
status: blocked
` + "```" + `
    end of example.
last_touched_by: agent-a
`
	item, err := File(writeTempFile(t, content))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.DuplicateTopLevelKeys) != 0 {
		t.Errorf("block/fence body keys must not flag; got %v", item.DuplicateTopLevelKeys)
	}
	if item.Type != "task" {
		t.Errorf("parser must keep the real top-level type=task, got %q", item.Type)
	}
}

func TestParseDoesNotFlagNonCanonicalDuplicate(t *testing.T) {
	// Legacy acceptance_criteria written as bare `cmd:` lines (the I-691
	// format class) repeats a NON-schema key — out of scope for the
	// duplicate-schema-key gate; must not flag.
	content := `id: I-3
type: issue
status: done
acceptance_criteria:
cmd: go build ./...
cmd: go test ./...
`
	item, err := File(writeTempFile(t, content))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.DuplicateTopLevelKeys) != 0 {
		t.Errorf("non-canonical duplicate (cmd) must not flag; got %v", item.DuplicateTopLevelKeys)
	}
}
