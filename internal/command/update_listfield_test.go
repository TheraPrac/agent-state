package command

import (
	"os"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// I-691: `st update <id> <listfield> "<single-line>"` previously fell to
// the scalar SetField path (the list branch required a newline), writing
// `next_actions: text`. The parser's storeScalar has no list-key case, so
// the value was silently dropped on reload. These tests pin the fix: a
// single-line value for a list field round-trips as a one-element list,
// on disk and through a fresh parse — for next_actions AND another list
// field (resolution) to prove it is the listFields branch, not a special
// case.
func TestUpdate_SingleLineListFieldRoundTrips(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	const naVal = "Phase B remaining increment: PostToolUse hook + SessionStart compact"
	const resVal = "Resolved by reverting the bad migration"

	if code := Update(s, cfg, "T-001", "next_actions", naVal, UpdateModeValue); code != 0 {
		t.Fatalf("Update next_actions returned %d, want 0", code)
	}
	if code := Update(s, cfg, "T-001", "resolution", resVal, UpdateModeValue); code != 0 {
		t.Fatalf("Update resolution returned %d, want 0", code)
	}

	// On disk: a proper list block, NOT a scalar. The colon in naVal must
	// force the canonical quoted list-item form (matches migrate.emitList).
	path, _ := s.Path("T-001")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "next_actions: "+naVal) {
		t.Fatalf("next_actions written as a scalar (silent-drop bug), body:\n%s", string(body))
	}
	if !strings.Contains(string(body), "next_actions:\n- \""+naVal+"\"") {
		t.Fatalf("next_actions not in canonical quoted list form, body:\n%s", string(body))
	}
	if !strings.Contains(string(body), "resolution:\n- "+resVal) {
		t.Fatalf("resolution not in list form, body:\n%s", string(body))
	}

	// Through a fresh parse: exactly one element, equal to the value.
	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	item, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001 missing after re-parse")
	}
	if len(item.NextActions) != 1 || item.NextActions[0] != naVal {
		t.Errorf("NextActions = %#v, want exactly [%q]", item.NextActions, naVal)
	}
	if len(item.Resolution) != 1 || item.Resolution[0] != resVal {
		t.Errorf("Resolution = %#v, want exactly [%q]", item.Resolution, resVal)
	}
}

// I-691 review fix: listFields must stay symmetric with the parser's
// list-key set — `tags`/`sessions`/`tests_written` were absent, so a
// single-line `st update <id> tags "x"` previously took the scalar path
// and was dropped. Also exercises listItemRaw's leading-quote branch
// (a value starting with `'` must round-trip, not corrupt).
func TestUpdate_SingleLineListField_TagsAndLeadingQuote(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	const tagVal = "needs-followup"
	const quoted = "'already quoted' value"

	if code := Update(s, cfg, "T-001", "tags", tagVal, UpdateModeValue); code != 0 {
		t.Fatalf("Update tags returned %d, want 0", code)
	}
	if code := Update(s, cfg, "T-001", "related_issues", quoted, UpdateModeValue); code != 0 {
		t.Fatalf("Update related_issues returned %d, want 0", code)
	}

	path, _ := s.Path("T-001")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "tags: "+tagVal) {
		t.Fatalf("tags written as scalar (listFields desync regression):\n%s", string(body))
	}

	s2, err := store.New(cfg)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	item, ok := s2.Get("T-001")
	if !ok {
		t.Fatal("T-001 missing after re-parse")
	}
	if len(item.Tags) != 1 || item.Tags[0] != tagVal {
		t.Errorf("Tags = %#v, want exactly [%q]", item.Tags, tagVal)
	}
	if len(item.RelatedIssues) != 1 || item.RelatedIssues[0] != quoted {
		t.Errorf("RelatedIssues = %#v, want exactly [%q] (leading-quote round-trip)", item.RelatedIssues, quoted)
	}
}

// I-698: ReplaceList must consume the whole prior malformed block. When
// plan-approve writes ACs as flat 'cmd:' lines (no '- ' markers), a
// subsequent `st update <id> acceptance_criteria --stdin` must fully replace
// them — not prepend the new list and leave the flat lines in the file.
func TestUpdateAC_ReplaceMalformedFlatBlock(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	// Inject a malformed AC block: acceptance_criteria followed by bare 'cmd:'
	// lines (no '- ' markers) — exactly what plan-approve used to write.
	if err := s.Mutate("T-001", func(item *model.Item) error {
		item.Doc.Lines = append(item.Doc.Lines,
			model.Line{Raw: "acceptance_criteria:", Key: "acceptance_criteria"},
			model.Line{Raw: "cmd: check 1", Key: "cmd"},
			model.Line{Raw: "cmd: check 2", Key: "cmd"},
		)
		return nil
	}); err != nil {
		t.Fatalf("inject malformed block: %v", err)
	}

	// Replace via multi-line value (same code path as --stdin).
	newAC := "- cmd: npm run test\n- cmd: go test ./..."
	if code := Update(s, cfg, "T-001", "acceptance_criteria", newAC, UpdateModeValue); code != 0 {
		t.Fatalf("Update acceptance_criteria returned %d, want 0", code)
	}

	path, _ := s.Path("T-001")
	body, _ := os.ReadFile(path)
	bodyStr := string(body)

	// Malformed flat lines must be gone.
	if strings.Contains(bodyStr, "check 1") || strings.Contains(bodyStr, "check 2") {
		t.Errorf("malformed flat AC lines survived replacement:\n%s", bodyStr)
	}
	// Both replacement list items must be present.
	if !strings.Contains(bodyStr, "- cmd: npm run test") {
		t.Errorf("replacement AC item 1 missing:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "- cmd: go test ./...") {
		t.Errorf("replacement AC item 2 missing:\n%s", bodyStr)
	}
	// No duplicate acceptance_criteria: headers.
	if strings.Count(bodyStr, "acceptance_criteria:") != 1 {
		t.Errorf("expected exactly 1 acceptance_criteria: header, got %d:\n%s",
			strings.Count(bodyStr, "acceptance_criteria:"), bodyStr)
	}
}

// I-691 review fix (regression guard): `tests_written` must NOT be in
// listFields. It is a LIST on read but lives nested under
// `testing_evidence:`; routing it through the top-level ReplaceList path
// would APPEND a second, orphaned top-level `tests_written:` block (the
// indent-0-only ReplaceList never finds the nested one) — structural
// corruption. This pins the deliberate WRITE⊂READ asymmetry: a generic
// `st update <id> tests_written "x"` must fall to the scalar path and
// never emit a top-level `tests_written:` list block.
func TestUpdate_TestsWrittenNotRoutedThroughReplaceList(t *testing.T) {
	s, cfg := setupTestEnvWithChangelog(t)

	if listFields["tests_written"] {
		t.Fatal("tests_written must NOT be in listFields (nested key — ReplaceList would corrupt the file)")
	}

	if code := Update(s, cfg, "T-001", "tests_written", "internal/foo_test.go", UpdateModeValue); code != 0 {
		t.Fatalf("Update tests_written returned %d, want 0", code)
	}

	path, _ := s.Path("T-001")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "tests_written:\n-") {
		t.Fatalf("tests_written written as a top-level list block (corruption — must be scalar/nested):\n%s", string(body))
	}
	if strings.Count(string(body), "tests_written:") > 1 {
		t.Fatalf("duplicate tests_written blocks (corruption):\n%s", string(body))
	}
}
