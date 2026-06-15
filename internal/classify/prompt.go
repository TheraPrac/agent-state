package classify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// verdictPayload is the JSON schema the model is asked to emit.
// Decoded by ParseModelOutput; classifier validates verdict/confidence
// before constructing a Result. Confidence is *float64 (pointer) so
// we can distinguish absent (nil) from explicit 0.0, and reject the
// former — a model that emits a verdict without a confidence value
// is violating the schema, not asserting low confidence.
type verdictPayload struct {
	Verdict    string   `json:"verdict"`
	Reason     string   `json:"reason"`
	Confidence *float64 `json:"confidence"`
}

func decodeVerdictPayload(s string) (verdictPayload, error) {
	var p verdictPayload
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		// Try again allowing unknown fields — some models add extra
		// keys like `notes`. We tolerate that rather than fail hard.
		var pLenient verdictPayload
		if err2 := json.Unmarshal([]byte(s), &pLenient); err2 == nil {
			return pLenient, nil
		}
		return verdictPayload{}, err
	}
	return p, nil
}

// MaxPlanContentBytes caps the plan-content portion of the prompt to
// keep token cost bounded on long plans. Plans larger than this are
// truncated with a "[…truncated…]" marker so the model still sees the
// shape of the plan.
const MaxPlanContentBytes = 4 * 1024

// MaxCorpusExamples caps how many past operator decisions get folded
// in as in-context examples per call. Recent-tail wins, since the
// classifier's policy intent drifts over time and the freshest
// signals are the most predictive.
const MaxCorpusExamples = 5

// BuildPrompt assembles the full classifier prompt from the inputs
// and a list of feedback examples. Pure string assembly — no IO, no
// external state, deterministic for any given (in, examples) pair.
// The model is expected to emit exactly one JSON object on stdout
// matching the schema documented in the prompt; ParseModelOutput
// (in claude_model.go) handles the response side.
func BuildPrompt(in Inputs, examples []CorpusEntry) string {
	var b strings.Builder

	b.WriteString("You are the binary autonomy classifier for the agent delivery loop.\n")
	b.WriteString("Your job: given an item that's about to be auto-shipped by an agent, decide\n")
	b.WriteString("whether the change is safe to run end-to-end without operator intervention.\n\n")

	b.WriteString("OUTPUT FORMAT — single JSON object, no prose, no markdown fences:\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": \"green\" | \"red\",\n")
	b.WriteString("  \"reason\": \"<one paragraph, ~2-4 sentences, explaining the call>\",\n")
	b.WriteString("  \"confidence\": 0.0 .. 1.0\n")
	b.WriteString("}\n\n")

	b.WriteString("CRITERIA:\n")
	b.WriteString("- green: scope is narrow, blast radius is contained, change is reversible,\n")
	b.WriteString("  tests + lint cover the behavior, no security-critical surface.\n")
	b.WriteString("  Examples: doc edits, test-only changes, one-line bug fixes, internal\n")
	b.WriteString("  refactors that don't change public contracts.\n")
	b.WriteString("- red: ANY of the following — RBAC / auth / access changes; IAM,\n")
	b.WriteString("  secrets, or terraform-state edits; destructive DB migrations\n")
	b.WriteString("  (DROP/ALTER TYPE); cross-cutting refactors with ambiguous intent;\n")
	b.WriteString("  changes whose blast radius you cannot bound from the diff.\n")
	b.WriteString("- If you're uncertain, the answer is red. The cost of a wrong green\n")
	b.WriteString("  is much higher than the cost of a wrong red.\n\n")

	writeItemSection(&b, in)
	writeFilesSection(&b, in.TouchedFiles, in.DiffLineCount)
	writePlanSection(&b, in.PlanContent)
	writeExamplesSection(&b, examples)

	b.WriteString("Emit the JSON object now. Do NOT include any other text.\n")
	return b.String()
}

func writeItemSection(b *strings.Builder, in Inputs) {
	b.WriteString("## Item\n")
	b.WriteString(fmt.Sprintf("- id: %s\n", in.ItemID))
	b.WriteString(fmt.Sprintf("- type: %s\n", in.Type))
	b.WriteString(fmt.Sprintf("- title: %s\n", in.Title))
	if in.Repo != "" {
		b.WriteString(fmt.Sprintf("- repo: %s\n", in.Repo))
	}
	if len(in.Tags) > 0 {
		tags := append([]string(nil), in.Tags...)
		sort.Strings(tags)
		b.WriteString(fmt.Sprintf("- tags: %s\n", strings.Join(tags, ", ")))
	}
	if !in.SBAR.isEmpty() {
		b.WriteString("\n## SBAR\n")
		writeSBARField(b, "Situation", in.SBAR.Situation)
		writeSBARField(b, "Background", in.SBAR.Background)
		writeSBARField(b, "Assessment", in.SBAR.Assessment)
		writeSBARField(b, "Recommendation", in.SBAR.Recommendation)
	}
	b.WriteString("\n")
}

