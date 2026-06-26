package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/theraprac/agent-state/internal/changelog"
	"github.com/theraprac/agent-state/internal/classify"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// flipForTest puts an item into awaiting_decision with a populated
// card, mirroring what the production halt-on-red path does. The
// setup fixtures leave T-003 as active; promoting to
// awaiting_decision and writing the card here is the natural
// pre-state for every `st decide` test.
func flipForTest(t *testing.T, s *store.Store, cfg *config.Config, id string) classify.DecisionCard {
	t.Helper()
	card := classify.BuildDecisionCard(
		classify.Result{
			Verdict:      classify.VerdictRed,
			Reason:       "touches RBAC handler",
			ClassifiedAt: time.Now().UTC(),
			ClassifiedBy: "model:claude",
			InputHash:    "abc123",
		},
		[]string{"theraprac-api/internal/handlers/billing/rbac.go"},
		"approve to merge",
		time.Now().UTC(),
	)
	if err := FlipToAwaitingDecision(s, cfg, id, card); err != nil {
		t.Fatalf("flip %s: %v", id, err)
	}
	return card
}

func TestDecide_ApproveResumesActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	flipForTest(t, s, cfg, "T-003")

	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideApprove}); rc != 0 {
		t.Fatalf("Decide approve rc = %d; want 0", rc)
	}
	item, _ := s.Get("T-003")
	if item.Status != "active" {
		t.Errorf("status = %q; want active", item.Status)
	}

	entries, _ := changelog.Read(cfg, "T-003")
	if !hasOp(entries, "decide_approve") {
		t.Errorf("changelog missing decide_approve entry; got %v", entries)
	}

	corpus, _ := classify.ReadCorpus(filepath.Join(cfg.Root(), ".as", "classify-corpus.jsonl"), 0)
	if len(corpus) != 1 || corpus[0].OperatorAction != "approved" || corpus[0].ItemID != "T-003" {
		t.Errorf("corpus = %+v; want one approved entry for T-003", corpus)
	}
}

func TestDecide_RejectClosesAsAbandoned(t *testing.T) {
	s, cfg := setupTestEnv(t)
	flipForTest(t, s, cfg, "T-003")

	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideReject}); rc != 2 {
		t.Fatalf("reject without --reason rc = %d; want 2", rc)
	}
	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideReject, Reason: "out of scope"}); rc != 0 {
		t.Fatalf("Decide reject rc = %d; want 0", rc)
	}
	item, _ := s.Get("T-003")
	if item.Status != "abandoned" {
		t.Errorf("status = %q; want abandoned", item.Status)
	}

	corpus, _ := classify.ReadCorpus(filepath.Join(cfg.Root(), ".as", "classify-corpus.jsonl"), 0)
	if len(corpus) != 1 || corpus[0].OperatorAction != "rejected" || corpus[0].OperatorReason != "out of scope" {
		t.Errorf("corpus = %+v; want one rejected entry with reason", corpus)
	}
}

func TestDecide_DeferReturnsToQueueAndClearsClassification(t *testing.T) {
	s, cfg := setupTestEnv(t)
	card := flipForTest(t, s, cfg, "T-003")

	// Write a classification block so we can prove defer clears it.
	if err := persistClassification(s, "T-003", classify.Result{
		Verdict:      card.ClassifierVerdict,
		Reason:       card.ClassifierReason,
		Confidence:   0.9,
		ClassifiedAt: time.Now().UTC(),
		ClassifiedBy: card.ClassifierBy,
		InputHash:    card.ClassifierInputHash,
	}); err != nil {
		t.Fatalf("persistClassification: %v", err)
	}

	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideDefer}); rc != 0 {
		t.Fatalf("Decide defer rc = %d; want 0", rc)
	}
	item, _ := s.Get("T-003")
	if item.Status != "queued" {
		t.Errorf("status = %q; want queued", item.Status)
	}
	if v, _ := item.Doc.GetNestedField("classification.verdict"); v != "" {
		t.Errorf("classification.verdict = %q; want cleared on defer", v)
	}
}

func TestDecide_RefusesNonAwaiting(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-003 is active (setupTestEnv default) — not awaiting_decision.
	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideApprove}); rc != 2 {
		t.Errorf("Decide on non-awaiting rc = %d; want 2", rc)
	}
}

func TestDecide_UnknownActionRejected(t *testing.T) {
	s, cfg := setupTestEnv(t)
	if rc := Decide(s, cfg, "T-001", DecideOpts{Action: "delete"}); rc != 2 {
		t.Errorf("unknown action rc = %d; want 2", rc)
	}
}

func TestDecide_RefusesPeerAgent(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-c")
	s, cfg := setupTestEnv(t)
	flipForTest(t, s, cfg, "T-003")
	// T-003's fixture sets assigned_to: agent-a — agent-c must be
	// refused.
	if rc := Decide(s, cfg, "T-003", DecideOpts{Action: DecideApprove}); rc != 1 {
		t.Errorf("peer-agent decide rc = %d; want 1", rc)
	}
}

