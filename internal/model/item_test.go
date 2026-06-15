package model

import (
	"strings"
	"testing"
)

func TestParsedDocumentRemoveNestedField(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "assigned_to_meta:", Key: "assigned_to_meta"},
			{Raw: "  parent_id: agent-a", Key: "parent_id", Value: "agent-a", Indent: 2, BlockKey: "assigned_to_meta"},
			{Raw: "  role: reviewer", Key: "role", Value: "reviewer", Indent: 2, BlockKey: "assigned_to_meta"},
			{Raw: "title: Test", Key: "title", Value: "Test"},
		},
	}

	if !doc.RemoveNestedField("assigned_to_meta.parent_id") {
		t.Fatal("RemoveNestedField should return true for existing key")
	}
	rendered := doc.String()
	if strings.Contains(rendered, "parent_id:") {
		t.Errorf("parent_id should be removed; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "role: reviewer") {
		t.Errorf("role should remain; got:\n%s", rendered)
	}

	// Removing the last child should also remove the now-empty parent
	if !doc.RemoveNestedField("assigned_to_meta.role") {
		t.Fatal("RemoveNestedField should return true for role")
	}
	rendered = doc.String()
	if strings.Contains(rendered, "assigned_to_meta") {
		t.Errorf("empty assigned_to_meta parent should be removed; got:\n%s", rendered)
	}
}

func TestParsedDocumentRemoveNestedField_Missing(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
		},
	}
	if doc.RemoveNestedField("assigned_to_meta.parent_id") {
		t.Error("RemoveNestedField on missing key should return false")
	}
}

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

// TestParsedDocumentSetFieldReplacesBlock is the I-385a regression test:
// when the existing field is rendered as a YAML block scalar (`key: |-`
// followed by indented continuation lines), SetField must remove the
// stale continuation lines instead of leaving them as orphans below the
// updated key line.
func TestParsedDocumentSetFieldReplacesBlock(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "description: |-", Key: "description"},
			{Raw: "  old line one", IsBlock: true, BlockKey: "description", Indent: 2},
			{Raw: "  old line two", IsBlock: true, BlockKey: "description", Indent: 2},
			{Raw: "  old line three", IsBlock: true, BlockKey: "description", Indent: 2},
			{Raw: "title: T", Key: "title", Value: "T"},
		},
	}

	doc.SetField("description", "fresh value")
	got := doc.String()
	if strings.Contains(got, "old line") {
		t.Errorf("orphaned block lines remain after SetField:\n%s", got)
	}
	if !strings.Contains(got, "description: fresh value") {
		t.Errorf("expected `description: fresh value`, got:\n%s", got)
	}
	if !strings.Contains(got, "title: T") {
		t.Errorf("title line lost during splice:\n%s", got)
	}
}

// TestParsedDocumentSetFieldMultilineEmitsBlockScalar verifies that a
// multi-line value writes YAML block-scalar form so a roundtrip parser
// sees a single field, not a key-then-stray-text sequence.
func TestParsedDocumentSetFieldMultilineEmitsBlockScalar(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
		},
	}

	doc.SetField("description", "first\nsecond\nthird")
	got := doc.String()
	want := "id: T-001\ndescription: |-\n  first\n  second\n  third"
	if got != want {
		t.Errorf("SetField multiline =\n%q\nwant:\n%q", got, want)
	}
}

// TestParsedDocumentSetFieldRepeatedReplaceMultiline reproduces I-385:
// repeatedly setting a long-form field with multi-line content must NOT
// accumulate stale copies of the prior value in the body.
func TestParsedDocumentSetFieldRepeatedReplaceMultiline(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
		},
	}

	for _, v := range []string{"alpha\nalphaB", "beta\nbetaB", "gamma\ngammaB"} {
		doc.SetField("description", v)
	}
	got := doc.String()
	for _, stale := range []string{"alpha", "beta"} {
		if strings.Contains(got, stale) {
			t.Errorf("stale value %q remains after repeated SetField:\n%s", stale, got)
		}
	}
	if !strings.Contains(got, "gamma") {
		t.Errorf("latest value missing:\n%s", got)
	}
}

