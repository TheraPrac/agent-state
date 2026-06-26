package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/extract"
)

// writeTranscript drops a minimal JSONL session file (the on-disk shape
// internal/transcript parses) and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(joinLines(lines)), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func joinLines(ls []string) string {
	out := ""
	for _, l := range ls {
		out += l + "\n"
	}
	return out
}

const (
	jsonlDecision = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: gate decision-capture per-agent because a peer changelog write is a coordination violation."}]}}`
	jsonlNarration = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me run the tests and read the output."}]}}`
)

// TestComposeExtractedReason_NoStutter: when the verdict text already
// contains the rationale/alts (the extractor's Text is often the whole
// sentence), the why/alts must NOT be appended again — no "X because Y —
// because Y" stutter; when they are genuinely additional, they ARE appended.
func TestComposeExtractedReason_NoStutter(t *testing.T) {
	whole := composeExtractedReason(extract.Candidate{
		Text:      "go agent-scoped because peers collide",
		Rationale: "peers collide",
	})
	if whole != "go agent-scoped because peers collide" {
		t.Errorf("rationale already in text must not be re-appended, got %q", whole)
	}
	sep := composeExtractedReason(extract.Candidate{
		Text:         "go agent-scoped",
		Rationale:    "peers collide",
		RejectedAlts: "global first-active",
	})
	if sep != "go agent-scoped — because peers collide (rejected: global first-active)" {
		t.Errorf("genuinely-separate why/alts must be appended, got %q", sep)
	}
}

// TestExtractDecisions_AppendsUncoveredFork: a clear prose fork with no
// prior decision entry is recovered as a source=extracted entry.
func TestExtractDecisions_AppendsUncoveredFork(t *testing.T) {
	s, cfg := setupTestEnv(t)
	tp := writeTranscript(t, jsonlNarration, jsonlDecision)

	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
		TranscriptPath: tp, ID: "T-001", Trigger: "precompact",
	}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	es, _ := changelog.Read(cfg, "T-001")
	var dec []changelog.Entry
	for _, e := range es {
		if e.EffectiveKind() == changelog.KindDecision {
			dec = append(dec, e)
		}
	}
	if len(dec) != 1 {
		t.Fatalf("want 1 extracted decision, got %d (%+v)", len(dec), dec)
	}
	if dec[0].Source != changelog.SourceExtracted {
		t.Errorf("Source = %q, want extracted", dec[0].Source)
	}
	if dec[0].Confidence <= 0 {
		t.Errorf("extracted entry must carry a confidence, got %v", dec[0].Confidence)
	}
	if dec[0].Field != "precompact" {
		t.Errorf("trigger attribution = %q, want precompact", dec[0].Field)
	}
}

// TestExtractDecisions_NeverClobbersStructured is the design-decision-#4
// invariant: a fork already captured verbatim by Phase B (source=structured)
// must NOT be re-added by the lossy extractor.
func TestExtractDecisions_NeverClobbersStructured(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Phase B already captured this exact fork, verbatim + authoritative.
	if err := recordStructuredDecision(cfg, "T-001", "ask_user_question",
		"gate decision-capture per-agent because a peer changelog write is a coordination violation"); err != nil {
		t.Fatalf("seed structured: %v", err)
	}
	tp := writeTranscript(t, jsonlDecision)

	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
		TranscriptPath: tp, ID: "T-001", Trigger: "precompact",
	}); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	es, _ := changelog.Read(cfg, "T-001")
	structured, extracted := 0, 0
	for _, e := range es {
		if e.EffectiveKind() != changelog.KindDecision {
			continue
		}
		switch e.Source {
		case changelog.SourceStructured:
			structured++
		case changelog.SourceExtracted:
			extracted++
		}
	}
	if structured != 1 {
		t.Errorf("the structured entry must survive untouched, got %d", structured)
	}
	if extracted != 0 {
		t.Errorf("extractor must NOT duplicate a structured-captured fork, got %d extracted", extracted)
	}
}

// TestExtractDecisions_Idempotent: re-running PreCompact on the same/over-
// lapping transcript must not duplicate — reconcile skips prior extracted.
func TestExtractDecisions_Idempotent(t *testing.T) {
	s, cfg := setupTestEnv(t)
	tp := writeTranscript(t, jsonlDecision)
	for i := 0; i < 3; i++ {
		if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
			TranscriptPath: tp, ID: "T-001", Trigger: "precompact",
		}); rc != 0 {
			t.Fatalf("run %d rc = %d, want 0", i, rc)
		}
	}
	es, _ := changelog.Read(cfg, "T-001")
	n := 0
	for _, e := range es {
		if e.EffectiveKind() == changelog.KindDecision {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("3 runs of the same transcript must yield exactly 1 decision, got %d", n)
	}
}

// TestExtractDecisions_MissingTranscriptIsLoudRC1: the to-be-summarized
// window is about to be lost — an unreadable transcript is a genuine
// failure, never a silent rc 0.
func TestExtractDecisions_MissingTranscriptIsLoudRC1(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
		TranscriptPath: filepath.Join(t.TempDir(), "nope.jsonl"), ID: "T-001",
	}); rc != 1 {
		t.Fatalf("missing-transcript rc = %d, want 1 (loud)", rc)
	}
	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{TranscriptPath: "", ID: "T-001"}); rc != 1 {
		t.Fatalf("empty-transcript-path rc = %d, want 1", rc)
	}
}

// TestExtractDecisions_NoCandidatesQuietNoOp: a transcript with only
// narration yields nothing — high precision, clean quiet rc 0.
func TestExtractDecisions_NoCandidatesQuietNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)
	tp := writeTranscript(t, jsonlNarration, jsonlNarration)
	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
		TranscriptPath: tp, ID: "T-001", Trigger: "precompact",
	}); rc != 0 {
		t.Fatalf("no-candidates rc = %d, want 0", rc)
	}
	es, _ := changelog.Read(cfg, "T-001")
	for _, e := range es {
		if e.EffectiveKind() == changelog.KindDecision {
			t.Fatalf("narration-only transcript must write no decision, got %+v", e)
		}
	}
}

// TestExtractDecisions_RefusesPeerItem: the extractor shares the exact
// cross-agent write guard with CaptureDecision (resolveCaptureTarget) — an
// explicit --item naming a peer's in-flight item is refused (rc 1, nothing
// written), so the lossy backstop can never violate the coordination rule.
func TestExtractDecisions_RefusesPeerItem(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-c") // setupTestEnv T-003 is assigned agent-a
	s, cfg := setupTestEnv(t)
	tp := writeTranscript(t, jsonlDecision)
	if rc := ExtractDecisions(s, cfg, ExtractDecisionsOpts{
		TranscriptPath: tp, ID: "T-003", Trigger: "precompact",
	}); rc != 1 {
		t.Fatalf("peer-item rc = %d, want 1 (refuse)", rc)
	}
	es, _ := changelog.Read(cfg, "T-003")
	for _, e := range es {
		if e.EffectiveKind() == changelog.KindDecision {
			t.Fatalf("peer item T-003 received an extracted decision — coordination violation: %+v", e)
		}
	}
}
