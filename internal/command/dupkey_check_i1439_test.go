package command

import (
	"path/filepath"
	"testing"
)

// I-1439: `st check` must fail loud (count an issue) for an item whose
// frontmatter carries a duplicate canonical top-level key — the parser
// keeps only the last value, so the corruption is otherwise invisible to
// schema validation. parse.File flags it; Check surfaces it.
func TestCheckFlagsDuplicateTopLevelKey(t *testing.T) {
	_, cfg := setupTestEnv(t)

	// Same body twice except for a stray second `status:` — a duplicate
	// canonical key. Baseline item is otherwise well-formed.
	corrupt := `id: I-900
type: issue
status: queued
created: 2026-03-25T10:00:00-06:00
last_touched: 2026-03-25T10:00:00-06:00
last_touched: 2026-04-01T10:00:00-06:00
depends_on:
- []
blocks:
- []
sbar:
  situation: a real one-line situation for the substance gate
`
	writeFile(t, filepath.Join(cfg.Root(), "issues", "I-900-x.md"), corrupt)
	s := newStoreOrFail(t, cfg)

	// The store must carry the parse-derived diagnostic.
	item, ok := s.Get("I-900")
	if !ok {
		t.Fatal("I-900 not loaded")
	}
	if len(item.DuplicateTopLevelKeys) != 1 || item.DuplicateTopLevelKeys[0] != "last_touched" {
		t.Fatalf("store item must carry DuplicateTopLevelKeys=[last_touched], got %v", item.DuplicateTopLevelKeys)
	}

	// And `st check` must surface it as a non-zero issue count.
	if issues := Check(s, cfg, true, false); issues == 0 {
		t.Error("Check must report a non-zero issue count for a duplicate-key item")
	}
}