// TestParsedDocumentAppendToNestedListNewParent is the I-185 regression:
// when the parent block doesn't exist, AppendToNestedList must splice
// it before the body separator (or at end if none), not after the
// markdown body where the parser will lose it on the next read.
func TestParsedDocumentAppendToNestedListNewParent(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "title: T", Key: "title", Value: "T"},
			{Raw: "---", Indent: 0},
			{Raw: "# T-001"},
			{Raw: "Body content."},
		},
	}

	doc.AppendToNestedList("testing_evidence", "api_unit", "first run")
	got := doc.String()

	// Verify ordering: testing_evidence block must precede the --- separator.
	idxBlock := strings.Index(got, "testing_evidence:")
	idxSep := strings.Index(got, "\n---\n")
	if idxBlock < 0 {
		t.Fatalf("testing_evidence not appended:\n%s", got)
	}
	if idxBlock > idxSep {
		t.Errorf("testing_evidence appended AFTER body separator (should be before):\n%s", got)
	}
}

// TestParsedDocumentAppendToNestedListNoDuplicates ensures repeated
// appends update the existing list rather than each creating a new
// `testing_evidence:` parent block (the original bug seen in T-272).
func TestParsedDocumentAppendToNestedListNoDuplicates(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: T-001", Key: "id", Value: "T-001"},
			{Raw: "---", Indent: 0},
			{Raw: "Body."},
		},
	}

	for i := 0; i < 3; i++ {
		doc.AppendToNestedList("testing_evidence", "tests_written", "run")
	}
	got := doc.String()
	if c := strings.Count(got, "testing_evidence:"); c != 1 {
		t.Errorf("testing_evidence parent appears %d times, want 1:\n%s", c, got)
	}
	if c := strings.Count(got, "tests_written:"); c != 1 {
		t.Errorf("tests_written key appears %d times, want 1:\n%s", c, got)
	}
}

// TestItemSetNestedSyncsMapAndDoc verifies the convenience method on
// Item updates both the in-memory map (read by validate, deps, list,
// etc.) and the document.
func TestItemSetNestedSyncsMapAndDoc(t *testing.T) {
	item := &Item{
		Doc: &ParsedDocument{
			Lines: []Line{{Raw: "id: T-001", Key: "id"}},
		},
	}

	item.SetNested("delivery", "stage", "merged")

	if item.Delivery["stage"] != "merged" {
		t.Errorf("Delivery[stage] = %v, want merged", item.Delivery["stage"])
	}
	got, ok := item.Doc.GetNestedField("delivery.stage")
	if !ok || got != "merged" {
		t.Errorf("Doc nested = %q ok=%v, want merged true", got, ok)
	}
}

