package parse

import "testing"

func TestParseGoalScalars(t *testing.T) {
	content := `id: G-001
type: goal
status: active
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Alpha Go-Live

weight: 40
success_criterion: all alpha-1 must_do items complete

sbar:
  situation: |-
    Alpha launch is blocked on 40 open items.
  background: |-
    Seed goal from the 2026-05-22 prioritization session.
  assessment: |-
    High confidence this is the right top goal.
  recommendation: |-
    Drive all must_do items to done.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.Type != "goal" {
		t.Errorf("Type = %q, want goal", item.Type)
	}
	if item.Weight == nil {
		t.Fatal("Weight is nil, want 40")
	}
	if *item.Weight != 40 {
		t.Errorf("Weight = %d, want 40", *item.Weight)
	}
	if item.SuccessCriterion != "all alpha-1 must_do items complete" {
		t.Errorf("SuccessCriterion = %q", item.SuccessCriterion)
	}

	// Roundtrip: the raw document string must still contain the exact weight line.
	if item.Doc == nil {
		t.Fatal("Doc is nil")
	}
	raw := item.Doc.String()
	if val, ok := item.Doc.GetField("weight"); !ok || val != "40" {
		t.Errorf("Doc.GetField(weight) = %q ok=%v, want 40 true (raw:\n%s)", val, ok, raw)
	}
	if val, _ := item.Doc.GetField("success_criterion"); val != "all alpha-1 must_do items complete" {
		t.Errorf("Doc.GetField(success_criterion) = %q", val)
	}
}

func TestParseGoalWeightZeroOmitted(t *testing.T) {
	content := `id: G-005
type: goal
status: draft
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Unweighted Draft

sbar:
  situation: |-
    Test.
  background: |-
    Test.
  assessment: |-
    Test.
  recommendation: |-
    Test.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Weight != nil {
		t.Errorf("Weight = %d, want nil for missing field", *item.Weight)
	}
}