func TestDecisionCard_RoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	original := classify.BuildDecisionCard(
		classify.Result{
			Verdict:      classify.VerdictRed,
			Reason:       "RLS handler change with missing tests",
			ClassifiedAt: now.Add(-1 * time.Minute),
			ClassifiedBy: "model:claude",
			InputHash:    "deadbeef",
		},
		[]string{"theraprac-api/internal/access/rls.go", "theraprac-api/cmd/server/main.go"},
		"approve to push",
		now,
	)
	original.UnblockCriteria = "add RLS integration test covering the new policy"

	fields := original.AsNestedFields()
	parsed := classify.ParseDecisionCard(fields)

	if parsed.RiskSummary != original.RiskSummary {
		t.Errorf("risk_summary lost: got %q want %q", parsed.RiskSummary, original.RiskSummary)
	}
	if parsed.Ask != original.Ask || parsed.UnblockCriteria != original.UnblockCriteria {
		t.Errorf("ask/unblock lost: %+v", parsed)
	}
	if parsed.FilesTouchedCount != 2 || len(parsed.FilesTouchedTop) != 2 {
		t.Errorf("files lost: count=%d top=%v", parsed.FilesTouchedCount, parsed.FilesTouchedTop)
	}
	if parsed.ClassifierVerdict != classify.VerdictRed {
		t.Errorf("verdict lost: %q", parsed.ClassifierVerdict)
	}
	if !parsed.ClassifiedAt.Equal(original.ClassifiedAt) {
		t.Errorf("classified_at lost: %v vs %v", parsed.ClassifiedAt, original.ClassifiedAt)
	}
}

// TestUpdate_StatusTransitions_AwaitingDecision verifies the
// preCheckVocab transition rules: queued/done/abandoned/archived
// cannot enter awaiting_decision; awaiting_decision cannot leave to
// anything except active/abandoned/queued.
func TestUpdate_StatusTransitions_AwaitingDecision(t *testing.T) {
	s, cfg := setupTestEnv(t)

	// queued → awaiting_decision must be rejected.
	if rc := Update(s, cfg, "T-001", "status", "awaiting_decision", UpdateModeValue); rc != 2 {
		t.Errorf("queued → awaiting rc = %d; want 2", rc)
	}

	// active → awaiting_decision must succeed (T-003 fixture is active).
	if rc := Update(s, cfg, "T-003", "status", "awaiting_decision", UpdateModeValue); rc != 0 {
		t.Errorf("active → awaiting rc = %d; want 0", rc)
	}

	// awaiting → done is not a supported manual exit.
	if rc := Update(s, cfg, "T-003", "status", "done", UpdateModeValue); rc != 2 {
		t.Errorf("awaiting → done rc = %d; want 2", rc)
	}

	// awaiting → active is allowed (mirrors approve).
	if rc := Update(s, cfg, "T-003", "status", "active", UpdateModeValue); rc != 0 {
		t.Errorf("awaiting → active rc = %d; want 0", rc)
	}
}

// T-461: approval gate removed — all QueueAdd entries are auto-approved.
// Binary autonomy (T-346) was gated on IsQueuePending; since that is now
// always false, start succeeds for any agent-added item without a pending
// bypass. The start_auto_approved changelog path is unreachable.
func TestStart_AutoApprovesGreenItem(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	t.Setenv("ST_BINARY_AUTONOMY", "1")
	s, cfg := setupTestEnv(t)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd: %d", rc)
	}
	// Start must succeed: no pending gate to bypass.
	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 0 {
		t.Errorf("Start green-classified rc = %d; want 0", rc)
	}
}

// T-461: without a pending gate, flag-off has no effect — start succeeds
// for any agent-added item regardless of ST_BINARY_AUTONOMY.
func TestStart_AutoApproveDisabledWhenFlagOff(t *testing.T) {
	t.Setenv("AS_AGENT_ID", "agent-a")
	os.Unsetenv("ST_BINARY_AUTONOMY")
	s, cfg := setupTestEnv(t)

	if rc := QueueAdd(s, cfg, "T-001", QueueOpts{}); rc != 0 {
		t.Fatalf("QueueAdd: %d", rc)
	}
	// No pending gate → start always succeeds (exit 0).
	if rc := Start(s, cfg, "T-001", StartOpts{}); rc != 0 {
		t.Errorf("Start (flag off, no gate) rc = %d; want 0", rc)
	}
}

// hasOp scans a changelog entry list for an Op match.
func hasOp(entries []changelog.Entry, op string) bool {
	for _, e := range entries {
		if e.Op == op {
			return true
		}
	}
	return false
}
