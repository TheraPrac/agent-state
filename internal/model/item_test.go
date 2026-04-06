package model

import (
	"testing"
)

func TestParsedDocumentSetField(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "status: queued", Key: "status", Value: "queued"},
			{Raw: "title: Test task", Key: "title", Value: "Test task"},
		},
	}

	// Update existing field
	found := doc.SetField("status", "active")
	if !found {
		t.Error("SetField should find existing 'status' field")
	}
	if doc.Lines[1].Raw != "status: active" {
		t.Errorf("updated line = %q, want %q", doc.Lines[1].Raw, "status: active")
	}
	if doc.Lines[1].Value != "active" {
		t.Errorf("updated value = %q, want %q", doc.Lines[1].Value, "active")
	}
}

func TestParsedDocumentSetFieldPreservesComment(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "stage: coding  # delivery stage", Key: "stage", Value: "coding", Comment: "delivery stage"},
		},
	}

	doc.SetField("stage", "pushed")
	if doc.Lines[0].Raw != "stage: pushed  # delivery stage" {
		t.Errorf("line = %q, want comment preserved", doc.Lines[0].Raw)
	}
}

func TestParsedDocumentSetFieldAppends(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
		},
	}

	found := doc.SetField("priority", "2")
	if found {
		t.Error("SetField should return false for new field")
	}
	if len(doc.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(doc.Lines))
	}
	if doc.Lines[1].Raw != "priority: 2" {
		t.Errorf("appended line = %q, want %q", doc.Lines[1].Raw, "priority: 2")
	}
}

func TestParsedDocumentGetField(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "status: queued", Key: "status", Value: "queued"},
		},
	}

	val, ok := doc.GetField("status")
	if !ok {
		t.Fatal("GetField should find 'status'")
	}
	if val != "queued" {
		t.Errorf("GetField = %q, want %q", val, "queued")
	}

	_, ok = doc.GetField("nonexistent")
	if ok {
		t.Error("GetField should return false for missing field")
	}
}

func TestParsedDocumentString(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001"},
			{Raw: "status: queued"},
			{Raw: ""},
			{Raw: "title: A task"},
		},
	}

	want := "id: T-001\nstatus: queued\n\ntitle: A task"
	got := doc.String()
	if got != want {
		t.Errorf("String() =\n%q\nwant:\n%q", got, want)
	}
}

func TestParsedDocumentStringEmpty(t *testing.T) {
	doc := &ParsedDocument{}
	if doc.String() != "" {
		t.Errorf("empty doc String() = %q, want empty", doc.String())
	}
}

func TestParsedDocumentSetListReplacesExisting(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "tags:", Key: "tags", Value: ""},
			{Raw: "- alpha", IsList: true},
			{Raw: "- beta", IsList: true},
			{Raw: "", IsEmpty: true},
			{Raw: "title: Test", Key: "title", Value: "Test"},
		},
	}

	found := doc.SetList("tags", []string{"alpha", "beta", "gamma"})
	if !found {
		t.Error("SetList should find existing 'tags' field")
	}

	got := doc.String()
	want := "id: T-001\ntags:\n- alpha\n- beta\n- gamma\n\ntitle: Test"
	if got != want {
		t.Errorf("SetList result =\n%s\nwant:\n%s", got, want)
	}
}

func TestParsedDocumentSetListEmptyWritesMarker(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "tags:", Key: "tags", Value: ""},
			{Raw: "- old", IsList: true},
		},
	}

	doc.SetList("tags", nil)
	got := doc.String()
	want := "tags:\n- []"
	if got != want {
		t.Errorf("empty SetList =\n%q\nwant:\n%q", got, want)
	}
}

func TestParsedDocumentSetListAppendsNew(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
		},
	}

	found := doc.SetList("tags", []string{"new-tag"})
	if found {
		t.Error("SetList should return false for new field")
	}
	got := doc.String()
	want := "id: T-001\ntags:\n- new-tag"
	if got != want {
		t.Errorf("new SetList =\n%q\nwant:\n%q", got, want)
	}
}

func TestParsedDocumentSetListClearsInlineValue(t *testing.T) {
	// Simulates the bug: tags written as inline "tags: [alpha, beta]"
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "tags: [alpha, beta]", Key: "tags", Value: "[alpha, beta]"},
			{Raw: "", IsEmpty: true},
			{Raw: "title: Test", Key: "title", Value: "Test"},
		},
	}

	doc.SetList("tags", []string{"alpha", "beta", "gamma"})
	got := doc.String()
	want := "tags:\n- alpha\n- beta\n- gamma\n\ntitle: Test"
	if got != want {
		t.Errorf("inline fix =\n%s\nwant:\n%s", got, want)
	}
}

func TestReplaceList_NestedField_ReplacesInPlace(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "testing_evidence:", Key: "testing_evidence"},
			{Raw: "  api_unit: old_value", Key: "api_unit", Value: "old_value", Indent: 2, BlockKey: "testing_evidence"},
			{Raw: "  api_lint: pass", Key: "api_lint", Value: "pass", Indent: 2, BlockKey: "testing_evidence"},
			{Raw: "title: Test", Key: "title", Value: "Test"},
		},
	}

	doc.ReplaceList("testing_evidence.api_unit", []string{"- line1", "- line2"})

	got := doc.String()
	want := "id: T-001\ntesting_evidence:\n  api_unit:\n  - line1\n  - line2\n  api_lint: pass\ntitle: Test"
	if got != want {
		t.Errorf("ReplaceList nested =\n%s\nwant:\n%s", got, want)
	}
}

func TestReplaceList_TopLevel_StillWorks(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "next_actions:", Key: "next_actions"},
			{Raw: "- old action", BlockKey: "next_actions"},
			{Raw: "title: Test", Key: "title", Value: "Test"},
		},
	}

	doc.ReplaceList("next_actions", []string{"- new action 1", "- new action 2"})

	got := doc.String()
	want := "id: T-001\nnext_actions:\n- new action 1\n- new action 2\ntitle: Test"
	if got != want {
		t.Errorf("ReplaceList top-level =\n%s\nwant:\n%s", got, want)
	}
}

func TestParsedDocumentSetFieldIgnoresNested(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001", Indent: 0},
			{Raw: "  id: nested", Key: "id", Value: "nested", Indent: 2},
		},
	}

	// Should update the top-level one, not the nested one
	doc.SetField("id", "T-002")
	if doc.Lines[0].Value != "T-002" {
		t.Error("should update top-level id")
	}
	if doc.Lines[1].Value != "nested" {
		t.Error("should not modify nested id")
	}
}
