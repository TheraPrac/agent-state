package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
)

func decisionTestCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"tasks", "issues", "archive", ".as", ".changelog"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestRecordStructuredDecision: a structured decision is captured verbatim
// as Kind=decision/Source=structured, attributed by trigger, and is the
// authoritative provenance the Phase C extractor must never clobber.
func TestRecordStructuredDecision(t *testing.T) {
	cfg := decisionTestCfg(t)
	reason := "parallel over sequence — Phase 1 substrate already merged, not blocked on agent-b"

	recordStructuredDecision(cfg, "I-679", "plan_approve", reason)
	recordStructuredDecision(cfg, "I-679", "stack_push", "interrupt I-679 for I-687 safety fix")

	entries, err := changelog.Read(cfg, "I-679")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.EffectiveKind() != changelog.KindDecision {
			t.Errorf("entry Kind = %q, want decision", e.EffectiveKind())
		}
		if e.Source != changelog.SourceStructured {
			t.Errorf("entry Source = %q, want structured (authoritative)", e.Source)
		}
		if e.Op != "decision" {
			t.Errorf("entry Op = %q, want decision", e.Op)
		}
	}
	if entries[0].Field != "plan_approve" || entries[0].Reason != reason {
		t.Errorf("first decision not captured verbatim/attributed: field=%q reason=%q", entries[0].Field, entries[0].Reason)
	}
	if entries[1].Field != "stack_push" {
		t.Errorf("second decision trigger = %q, want stack_push", entries[1].Field)
	}
}

// TestRecordStructuredDecision_EmptyReasonRecordsNothing: a decision with
// no rationale is not a non-re-derivable fact — a bare verdict is the
// useless half. Empty/whitespace reason must append nothing.
func TestRecordStructuredDecision_EmptyReasonRecordsNothing(t *testing.T) {
	cfg := decisionTestCfg(t)

	recordStructuredDecision(cfg, "T-1", "plan_approve", "")
	recordStructuredDecision(cfg, "T-1", "stack_push", "   \n\t ")

	entries, err := changelog.Read(cfg, "T-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0 (no rationale ⇒ nothing recorded)", len(entries))
	}
}
