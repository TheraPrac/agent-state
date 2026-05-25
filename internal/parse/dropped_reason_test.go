package parse

import "testing"

func TestParseDroppedReasonScalar(t *testing.T) {
	content := `id: T-001
type: task
status: abandoned
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Abandoned task

dropped_reason: superseded

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
	if item.DroppedReason != "superseded" {
		t.Errorf("DroppedReason = %q, want superseded", item.DroppedReason)
	}
	if val, ok := item.Doc.GetField("dropped_reason"); !ok || val != "superseded" {
		t.Errorf("Doc.GetField(dropped_reason) = %q ok=%v, want superseded true", val, ok)
	}
}
