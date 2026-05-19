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

// TestCaptureDecision_ExplicitItem: the hook-invoked entry point writes the
// SAME native-structured record as recordStructuredDecision (one tested
// codepath), attributed to an explicit --item, and defaults a blank trigger.
func TestCaptureDecision_ExplicitItem(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{
		ID:      "T-001",
		Trigger: "ask_user_question",
		Reason:  "Q: cache layer? → A: Redis (rejected: in-process LRU — multi-pod)",
	}); rc != 0 {
		t.Fatalf("explicit-item capture rc = %d, want 0", rc)
	}
	// Blank trigger must not drop the decision — it falls back to a
	// stable label so the entry stays attributable.
	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{
		ID:     "T-001",
		Reason: "second fork captured with no explicit trigger",
	}); rc != 0 {
		t.Fatalf("blank-trigger capture rc = %d, want 0", rc)
	}

	entries, err := changelog.Read(cfg, "T-001")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.EffectiveKind() != changelog.KindDecision {
			t.Errorf("Kind = %q, want decision", e.EffectiveKind())
		}
		if e.Source != changelog.SourceStructured {
			t.Errorf("Source = %q, want structured", e.Source)
		}
	}
	if entries[0].Field != "ask_user_question" {
		t.Errorf("trigger = %q, want ask_user_question", entries[0].Field)
	}
	if entries[1].Field != "hook_decision" {
		t.Errorf("blank trigger should default to hook_decision, got %q", entries[1].Field)
	}
}

// TestCaptureDecision_EmptyReasonIsCleanNoOp: an unparseable AskUserQuestion
// answer / empty plan is genuinely nothing to record. It must exit 0 (so the
// hook stays quiet) and write nothing — never cry wolf on a no-op.
func TestCaptureDecision_EmptyReasonIsCleanNoOp(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{ID: "T-001", Trigger: "exit_plan_mode", Reason: "   \n\t "}); rc != 0 {
		t.Fatalf("empty-reason rc = %d, want 0 (clean no-op)", rc)
	}
	entries, _ := changelog.Read(cfg, "T-001")
	if len(entries) != 0 {
		t.Fatalf("empty reason recorded %d entries, want 0", len(entries))
	}
}

// TestCaptureDecision_UnknownItemIsLoudFailure: an explicit --item that does
// not exist must NOT silently drop the fork. Returns 1 so the hook emits its
// loud "decision NOT captured" line (operator silent-failure principle).
func TestCaptureDecision_UnknownItemIsLoudFailure(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{ID: "Z-999", Trigger: "ask_user_question", Reason: "real fork that must not vanish"}); rc != 1 {
		t.Fatalf("unknown-item rc = %d, want 1", rc)
	}
	if entries, _ := changelog.Read(cfg, "Z-999"); len(entries) != 0 {
		t.Fatalf("unknown item wrote %d entries, want 0", len(entries))
	}
}

// TestCaptureDecision_ResolvesActiveItem: with no --item the capture must
// land on whatever item the session is working — mirroring `st resume`'s
// stack-top → first-active precedence so next session replays it from the
// same item. setupTestEnv's only active item is T-003.
func TestCaptureDecision_ResolvesActiveItem(t *testing.T) {
	s, cfg := setupTestEnv(t)

	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{Trigger: "exit_plan_mode", Reason: "approach: build the thing the documented way"}); rc != 0 {
		t.Fatalf("active-resolution rc = %d, want 0", rc)
	}
	if entries, _ := changelog.Read(cfg, "T-003"); len(entries) != 1 {
		t.Fatalf("expected the fork on the active item T-003, got %d entries", len(entries))
	}
}

// TestCaptureDecision_NeverResolvesOntoPeerItem is the regression test for
// the defect caught by live verification on 2026-05-19: with no --item and
// an un-scoped "first active" resolver, the PostToolUse capture appended a
// decision onto a PEER's changelog (I-542, agent-a) in the shared
// multi-agent workspace — violating the coordination rule "never edit a
// peer's item". setupTestEnv's only active item (T-003) is assigned
// agent-a; running AS agent-c, the capture must refuse (rc 1, loud, nothing
// written) rather than silently land on the peer's item. As agent-a it
// resolves normally — the guard scopes, it does not break resolution.
func TestCaptureDecision_NeverResolvesOntoPeerItem(t *testing.T) {
	t.Run("foreign agent refuses, writes nothing", func(t *testing.T) {
		t.Setenv("AS_AGENT_ID", "agent-c") // T-003 is assigned agent-a
		s, cfg := setupTestEnv(t)
		if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{
			Trigger: "ask_user_question",
			Reason:  "a fork that must NOT be written onto a peer's item",
		}); rc != 1 {
			t.Fatalf("foreign-agent capture rc = %d, want 1 (refuse, never touch peer item)", rc)
		}
		if entries, _ := changelog.Read(cfg, "T-003"); len(entries) != 0 {
			t.Fatalf("peer item T-003 received %d entries — coordination violation", len(entries))
		}
	})

	t.Run("owning agent resolves normally", func(t *testing.T) {
		t.Setenv("AS_AGENT_ID", "agent-a") // matches T-003's assignee
		s, cfg := setupTestEnv(t)
		if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{
			Trigger: "ask_user_question",
			Reason:  "owning agent's fork lands on its own active item",
		}); rc != 0 {
			t.Fatalf("owning-agent capture rc = %d, want 0", rc)
		}
		if entries, _ := changelog.Read(cfg, "T-003"); len(entries) != 1 {
			t.Fatalf("owning agent's fork should land on T-003, got %d entries", len(entries))
		}
	})
}

// TestCaptureDecision_StackTopBeatsActive: a pushed item is the live focus,
// so an un-attributed capture must prefer the stack top over the first
// active item — exactly resolveResumeTarget's precedence.
func TestCaptureDecision_StackTopBeatsActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if err := SaveStack(cfg, []StackEntry{{ID: "T-001"}}); err != nil {
		t.Fatalf("seed stack: %v", err)
	}

	if rc := CaptureDecision(s, cfg, CaptureDecisionOpts{Trigger: "ask_user_question", Reason: "fork while interrupted on T-001"}); rc != 0 {
		t.Fatalf("stack-top-resolution rc = %d, want 0", rc)
	}
	if entries, _ := changelog.Read(cfg, "T-001"); len(entries) != 1 {
		t.Fatalf("expected the fork on stack-top T-001, got %d entries", len(entries))
	}
	if entries, _ := changelog.Read(cfg, "T-003"); len(entries) != 0 {
		t.Fatalf("first-active T-003 must NOT receive the fork when a stack top exists, got %d", len(entries))
	}
}
