package parse

import (
	"testing"
)

func TestParseMustDoNestedBuckets(t *testing.T) {
	content := `id: G-001
type: goal
status: active
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: Alpha Go-Live

weight: 40
must_do:
  billing:
  - T-200
  - T-201
  clinical:
  - T-100

sbar:
  situation: |-
    Test goal.
  background: |-
    Background.
  assessment: |-
    Assessment.
  recommendation: |-
    Recommendation.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if item.MustDo == nil {
		t.Fatal("MustDo is nil")
	}
	billing := item.MustDo["billing"]
	if len(billing) != 2 || billing[0] != "T-200" || billing[1] != "T-201" {
		t.Errorf("MustDo[billing] = %v, want [T-200 T-201]", billing)
	}
	clinical := item.MustDo["clinical"]
	if len(clinical) != 1 || clinical[0] != "T-100" {
		t.Errorf("MustDo[clinical] = %v, want [T-100]", clinical)
	}
}

func TestParseMustDoFlatList(t *testing.T) {
	content := `id: G-004
type: goal
status: active
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: st-tooling

weight: 18
must_do:
- T-407
- T-409
- T-410

sbar:
  situation: |-
    Tooling.
  background: |-
    Background.
  assessment: |-
    Assessment.
  recommendation: |-
    Recommendation.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.MustDo == nil {
		t.Fatal("MustDo is nil for flat list")
	}
	flat := item.MustDo[""]
	if len(flat) != 3 || flat[0] != "T-407" || flat[2] != "T-410" {
		t.Errorf("MustDo[\"\"] = %v, want [T-407 T-409 T-410]", flat)
	}
}

func TestParseMustDoEmpty(t *testing.T) {
	content := `id: G-002
type: goal
status: active
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: Compliance

weight: 22
must_do:

sbar:
  situation: |-
    Compliance items.
  background: |-
    Background.
  assessment: |-
    Assessment.
  recommendation: |-
    Recommendation.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// Empty must_do: — MustDo should be nil or empty map, not error.
	if item.MustDo != nil && len(item.MustDo) > 0 {
		t.Errorf("expected empty/nil MustDo for empty must_do: block, got %v", item.MustDo)
	}
}

func TestParseMustDoTerminatesAtSBAR(t *testing.T) {
	// must_do block immediately followed by sbar: — neither block should eat the other.
	content := `id: G-001
type: goal
status: active
created: 2026-05-24T10:00:00-06:00
last_touched: 2026-05-24T10:00:00-06:00

title: Test

weight: 40
must_do:
  clinical:
  - T-100
sbar:
  situation: |-
    Situation text.
  background: |-
    Background text.
  assessment: |-
    Assessment text.
  recommendation: |-
    Recommendation text.
`
	path := writeTempFile(t, content)
	item, err := File(path)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if item.MustDo == nil || len(item.MustDo["clinical"]) != 1 {
		t.Errorf("MustDo[clinical] = %v, want [T-100]", item.MustDo["clinical"])
	}
	if item.SBAR.Situation != "Situation text." {
		t.Errorf("SBAR.Situation = %q, want 'Situation text.'", item.SBAR.Situation)
	}
	if item.SBAR.Background != "Background text." {
		t.Errorf("SBAR.Background = %q, want 'Background text.'", item.SBAR.Background)
	}
}
