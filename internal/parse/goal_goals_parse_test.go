package parse

import (
	"strings"
	"testing"
)

const itemWithGoals = `id: T-001
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

completed: null

title: Task with goals

priority: 2

goals:
- G-001
- G-004

depends_on:
- []

next_actions:
- []
`

func TestParseItemGoalsList(t *testing.T) {
	path := writeTempFile(t, itemWithGoals)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.Goals) != 2 {
		t.Fatalf("Goals = %v, want [G-001 G-004]", item.Goals)
	}
	if item.Goals[0] != "G-001" || item.Goals[1] != "G-004" {
		t.Errorf("Goals = %v, want [G-001 G-004]", item.Goals)
	}
}

func TestParseItemGoalsEmpty(t *testing.T) {
	content := `id: T-002
type: task
status: queued
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: Task without goals

priority: 2
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if len(item.Goals) != 0 {
		t.Errorf("Goals = %v, want [] for item with no goals field", item.Goals)
	}
}

func TestParseItemGoalsLosslessRoundtrip(t *testing.T) {
	path := writeTempFile(t, itemWithGoals)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.Doc == nil {
		t.Fatal("Doc is nil")
	}
	raw := item.Doc.String()
	if !strings.Contains(raw, "goals:") {
		t.Errorf("roundtrip lost goals key:\n%s", raw)
	}
	if !strings.Contains(raw, "G-001") || !strings.Contains(raw, "G-004") {
		t.Errorf("roundtrip lost goal IDs:\n%s", raw)
	}
}
