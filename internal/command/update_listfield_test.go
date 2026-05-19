package command

import (
	"os"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/store"
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
