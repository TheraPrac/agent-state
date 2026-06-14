package command

import (
	"testing"

	"github.com/jfinlinson/agent-state/internal/model"
)

func seedReviewEvidence(t *testing.T, s interface {
	Mutate(string, func(*model.Item) error) error
}, id, ev string) {
	t.Helper()
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("review_evidence", ev)
		return nil
	}); err != nil {
		t.Fatalf("seeding review_evidence on %s: %v", id, err)
	}
}

func TestReviewCheck(t *testing.T) {
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "pass abc1234 2026-06-14T10:00:00-06:00 evidence:mock://T-003/review/abc1234/20260614T100000/report.json.gz")

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "abc1234", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("ReviewCheck pass: got %d, want 0", code)
	}
}

func TestReviewCheckFail(t *testing.T) {
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "fail abc1234 2026-06-14T10:00:00-06:00 evidence:")

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "abc1234", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("ReviewCheck fail verdict: got %d, want 1", code)
	}
}

func TestReviewCheckSHAMismatch(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// Review was run on stale00 but HEAD is now newsha1.
	seedReviewEvidence(t, s, "T-003", "pass stale00 2026-06-14T10:00:00-06:00 evidence:")

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "newsha1", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("ReviewCheck SHA mismatch: got %d, want 1", code)
	}
}

func TestReviewCheckMissingEvidence(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-003 has no review_evidence field.
	code := ReviewCheck(s, cfg, "T-003", ReviewCheckOpts{})
	if code != 1 {
		t.Errorf("ReviewCheck no evidence: got %d, want 1", code)
	}
}

func TestReviewCheckNotFound(t *testing.T) {
	s, cfg := setupTestEnv(t)
	code := ReviewCheck(s, cfg, "T-999", ReviewCheckOpts{})
	if code != 1 {
		t.Errorf("ReviewCheck not found: got %d, want 1", code)
	}
}

func TestReviewCheckSHAResolveFailure(t *testing.T) {
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "pass abc1234 2026-06-14T10:00:00-06:00 evidence:")

	// SHA resolver returns error → SHA is "" → skip SHA check → passes on verdict alone.
	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "", nil },
	}
	// When resolveCurrentSHA returns "" (not an error but empty), SHA check is skipped.
	// The item has no worktree dir in the test env, so resolveCurrentSHA returns "".
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("ReviewCheck with unresolvable SHA (empty): got %d, want 0 (skip SHA check)", code)
	}
}

// Verify that review_evidence field parsing handles missing parts gracefully.
func TestReviewCheckMalformedEvidence(t *testing.T) {
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "justoneword")

	code := ReviewCheck(s, cfg, "T-003", ReviewCheckOpts{})
	if code != 1 {
		t.Errorf("ReviewCheck malformed evidence: got %d, want 1", code)
	}
}

func TestReviewCheckNonActive(t *testing.T) {
	s, cfg := setupTestEnv(t)
	// T-001 is queued — not active.
	code := ReviewCheck(s, cfg, "T-001", ReviewCheckOpts{})
	if code != 1 {
		t.Errorf("ReviewCheck non-active item: got %d, want 1", code)
	}
}
