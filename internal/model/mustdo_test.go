package model

import (
	"strings"
	"testing"
)

func TestMustDoFieldCanonical(t *testing.T) {
	if !CanonicalTopLevelKeys["must_do"] {
		t.Error("CanonicalTopLevelKeys missing must_do")
	}
}

func TestGoalItemHasMustDo(t *testing.T) {
	w := 40
	it := &Item{
		ID:     "G-001",
		Type:   "goal",
		Status: "active",
		Title:  "Alpha Go-Live",
		Weight: &w,
		MustDo: map[string][]string{
			"clinical": {"T-100", "T-101"},
			"billing":  {"T-200"},
		},
	}
	if len(it.MustDo["clinical"]) != 2 {
		t.Errorf("MustDo[clinical] = %v, want 2 items", it.MustDo["clinical"])
	}
	if it.MustDo["billing"][0] != "T-200" {
		t.Errorf("MustDo[billing][0] = %q, want T-200", it.MustDo["billing"][0])
	}
}

func TestSetMustDoRoundTrip(t *testing.T) {
	// Build a doc with a must_do block, then set a new one and verify.
	d := &ParsedDocument{}
	d.Lines = []Line{
		{Raw: "id: G-001", Key: "id", Value: "G-001"},
		{Raw: "type: goal", Key: "type", Value: "goal"},
		{Raw: "must_do:", Key: "must_do"},
		{Raw: "  clinical:", Key: "clinical", Indent: 2, BlockKey: "must_do"},
		{Raw: "    - T-100", IsList: true, Indent: 4, BlockKey: "clinical"},
		{Raw: ""},
		{Raw: "sbar:", Key: "sbar"},
	}

	newBuckets := map[string][]string{
		"clinical": {"T-100", "T-101"},
		"billing":  {"T-200"},
	}
	d.SetMustDo(newBuckets)

	out := d.String()
	if !strings.Contains(out, "must_do:") {
		t.Error("SetMustDo: missing must_do: header")
	}
	if !strings.Contains(out, "  clinical:") {
		t.Error("SetMustDo: missing clinical bucket")
	}
	if !strings.Contains(out, "    - T-101") {
		t.Error("SetMustDo: missing T-101")
	}
	if !strings.Contains(out, "  billing:") {
		t.Error("SetMustDo: missing billing bucket")
	}
	// sbar block must still be present after must_do.
	if !strings.Contains(out, "sbar:") {
		t.Error("SetMustDo: sbar block was eaten")
	}
}

func TestSetMustDoFlatForm(t *testing.T) {
	// Flat form: only "" bucket.
	d := &ParsedDocument{}
	d.Lines = []Line{
		{Raw: "id: G-004", Key: "id", Value: "G-004"},
		{Raw: "type: goal", Key: "type", Value: "goal"},
	}
	buckets := map[string][]string{
		"": {"T-407", "T-409"},
	}
	d.SetMustDo(buckets)
	out := d.String()
	if !strings.Contains(out, "must_do:") {
		t.Error("flat SetMustDo: missing must_do: header")
	}
	if !strings.Contains(out, "- T-407") {
		t.Error("flat SetMustDo: missing T-407")
	}
	// Flat form must NOT have bucket sub-headers.
	if strings.Contains(out, ":") && strings.Contains(out, "  ") {
		// The only indented colon-containing line should not exist.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "  ") && strings.Contains(line, ":") && !strings.HasPrefix(strings.TrimSpace(line), "- ") {
				// Found an indented key-line — that's a bucket header we shouldn't have.
				t.Errorf("flat SetMustDo: unexpected bucket header line: %q", line)
			}
		}
	}
}

func TestSetMustDoEmpty(t *testing.T) {
	d := &ParsedDocument{}
	d.Lines = []Line{
		{Raw: "id: G-001", Key: "id", Value: "G-001"},
		{Raw: "must_do:", Key: "must_do"},
		{Raw: "  - T-100", IsList: true, Indent: 2, BlockKey: "must_do"},
		{Raw: ""},
		{Raw: "tags:", Key: "tags"},
	}
	d.SetMustDo(nil)
	out := d.String()
	if strings.Contains(out, "must_do:") {
		t.Error("SetMustDo(nil): must_do block should be removed")
	}
	// tags must still be present.
	if !strings.Contains(out, "tags:") {
		t.Error("SetMustDo(nil): tags eaten")
	}
}
