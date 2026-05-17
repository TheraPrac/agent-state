package model

import "testing"

// I-593: SetNestedField overwriting a `key: |-` block scalar must drop
// the old block body, not strand it beneath the new value.

func TestSetNestedField_StripsBlockContinuation_SingleLine(t *testing.T) {
	// Post-corruption shape: the situation header was already collapsed
	// to a single-line scalar by the buggy setter, leaving its old
	// block body orphaned at Indent 4. A following sub-field and a
	// genuine top-level key must survive untouched.
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: stale collapsed value", Key: "situation", Value: "stale collapsed value", Indent: 2, BlockKey: "sbar"},
		{Raw: "    orphan body A", Indent: 4},
		{Raw: "    orphan body B", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "    bg body", IsBlock: true, BlockKey: "background", Indent: 4},
		{Raw: "blocks:", Key: "blocks"},
	}}

	if !doc.SetNestedField("sbar.situation", "fresh value") {
		t.Fatal("SetNestedField should find sbar.situation")
	}

	want := "sbar:\n" +
		"  situation: fresh value\n" +
		"  background: |-\n" +
		"    bg body\n" +
		"blocks:"
	if got := doc.String(); got != want {
		t.Errorf("orphan body not stripped.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetNestedField_StripsBlockContinuation_MultiLine(t *testing.T) {
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    old line 1", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "    old line 2", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "    bg body", IsBlock: true, BlockKey: "background", Indent: 4},
		{Raw: "blocks:", Key: "blocks"},
	}}

	if !doc.SetNestedField("sbar.situation", "para one\npara two") {
		t.Fatal("SetNestedField should find sbar.situation")
	}

	// Multi-line value must round-trip as a nested block scalar, not
	// collapse onto one line.
	want := "sbar:\n" +
		"  situation: |-\n" +
		"    para one\n" +
		"    para two\n" +
		"  background: |-\n" +
		"    bg body\n" +
		"blocks:"
	if got := doc.String(); got != want {
		t.Errorf("multi-line nested value mishandled.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetNestedField_DoesNotConsumeFollowingNestedField(t *testing.T) {
	// Generic (non-sbar) nested usage: a single-line child followed by
	// a sibling. No block body to strip; the sibling must be intact.
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "work_tracking:", Key: "work_tracking"},
		{Raw: "  branch: old-branch", Key: "branch", Value: "old-branch", Indent: 2, BlockKey: "work_tracking"},
		{Raw: "  pr: 42", Key: "pr", Value: "42", Indent: 2, BlockKey: "work_tracking"},
		{Raw: "status: queued", Key: "status", Value: "queued"},
	}}

	if !doc.SetNestedField("work_tracking.branch", "new-branch") {
		t.Fatal("SetNestedField should find work_tracking.branch")
	}
	want := "work_tracking:\n" +
		"  branch: new-branch\n" +
		"  pr: 42\n" +
		"status: queued"
	if got := doc.String(); got != want {
		t.Errorf("sibling/following field disturbed.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// I-593: SetSBARBlock's end-of-block boundary must absorb I-487 col-0
// dedented garbage (including garbage mis-parsed with a spurious Key)
// and duplicate orphaned sub-field headers, stopping only at the next
// recognized top-level schema key.

func TestSetSBARBlock_HealsColZeroGarbageAndDuplicateHeaders(t *testing.T) {
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: leading clean sit", Key: "situation", Value: "leading clean sit", Indent: 2, BlockKey: "sbar"},
		{Raw: "  background: leading clean bg", Key: "background", Value: "leading clean bg", Indent: 2, BlockKey: "sbar"},
		// I-487 dedented col-0 prose:
		{Raw: "PROBLEM", Indent: 0},
		{Raw: "some dedented narrative line", Indent: 0},
		// garbage line containing a stray colon -> spurious Key:
		{Raw: "T-182 path: PostClientCharge etc", Key: "T-182 path", Value: "PostClientCharge etc", Indent: 0},
		// duplicate orphaned sub-field headers + bodies:
		{Raw: "  assessment: |-", Key: "assessment", Indent: 2, BlockKey: "sbar"},
		{Raw: "    stale asmt body", IsBlock: true, BlockKey: "assessment", Indent: 4},
		{Raw: "  recommendation: |-", Key: "recommendation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    stale rec body", IsBlock: true, BlockKey: "recommendation", Indent: 4},
		// next genuine field — must survive:
		{Raw: "blocks:", Key: "blocks"},
		{Raw: "- T-197", IsList: true, BlockKey: "blocks"},
	}}

	doc.SetSBARBlock(SBAR{
		Situation:      "S",
		Background:     "B",
		Assessment:     "A",
		Recommendation: "R",
	})

	want := "sbar:\n" +
		"  situation: |-\n    S\n" +
		"  background: |-\n    B\n" +
		"  assessment: |-\n    A\n" +
		"  recommendation: |-\n    R\n" +
		"\n" +
		"blocks:\n" +
		"- T-197"
	if got := doc.String(); got != want {
		t.Errorf("corrupt sbar not healed.\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetSBARBlock_CleanCaseRegressionAndIdempotent(t *testing.T) {
	// A well-formed sbar immediately followed by a genuine key must
	// rebuild canonically and be byte-stable across repeated calls
	// (the editor flow saves through this writer on every SBAR edit).
	base := &ParsedDocument{Lines: []Line{
		{Raw: "title: x", Key: "title", Value: "x"},
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    sit", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "    bg", IsBlock: true, BlockKey: "background", Indent: 4},
		{Raw: "  assessment: |-", Key: "assessment", Indent: 2, BlockKey: "sbar"},
		{Raw: "    as", IsBlock: true, BlockKey: "assessment", Indent: 4},
		{Raw: "  recommendation: |-", Key: "recommendation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    rec", IsBlock: true, BlockKey: "recommendation", Indent: 4},
		{Raw: "", IsEmpty: true},
		{Raw: "blocks:", Key: "blocks"},
	}}
	s := SBAR{Situation: "sit", Background: "bg", Assessment: "as", Recommendation: "rec"}

	base.SetSBARBlock(s)
	first := base.String()

	want := "title: x\n" +
		"sbar:\n" +
		"  situation: |-\n    sit\n" +
		"  background: |-\n    bg\n" +
		"  assessment: |-\n    as\n" +
		"  recommendation: |-\n    rec\n" +
		"\n" +
		"blocks:"
	if first != want {
		t.Errorf("clean-case rebuild not canonical.\n got:\n%s\nwant:\n%s", first, want)
	}

	base.SetSBARBlock(s) // idempotency
	if second := base.String(); second != first {
		t.Errorf("SetSBARBlock not idempotent.\n first:\n%s\nsecond:\n%s", first, second)
	}
}
