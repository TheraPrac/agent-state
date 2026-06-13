package model

import (
	"strings"
	"testing"
)

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

// Finding B (PR #106 review): the rebuilt nested header must carry
// BlockKey=parent, matching what the parser assigns on re-parse, so an
// in-session SetNestedField followed by RemoveNestedField (which
// matches on BlockKey==parent) still works.
func TestSetNestedField_RoundTripEnablesRemove(t *testing.T) {
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    old", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "    bg", IsBlock: true, BlockKey: "background", Indent: 4},
		{Raw: "blocks:", Key: "blocks"},
	}}
	if !doc.SetNestedField("sbar.situation", "fresh") {
		t.Fatal("SetNestedField should find sbar.situation")
	}
	// Header line must carry BlockKey == "sbar".
	for _, l := range doc.Lines {
		if l.Key == "situation" && l.Indent == 2 && l.BlockKey != "sbar" {
			t.Fatalf("rebuilt situation header BlockKey = %q, want \"sbar\"", l.BlockKey)
		}
	}
	if !doc.RemoveNestedField("sbar.situation") {
		t.Error("RemoveNestedField failed in-session after SetNestedField (BlockKey not set)")
	}
}

// Finding A (PR #106 review): a structurally CLEAN sbar block followed
// immediately by a non-canonical legacy freeform field must NOT have
// that field consumed by SetSBARBlock. No dedented prose => boundary is
// the first Indent==0 line regardless of whether its key is canonical.
func TestSetSBARBlock_CleanBlockThenLegacyKeyNotConsumed(t *testing.T) {
	doc := &ParsedDocument{Lines: []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    s", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "    b", IsBlock: true, BlockKey: "background", Indent: 4},
		{Raw: "  assessment: |-", Key: "assessment", Indent: 2, BlockKey: "sbar"},
		{Raw: "    a", IsBlock: true, BlockKey: "assessment", Indent: 4},
		{Raw: "  recommendation: |-", Key: "recommendation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    r", IsBlock: true, BlockKey: "recommendation", Indent: 4},
		// legacy, non-canonical, NOT in CanonicalTopLevelKeys:
		{Raw: "impact: a real legacy field value", Key: "impact", Value: "a real legacy field value"},
		{Raw: "root_cause: also real", Key: "root_cause", Value: "also real"},
	}}
	doc.SetSBARBlock(SBAR{Situation: "s", Background: "b", Assessment: "a", Recommendation: "r"})
	got := doc.String()
	if !strings.Contains(got, "impact: a real legacy field value") ||
		!strings.Contains(got, "root_cause: also real") {
		t.Errorf("clean-block legacy field was consumed by SetSBARBlock:\n%s", got)
	}
}

// The CanonicalTopLevelKeys set must mirror exactly what internal/parse
// recognizes as top-level fields. Update both together if the parser
// changes. (Sub-keys situation/background/assessment/recommendation and
// the testing_evidence-nested required_suites/scope_suites are NOT
// top-level and are intentionally excluded.)
func TestCanonicalTopLevelKeys_MatchesParser(t *testing.T) {
	want := map[string]bool{
		// storeScalar
		"id": true, "type": true, "status": true, "title": true,
		"created": true, "last_touched": true, "completed": true,
		"priority": true, "severity": true, "category": true, "repo": true,
		"assigned_to": true, "last_touched_by": true, "epic": true,
		"sprint": true, "arc": true, "scope_class": true,
		"claimed_by": true, "claimed_at": true,
		"plan_approved": true, "plan_approved_at": true,
		"plan_approved_by": true, "parallel_group": true,
		"weight": true, "success_criterion": true,
		"dropped_reason": true, "hotfix": true,
		"plan_written_at": true, "plan_failed_at": true, "plan_failure_reason": true,
		// storeList
		"tags": true, "depends_on": true, "blocks": true,
		"related_issues": true, "acceptance_criteria": true,
		"next_actions": true, "resolution": true, "invariants": true,
		"doc_changes": true, "sessions": true, "linked_plans": true,
		"tests_written": true, "goals": true,
		// storeListOfMaps / storeNestedScalar top-level parents
		"testing_evidence": true, "work_tracking": true, "delivery": true,
		"time_tracking": true, "manifest": true, "sbar": true,
		// storeMultiline top-level
		"summary": true, "context": true,
	}
	if !reflectDeepEqualStringSet(want, CanonicalTopLevelKeys) {
		t.Errorf("CanonicalTopLevelKeys drifted from the parser-recognized set.\n got:  %v\n want: %v",
			CanonicalTopLevelKeys, want)
	}
}

func reflectDeepEqualStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
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

// I-670: SBARIsScalarCorrupted distinguishes a canonical 4-section
// mapping (bare `sbar:` header) from the post-bug flattened forms
// (`sbar: |-` block scalar / `sbar: <inline>`), keying off the header
// line alone so prose body lines containing a `key:` cannot fool it.
func TestSBARIsScalarCorrupted(t *testing.T) {
	cleanMapping := []Line{
		{Raw: "sbar:", Key: "sbar"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
		{Raw: "    real situation", IsBlock: true, BlockKey: "situation", Indent: 4},
		{Raw: "  background: |-", Key: "background", Indent: 2, BlockKey: "sbar"},
		{Raw: "  assessment: |-", Key: "assessment", Indent: 2, BlockKey: "sbar"},
		{Raw: "  recommendation: |-", Key: "recommendation", Indent: 2, BlockKey: "sbar"},
	}
	blockScalar := []Line{
		{Raw: "sbar: |-", Key: "sbar"},
		{Raw: "  situation: |-", Indent: 2, BlockKey: "sbar"},
		{Raw: "  this is all one flattened string now", Indent: 2, BlockKey: "sbar"},
	}
	inlineScalar := []Line{
		{Raw: "sbar: a one-line flattened value", Key: "sbar", Value: "a one-line flattened value"},
	}
	commentedHeader := []Line{
		{Raw: "sbar:  # legacy note", Key: "sbar", Comment: "legacy note"},
		{Raw: "  situation: |-", Key: "situation", Indent: 2, BlockKey: "sbar"},
	}
	noSBAR := []Line{
		{Raw: "title: no sbar here", Key: "title", Value: "no sbar here"},
	}

	cases := []struct {
		name  string
		lines []Line
		want  bool
	}{
		{"clean mapping", cleanMapping, false},
		{"block scalar |-", blockScalar, true},
		{"inline scalar", inlineScalar, true},
		{"bare header with comment", commentedHeader, false},
		{"no sbar line", noSBAR, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &ParsedDocument{Lines: c.lines}
			if got := d.SBARIsScalarCorrupted(); got != c.want {
				t.Errorf("SBARIsScalarCorrupted() = %v, want %v", got, c.want)
			}
		})
	}
}
