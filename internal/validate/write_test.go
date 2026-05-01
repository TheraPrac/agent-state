package validate

import (
	"errors"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
)

// testCfg returns a minimal config sufficient for write-time vocab checks.
func testCfg() *config.Config {
	return &config.Config{
		Types: map[string]config.TypeConfig{
			"task": {
				Statuses:         []string{"queued", "active", "done", "abandoned", "archived"},
				StartStatus:      "queued",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"done", "abandoned", "archived"},
				RequiredFields:   []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"queued": "tasks", "active": "tasks", "done": "archive", "abandoned": "archive",
				},
			},
			"issue": {
				Statuses:         []string{"queued", "active", "done", "abandoned", "archived"},
				StartStatus:      "queued",
				ActiveStatus:     "active",
				TerminalStatuses: []string{"done", "abandoned", "archived"},
				RequiredFields:   []string{"depends_on", "blocks"},
				DirectoryMap: map[string]string{
					"queued": "issues", "active": "issues", "done": "archive", "abandoned": "archive",
				},
			},
		},
	}
}

// writeOKItem returns a non-terminal task with all required fields
// present. Tests mutate it to drive specific failure cases. Named
// distinctly from validItem in validate_test.go to avoid collision.
func writeOKItem() *model.Item {
	doc := &model.ParsedDocument{
		Lines: []model.Line{
			{Raw: "id: T-100", Key: "id", Value: "T-100"},
			{Raw: "type: task", Key: "type", Value: "task"},
			{Raw: "status: active", Key: "status", Value: "active"},
			{Raw: "title: stub", Key: "title", Value: "stub"},
			{Raw: "depends_on:", Key: "depends_on"},
			{Raw: "- []", IsList: true},
			{Raw: "blocks:", Key: "blocks"},
			{Raw: "- []", IsList: true},
		},
	}
	return &model.Item{
		ID:     "T-100",
		Type:   "task",
		Status: "active",
		Title:  "stub",
		Doc:    doc,
	}
}

// (writeOKItem doesn't take a *testing.T because it does no fixture
// setup; the helper is pure. Tests pass `t` only for assertions.)

func TestWriteOK_ValidItem(t *testing.T) {
	if err := WriteOK(writeOKItem(), testCfg()); err != nil {
		t.Errorf("valid item rejected: %v", err)
	}
}

func TestWriteOK_RejectsUnknownType(t *testing.T) {
	it := writeOKItem()
	it.Type = "bogus"
	err := WriteOK(it, testCfg())
	if !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("want ErrInvalidWrite, got %v", err)
	}
	if !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("error should mention unknown type; got %s", err)
	}
}

func TestWriteOK_RejectsInvalidStatusForType(t *testing.T) {
	it := writeOKItem()
	it.Status = "open" // legacy alias
	err := WriteOK(it, testCfg())
	if !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("want ErrInvalidWrite, got %v", err)
	}
	if !strings.Contains(err.Error(), `did you mean "queued"`) {
		t.Errorf("error should suggest queued for legacy 'open'; got %s", err)
	}
}

func TestWriteOK_LegacyResolvedSuggestsDone(t *testing.T) {
	it := writeOKItem()
	it.Status = "resolved"
	err := WriteOK(it, testCfg())
	if !strings.Contains(err.Error(), `did you mean "done"`) {
		t.Errorf("legacy 'resolved' should suggest 'done'; got %s", err)
	}
}

func TestWriteOK_NonAliasInvalidStatusHasNoSuggestion(t *testing.T) {
	it := writeOKItem()
	it.Status = "purplish"
	err := WriteOK(it, testCfg())
	if !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("want ErrInvalidWrite, got %v", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("non-alias should not produce a suggestion; got %s", err)
	}
}

