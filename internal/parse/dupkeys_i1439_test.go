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

func TestParseIgnoresDedentedBlockKeyLookalike(t *testing.T) {
	// I-487 signature: an UNFENCED block-scalar body dedented to column 0
	// containing a line that looks like a canonical key (`status:`). The
	// dedent terminates the block and the line falls into the key branch,
	// but it must NOT be flagged as a duplicate — the file is healthy
	// dedented prose, not duplicate-key corruption.
	content := `id: I-7
type: task
status: queued
sbar:
  recommendation: |-
    Design note about the workflow.
status: blocked is just dedented prose, not a field
last_touched_by: agent-a
`
	item, err := File(writeTempFile(t, content))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.DuplicateTopLevelKeys) != 0 {
		t.Errorf("dedented block prose must not flag as duplicate; got %v", item.DuplicateTopLevelKeys)
	}
}

func TestParseDetectsDuplicateAfterBlockScalar(t *testing.T) {
	// Guard against the false negative the dedent-suppression could
	// introduce: a genuine duplicate whose FIRST occurrence terminates a
	// block scalar must still be caught on its SECOND (non-block-ending)
	// occurrence. Here last_touched_by ends the sbar block, then recurs.
	content := `id: I-8
type: task
status: done
sbar:
  recommendation: |-
    prose body of the recommendation.
last_touched_by: agent-a
delivery:
  stage: closed
last_touched_by: agent-b
`
	item, err := File(writeTempFile(t, content))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if got := item.DuplicateTopLevelKeys; len(got) != 1 || got[0] != "last_touched_by" {
		t.Errorf("genuine duplicate after a block scalar must be caught; got %v", got)
	}
}