// TestSetNestedFieldInsertMultiLine reproduces I-668: both insert branches of
// SetNestedField must emit valid YAML when the value contains newlines.
func TestSetNestedFieldInsertMultiLine(t *testing.T) {
	t.Run("child-not-found", func(t *testing.T) {
		doc := &ParsedDocument{
			Lines: []Line{
				{Raw: "id: T-001", Key: "id", Value: "T-001"},
				{Raw: "sbar:", Key: "sbar"},
				{Raw: "  situation: existing", Key: "situation", Value: "existing", Indent: 2, BlockKey: "sbar"},
			},
		}
		doc.SetNestedField("sbar.background", "line1\nline2")
		got := doc.String()
		// A bare newline inside a scalar is invalid YAML — the block body
		// lines must be separate indented lines under a `key: |-` header.
		if strings.Contains(got, "background: line1\nline2") {
			t.Errorf("raw newline in scalar: output is invalid YAML:\n%s", got)
		}
		if !strings.Contains(got, "background: |-") {
			t.Errorf("expected block-scalar header 'background: |-' in:\n%s", got)
		}
		if !strings.Contains(got, "    line1") || !strings.Contains(got, "    line2") {
			t.Errorf("expected indented block body lines in:\n%s", got)
		}
		// GetNestedField must reconstruct the value from block-body lines.
		val, ok := doc.GetNestedField("sbar.background")
		if !ok || val != "line1\nline2" {
			t.Errorf("GetNestedField round-trip: got %q ok=%v, want %q true", val, ok, "line1\nline2")
		}
		// RemoveNestedField must clean up body lines, not just the header.
		doc.RemoveNestedField("sbar.background")
		for _, l := range doc.Lines {
			if l.IsBlock && l.BlockKey == "background" {
				t.Errorf("orphaned block-body line after RemoveNestedField: %q", l.Raw)
			}
		}
	})

	t.Run("parent-not-found", func(t *testing.T) {
		doc := &ParsedDocument{
			Lines: []Line{
				{Raw: "id: T-001", Key: "id", Value: "T-001"},
			},
		}
		doc.SetNestedField("new_parent.new_child", "alpha\nbeta")
		got := doc.String()
		if strings.Contains(got, "new_child: alpha\nbeta") {
			t.Errorf("raw newline in scalar: output is invalid YAML:\n%s", got)
		}
		if !strings.Contains(got, "new_child: |-") {
			t.Errorf("expected block-scalar header 'new_child: |-' in:\n%s", got)
		}
		if !strings.Contains(got, "    alpha") || !strings.Contains(got, "    beta") {
			t.Errorf("expected indented block body lines in:\n%s", got)
		}
		val, ok := doc.GetNestedField("new_parent.new_child")
		if !ok || val != "alpha\nbeta" {
			t.Errorf("GetNestedField round-trip: got %q ok=%v, want %q true", val, ok, "alpha\nbeta")
		}
		doc.RemoveNestedField("new_parent.new_child")
		for _, l := range doc.Lines {
			if l.IsBlock && l.BlockKey == "new_child" {
				t.Errorf("orphaned block-body line after RemoveNestedField: %q", l.Raw)
			}
		}
	})
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

func TestHasDuplicateDeliveryBlock_NoDuplicate(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: I-001", Key: "id"},
			{Raw: "delivery:", Key: "delivery"},
			{Raw: "  stage: coding", Key: "stage", Indent: 2, BlockKey: "delivery"},
		},
	}
	if doc.HasDuplicateDeliveryBlock() {
		t.Error("should be false when only one delivery block")
	}
}

func TestHasDuplicateDeliveryBlock_WithDuplicate(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: I-001", Key: "id"},
			{Raw: "delivery:", Key: "delivery"},
			{Raw: "  stage: uat_approved", Key: "stage", Indent: 2, BlockKey: "delivery"},
			{Raw: "other: foo", Key: "other"},
			{Raw: "delivery:", Key: "delivery"},
			{Raw: "  stage: coding", Key: "stage", Indent: 2, BlockKey: "delivery"},
		},
	}
	if !doc.HasDuplicateDeliveryBlock() {
		t.Error("should be true when two delivery blocks present")
	}
}

func TestRemoveDuplicateDeliveryBlock_KeepsFirst(t *testing.T) {
	doc := &ParsedDocument{
		Lines: []Line{
			{Raw: "id: I-001", Key: "id"},
			{Raw: "delivery:", Key: "delivery"},
			{Raw: "  stage: uat_approved", Key: "stage", Indent: 2, BlockKey: "delivery"},
			{Raw: "other: foo", Key: "other"},
			{Raw: "delivery:", Key: "delivery"},
			{Raw: "  stage: coding", Key: "stage", Indent: 2, BlockKey: "delivery"},
		},
	}
	n := doc.RemoveDuplicateDeliveryBlock()
	if n == 0 {
		t.Fatal("expected lines to be removed")
	}
	got := doc.String()
	if strings.Contains(got, "stage: coding") {
		t.Error("second delivery block should be removed")
	}
	if !strings.Contains(got, "stage: uat_approved") {
		t.Error("first delivery block should be preserved")
	}
	if !strings.Contains(got, "other: foo") {
		t.Error("non-delivery field should be preserved")
	}
	if doc.HasDuplicateDeliveryBlock() {
		t.Error("should have no duplicate after removal")
	}
}
