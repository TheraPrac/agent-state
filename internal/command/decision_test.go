package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/plan"
)

// TestApproachGist: the plan_approve decision must carry the verdict (the
// chosen approach), not a bare pointer — first non-empty line, collapsed,
// capped; empty in ⇒ empty out (caller falls back to the pointer).
func TestApproachGist(t *testing.T) {
	if got := approachGist(""); got != "" {
		t.Errorf("empty ⇒ empty, got %q", got)
	}
	if got := approachGist("\n\n  \n"); got != "" {
		t.Errorf("whitespace-only ⇒ empty, got %q", got)
	}
	if got := approachGist("\n\n  Wire it   into\tst plan approve  \nsecond line"); got != "Wire it into st plan approve" {
		t.Errorf("first non-empty line collapsed; got %q", got)
	}
	long := approachGist(strings.Repeat("x", 300))
	if len(long) != 160 || !strings.HasSuffix(long, "...") {
		t.Errorf("cap at 160 w/ ellipsis, got len %d", len(long))
	}
}

// TestPlanParseExtractsApproach pins the production dependency: the
// plan_approve enrichment is only non-inert if plan.Parse populates
// Approach from a realistic plan body. If this regresses, the decision
// silently degrades to the bare-pointer fallback.
func TestPlanParseExtractsApproach(t *testing.T) {
	body := "# I-999 — x\n\n## Approach\n\nWire recordStructuredDecision into st plan approve.\n\n## Acceptance Criteria\n- cmd: true\n"
	p, err := plan.Parse(body)
	if err != nil || p == nil {
		t.Fatalf("Parse: p=%v err=%v", p, err)
	}
	if p.Approach == "" {
		t.Fatalf("plan.Parse did not extract ## Approach — plan_approve enrichment would be inert")
	}
	if got := approachGist(p.Approach); got != "Wire recordStructuredDecision into st plan approve." {
		t.Errorf("end-to-end gist wrong: %q", got)
	}
}

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

// TestRecordStructuredDecision_WriteFailureIsNotSilent: the Phase A
// self-attestation audit only covers the exec/commit tape vs git — it does
// NOT cover decision entries. So a dropped decision write has no downstream
// backstop; the only thing that makes it non-silent is reporting the
// failure here. A failed Append must emit an stderr warning, never vanish.
func TestRecordStructuredDecision_WriteFailureIsNotSilent(t *testing.T) {
	cfg := decisionTestCfg(t)
	// Force changelog.Append to fail deterministically + cross-platform:
	// put a regular FILE where the .changelog directory must be created,
	// so os.MkdirAll inside Append returns "not a directory".
	clDir := cfg.ChangelogDir()
	_ = os.RemoveAll(clDir)
	if err := os.WriteFile(clDir, []byte("blocker"), 0644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}

	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	recordStructuredDecision(cfg, "I-679", "plan_approve", "a real rationale that must not vanish")
	_ = w.Close()
	os.Stderr = origStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	if !strings.Contains(got, "warning") || !strings.Contains(got, "I-679") || !strings.Contains(got, "plan_approve") {
		t.Errorf("write failure must emit a clear stderr warning naming the item+trigger; got: %q", got)
	}
	if !strings.Contains(got, "st resume") {
		t.Errorf("warning should tell the operator the rationale won't appear in st resume; got: %q", got)
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
