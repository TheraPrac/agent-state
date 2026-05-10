package classify

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBuildPrompt_IncludesAllSections verifies the prompt contains
// every major section the model needs to make its call: criteria,
// item context, files, plan, and an empty-corpus placeholder.
func TestBuildPrompt_IncludesAllSections(t *testing.T) {
	in := Inputs{
		ItemID: "T-345",
		Type:   "task",
		Title:  "AI-based item classifier",
		Repo:   "as",
		Tags:   []string{"st-cli", "agent-tooling"},
		SBAR: SBARInput{
			Situation:      "no autonomy gate",
			Background:     "45 items awaiting approval",
			Assessment:     "need binary classifier",
			Recommendation: "build st classify",
		},
		TouchedFiles: []string{"internal/classify/classifier.go"},
		PlanContent:  "## Goal\nShip phase 1\n",
	}
	out := BuildPrompt(in, nil)

	want := []string{
		"binary autonomy classifier",
		"verdict",
		"green",
		"red",
		"## Item",
		"T-345",
		"AI-based item classifier",
		"agent-tooling, st-cli", // alphabetical sort
		"## SBAR",
		"**Situation:**",
		"no autonomy gate",
		"**Recommendation:**",
		"## Touched files",
		"internal/classify/classifier.go",
		"## Plan",
		"Ship phase 1",
		"## Past operator decisions",
		"corpus is empty",
		"Emit the JSON object now",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("BuildPrompt missing substring %q", w)
		}
	}
}

// TestBuildPrompt_TruncatesLongPlan verifies plans larger than
// MaxPlanContentBytes get the "[…truncated…]" marker. Without this
// guard a 100KB plan could blow the token budget.
func TestBuildPrompt_TruncatesLongPlan(t *testing.T) {
	longPlan := strings.Repeat("x", MaxPlanContentBytes+1000)
	in := Inputs{ItemID: "T-345", Title: "test", PlanContent: longPlan}
	out := BuildPrompt(in, nil)
	if !strings.Contains(out, "[…truncated…]") {
		t.Error("long plan was not truncated")
	}
	// The prompt should still contain a chunk of the original plan.
	if !strings.Contains(out, strings.Repeat("x", 100)) {
		t.Error("truncated plan does not contain the original prefix")
	}
}

// TestBuildPrompt_EmptyFilesSection verifies the wording is reasonable
// when no touched-files are known (e.g., classifier was called on an
// item with no diff yet — the model still needs to render a verdict
// based on plan + SBAR).
func TestBuildPrompt_EmptyFilesSection(t *testing.T) {
	in := Inputs{ItemID: "T-345", Title: "test"}
	out := BuildPrompt(in, nil)
	if !strings.Contains(out, "## Touched files") {
		t.Error("missing files section header")
	}
	if !strings.Contains(out, "(none — classification is from item metadata only)") {
		t.Error("empty-files placeholder missing")
	}
}

// TestBuildPrompt_CorpusExamplesTailWindow verifies the corpus
// section caps at MaxCorpusExamples entries and includes the most
// recent ones (tail of the slice).
func TestBuildPrompt_CorpusExamplesTailWindow(t *testing.T) {
	var entries []CorpusEntry
	for i := 0; i < MaxCorpusExamples+3; i++ {
		entries = append(entries, CorpusEntry{
			ItemID:    "I-" + string(rune('A'+i)),
			DecidedAt: time.Date(2026, 5, i+1, 0, 0, 0, 0, time.UTC),
			Verdict:   VerdictGreen,
		})
	}
	in := Inputs{ItemID: "T-345"}
	out := BuildPrompt(in, entries)

	// The first three (oldest) should be dropped.
	for i := 0; i < 3; i++ {
		drop := "I-" + string(rune('A'+i))
		if strings.Contains(out, drop+" ") {
			t.Errorf("corpus retained dropped older entry %q", drop)
		}
	}
	// The last MaxCorpusExamples should remain.
	for i := 3; i < MaxCorpusExamples+3; i++ {
		keep := "I-" + string(rune('A'+i))
		if !strings.Contains(out, keep) {
			t.Errorf("corpus dropped recent entry %q", keep)
		}
	}
}

// TestParseModelOutput_HappyPath covers the standard expected case:
// the model emits one JSON object with all three fields.
func TestParseModelOutput_HappyPath(t *testing.T) {
	out := `{"verdict": "green", "reason": "doc-only change, no behavior risk", "confidence": 0.95}`
	res, err := ParseModelOutput(out)
	if err != nil {
		t.Fatalf("ParseModelOutput: %v", err)
	}
	if res.Verdict != VerdictGreen {
		t.Errorf("verdict = %s; want green", res.Verdict)
	}
	if !strings.Contains(res.Reason, "doc-only") {
		t.Errorf("reason = %q; want substring 'doc-only'", res.Reason)
	}
	if res.Confidence != 0.95 {
		t.Errorf("confidence = %g; want 0.95", res.Confidence)
	}
	if res.ClassifiedBy != "model:claude" {
		t.Errorf("classified_by = %q; want model:claude", res.ClassifiedBy)
	}
}