func writeSBARField(b *strings.Builder, label, content string) {
	if content == "" {
		return
	}
	b.WriteString(fmt.Sprintf("**%s:**\n%s\n\n", label, strings.TrimSpace(content)))
}

func (s SBARInput) isEmpty() bool {
	return s.Situation == "" && s.Background == "" && s.Assessment == "" && s.Recommendation == ""
}

func writeFilesSection(b *strings.Builder, files []string, diffLineCount int) {
	b.WriteString("## Touched files\n")
	if len(files) == 0 {
		b.WriteString("(none — classification is from item metadata only)\n\n")
		return
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	for _, f := range sorted {
		b.WriteString(fmt.Sprintf("- %s\n", f))
	}
	if diffLineCount > 0 && diffLineCount != len(files) {
		b.WriteString(fmt.Sprintf("\n(approx %d lines of diff across these files)\n", diffLineCount))
	}
	b.WriteString("\n")
}

func writePlanSection(b *strings.Builder, content string) {
	if content == "" {
		b.WriteString("## Plan\n(no plan sidecar)\n\n")
		return
	}
	b.WriteString("## Plan\n")
	if len(content) <= MaxPlanContentBytes {
		b.WriteString(content)
	} else {
		b.WriteString(content[:MaxPlanContentBytes])
		b.WriteString("\n[…truncated…]\n")
	}
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeExamplesSection(b *strings.Builder, examples []CorpusEntry) {
	if len(examples) == 0 {
		b.WriteString("## Past operator decisions\n(none yet — corpus is empty; T-346's `st decide` will populate it)\n\n")
		return
	}
	// Tail-window of most recent decisions.
	if len(examples) > MaxCorpusExamples {
		examples = examples[len(examples)-MaxCorpusExamples:]
	}
	b.WriteString("## Past operator decisions (most recent first)\n")
	b.WriteString("Use these as guidance — they reflect how the operator wants\n")
	b.WriteString("similar changes classified going forward.\n\n")
	// Iterate newest→oldest so the model sees the freshest cues first.
	for i := len(examples) - 1; i >= 0; i-- {
		e := examples[i]
		b.WriteString(fmt.Sprintf("- %s (%s): verdict=%s, action=%s",
			e.ItemID, e.DecidedAt.Format(time.RFC3339), e.Verdict, e.OperatorAction))
		if e.OperatorReason != "" {
			b.WriteString(fmt.Sprintf(", reason: %s", e.OperatorReason))
		}
		if len(e.TouchedFiles) > 0 {
			b.WriteString(fmt.Sprintf("\n  touched: %s", strings.Join(e.TouchedFiles, ", ")))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// ParseModelOutput parses the model's stdout into a Result. The model
// is asked to emit exactly one JSON object; we look for the LAST
// balanced JSON object in the output (defense against models that
// preface with stray text despite the instruction).
//
// Returned Result has Verdict/Reason/Confidence/ClassifiedBy set;
// ClassifiedAt and InputHash are stamped by Classifier.Classify (the
// pure orchestrator), not here.
func ParseModelOutput(out string) (Result, error) {
	// Strip optional ```json fencing.
	s := strings.TrimSpace(out)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	start, end := findLastJSONObject(s)
	if start < 0 {
		return Result{}, fmt.Errorf("no JSON object in model output: %q", truncateForError(s))
	}
	payload := s[start : end+1]

	parsed, err := decodeVerdictPayload(payload)
	if err != nil {
		return Result{}, fmt.Errorf("decode JSON %q: %w", truncateForError(payload), err)
	}

	v := Verdict(strings.ToLower(strings.TrimSpace(parsed.Verdict)))
	if v != VerdictGreen && v != VerdictRed {
		return Result{}, fmt.Errorf("model returned unknown verdict %q (want green|red)", parsed.Verdict)
	}
	if strings.TrimSpace(parsed.Reason) == "" {
		return Result{}, fmt.Errorf("model returned empty reason")
	}
	if parsed.Confidence == nil {
		return Result{}, fmt.Errorf("model omitted required confidence field")
	}

	conf := *parsed.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	return Result{
		Verdict:      v,
		Reason:       strings.TrimSpace(parsed.Reason),
		Confidence:   conf,
		ClassifiedBy: "model:claude",
	}, nil
}

// findLastJSONObject returns the byte range of the last balanced
// "{...}" in s, or (-1, -1) if none. Walks forward tracking brace
// depth + string state; collects every top-level object and returns
// the last one. Forward iteration keeps escape handling correct
// (the `\` precedes the escaped char) and skips brace-depth changes
// inside string literals so a payload containing `"{"` still bounds.
func findLastJSONObject(s string) (int, int) {
	bestStart, bestEnd := -1, -1
	depth := 0
	curStart := -1
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				curStart = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && curStart >= 0 {
				bestStart, bestEnd = curStart, i
				curStart = -1
			}
		}
	}
	return bestStart, bestEnd
}

// truncateForError caps long strings in error messages so a 100KB
// stray output doesn't end up in the log.
func truncateForError(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