// TestWriteOKDelta_RejectsRemovingRequiredField verifies the regression
// guard: a Mutate that drops a previously-present required field must
// reject. Pre-existing missing fields are not retroactively rejected
// (covered by the legacy-update test below).
func TestWriteOKDelta_RejectsRemovingRequiredField(t *testing.T) {
	before := writeOKItem()
	after := writeOKItem()
	// Strip blocks from the post-state — simulate a buggy mutation
	// that would drop a required field.
	stripped := after.Doc.Lines[:0]
	for i, l := range after.Doc.Lines {
		if l.Key == "blocks" {
			continue
		}
		// Drop the "- []" continuation that follows blocks.
		if i > 0 && after.Doc.Lines[i-1].Key == "blocks" {
			continue
		}
		stripped = append(stripped, l)
	}
	after.Doc.Lines = stripped

	err := WriteOKDelta(before, after, testCfg())
	if !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("dropping required field should reject; got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be removed once present") {
		t.Errorf("error should mention regression guard; got %s", err)
	}
}

// TestWriteOKDelta_AllowsLegacyItemMissingField verifies that a Mutate
// on an item that ALREADY lacks a required field (legacy / pre-I-508
// data) still goes through. This is the "don't freeze recovery" rule.
func TestWriteOKDelta_AllowsLegacyItemMissingField(t *testing.T) {
	before := writeOKItem()
	after := writeOKItem()
	// Strip blocks from BOTH — the field was never present.
	stripBlocks := func(it *model.Item) {
		stripped := it.Doc.Lines[:0]
		for i, l := range it.Doc.Lines {
			if l.Key == "blocks" {
				continue
			}
			if i > 0 && it.Doc.Lines[i-1].Key == "blocks" {
				continue
			}
			stripped = append(stripped, l)
		}
		it.Doc.Lines = stripped
	}
	stripBlocks(before)
	stripBlocks(after)
	// Mutation only changes priority — totally unrelated to blocks.
	after.Doc.SetField("priority", "1")

	if err := WriteOKDelta(before, after, testCfg()); err != nil {
		t.Errorf("legacy item with pre-existing missing field should permit unrelated update; got %v", err)
	}
}

func TestWriteOK_RejectsMissingRequiredField(t *testing.T) {
	it := writeOKItem()
	// Strip blocks from the parsed doc.
	var kept []model.Line
	for _, l := range it.Doc.Lines {
		if l.Key == "blocks" {
			continue
		}
		// Also drop the list continuation that follows blocks.
		kept = append(kept, l)
	}
	// Remove the "- []" line that was under blocks.
	stripped := kept[:0]
	skipNext := false
	for i, l := range kept {
		if skipNext {
			skipNext = false
			continue
		}
		if i+1 < len(kept) && l.Key == "depends_on" && kept[i+1].IsList {
			stripped = append(stripped, l, kept[i+1])
			skipNext = true
			continue
		}
		stripped = append(stripped, l)
	}
	it.Doc.Lines = stripped

	err := WriteOK(it, testCfg())
	if !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("missing blocks should reject; got %v", err)
	}
	if !strings.Contains(err.Error(), "blocks") {
		t.Errorf("error should name missing field; got %s", err)
	}
}

func TestWriteOK_TerminalItemExemptFromRequiredFields(t *testing.T) {
	// Archived items predate field requirements; stripping required
	// fields on a terminal item must not block a write (e.g., a
	// migration or archive move).
	it := writeOKItem()
	it.Status = "done"
	// Strip everything from doc except the minimum to keep validate
	// happy on type/status; intentionally drop required fields.
	it.Doc.Lines = []model.Line{
		{Raw: "id: T-100", Key: "id", Value: "T-100"},
		{Raw: "type: task", Key: "type", Value: "task"},
		{Raw: "status: done", Key: "status", Value: "done"},
		{Raw: "title: stub", Key: "title", Value: "stub"},
	}
	if err := WriteOK(it, testCfg()); err != nil {
		t.Errorf("terminal item should be exempt from required-fields gate; got %v", err)
	}
}

func TestWriteOK_AggregatesMultipleErrors(t *testing.T) {
	it := writeOKItem()
	it.Type = "bogus"
	it.Status = "open"
	err := WriteOK(it, testCfg())
	we, ok := err.(*WriteError)
	if !ok {
		t.Fatalf("want *WriteError, got %T", err)
	}
	if len(we.Errors) < 1 {
		t.Errorf("expected at least one error, got %d", len(we.Errors))
	}
}