// TestParseModelOutput_StripsFencing covers models that wrap in
// markdown code fences despite the instruction.
func TestParseModelOutput_StripsFencing(t *testing.T) {
	out := "```json\n{\"verdict\": \"red\", \"reason\": \"IAM change\", \"confidence\": 0.8}\n```"
	res, err := ParseModelOutput(out)
	if err != nil {
		t.Fatalf("ParseModelOutput: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Errorf("verdict = %s; want red", res.Verdict)
	}
}

// TestParseModelOutput_LastObjectWins covers the defense-in-depth
// case where the model emits multiple objects (e.g., one in chain-of-
// thought and one final answer). The last balanced object is taken.
func TestParseModelOutput_LastObjectWins(t *testing.T) {
	out := `Let me think...
{"thought": "this might be tricky"}
After reflection:
{"verdict": "green", "reason": "small refactor, well-tested", "confidence": 0.7}`
	res, err := ParseModelOutput(out)
	if err != nil {
		t.Fatalf("ParseModelOutput: %v", err)
	}
	if res.Verdict != VerdictGreen {
		t.Errorf("verdict = %s; want green (last object)", res.Verdict)
	}
}

// TestParseModelOutput_RejectsUnknownVerdict guards against the
// model returning something other than green/red. We don't silently
// coerce — an unknown verdict is an error so callers can decide what
// to do (probably retry with the operator on the loop).
func TestParseModelOutput_RejectsUnknownVerdict(t *testing.T) {
	out := `{"verdict": "yellow", "reason": "ambiguous", "confidence": 0.5}`
	_, err := ParseModelOutput(out)
	if err == nil {
		t.Fatal("expected error for unknown verdict, got nil")
	}
	if !strings.Contains(err.Error(), "unknown verdict") {
		t.Errorf("err = %v; want substring 'unknown verdict'", err)
	}
}

// TestParseModelOutput_RejectsEmptyReason guards against the model
// returning a verdict with no justification — the reason is the
// audit trail, it can't be blank.
func TestParseModelOutput_RejectsEmptyReason(t *testing.T) {
	out := `{"verdict": "green", "reason": "", "confidence": 0.9}`
	_, err := ParseModelOutput(out)
	if err == nil {
		t.Fatal("expected error for empty reason, got nil")
	}
	if !strings.Contains(err.Error(), "empty reason") {
		t.Errorf("err = %v; want substring 'empty reason'", err)
	}
}

// TestParseModelOutput_ClampsConfidence verifies values outside
// [0,1] are clamped rather than rejected — the model occasionally
// emits 1.1 or -0.0.
func TestParseModelOutput_ClampsConfidence(t *testing.T) {
	cases := []struct {
		raw  float64
		want float64
	}{
		{raw: 1.5, want: 1.0},
		{raw: -0.5, want: 0.0},
		{raw: 0.5, want: 0.5},
	}
	for _, tc := range cases {
		out := `{"verdict":"green","reason":"x","confidence":` + jsonFloat(tc.raw) + `}`
		res, err := ParseModelOutput(out)
		if err != nil {
			t.Fatalf("raw=%g: %v", tc.raw, err)
		}
		if res.Confidence != tc.want {
			t.Errorf("raw=%g: confidence = %g; want %g", tc.raw, res.Confidence, tc.want)
		}
	}
}

// TestParseModelOutput_NoJSON covers garbage input — no balanced
// object anywhere. Should return a non-nil error, not panic.
func TestParseModelOutput_NoJSON(t *testing.T) {
	out := "I refuse to answer."
	_, err := ParseModelOutput(out)
	if err == nil {
		t.Fatal("expected error for no JSON, got nil")
	}
}

// TestParseModelOutput_LenientUnknownFields covers the case where the
// model adds extra keys like `notes` or `chain_of_thought`. We accept
// the verdict + reason + confidence and ignore the rest.
func TestParseModelOutput_LenientUnknownFields(t *testing.T) {
	out := `{"verdict":"red","reason":"RBAC change","confidence":0.9,"notes":"this is an extra field"}`
	res, err := ParseModelOutput(out)
	if err != nil {
		t.Fatalf("ParseModelOutput: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Errorf("verdict = %s; want red", res.Verdict)
	}
}

// TestParseModelOutput_RejectsMissingConfidence guards against a model
// emitting verdict + reason but omitting confidence. Without this the
// missing field would decode to 0.0 silently and look like a
// low-confidence verdict instead of a schema violation. Symmetric
// with the empty-reason rejection.
func TestParseModelOutput_RejectsMissingConfidence(t *testing.T) {
	out := `{"verdict":"green","reason":"safe doc edit"}`
	_, err := ParseModelOutput(out)
	if err == nil {
		t.Fatal("expected error for missing confidence, got nil")
	}
	if !strings.Contains(err.Error(), "confidence") {
		t.Errorf("err = %v; want substring 'confidence'", err)
	}
}

// TestParseModelOutput_RejectsNullConfidence covers the JSON-null
// case — explicitly written `"confidence": null` should fail the
// same way as an absent field.
func TestParseModelOutput_RejectsNullConfidence(t *testing.T) {
	out := `{"verdict":"green","reason":"safe","confidence":null}`
	_, err := ParseModelOutput(out)
	if err == nil {
		t.Fatal("expected error for null confidence, got nil")
	}
}

// TestFindLastJSONObject_HandlesStringLiterals verifies the byte
// scanner doesn't get tripped up by braces inside string literals.
func TestFindLastJSONObject_HandlesStringLiterals(t *testing.T) {
	s := `prefix {"k": "value with { and } inside"} suffix`
	start, end := findLastJSONObject(s)
	if start < 0 {
		t.Fatal("findLastJSONObject returned no match")
	}
	if !strings.HasPrefix(s[start:end+1], "{") {
		t.Errorf("range starts wrong: %q", s[start:end+1])
	}
	if !strings.HasSuffix(s[start:end+1], "}") {
		t.Errorf("range ends wrong: %q", s[start:end+1])
	}
	if !strings.Contains(s[start:end+1], "value with { and } inside") {
		t.Errorf("range did not capture the full literal: %q", s[start:end+1])
	}
}

// jsonFloat formats a float for JSON test inputs without scientific
// notation, so inputs like -0.5 round-trip as the literal string "-0.5".
func jsonFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
