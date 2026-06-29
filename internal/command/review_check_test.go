package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
)

// gitInTest runs git in dir and fails the test on error.
func gitInTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// seedRepoLevelWithMain creates a git repo whose HEAD equals origin/main
// (no commits ahead) — reviewedRepoSHA reports hasDiff=false for it.
func seedRepoLevelWithMain(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "init", "-q")
	gitInTest(t, dir, "config", "user.email", "t@t.test")
	gitInTest(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "add", "-A")
	gitInTest(t, dir, "commit", "-q", "-m", "base")
	// Point origin/main at HEAD — no real remote needed.
	gitInTest(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
}

// seedRepoAheadOfMain creates a git repo with one commit ahead of origin/main —
// reviewedRepoSHA reports hasDiff=true and the SHA of that ahead commit.
func seedRepoAheadOfMain(t *testing.T, dir string) string {
	t.Helper()
	seedRepoLevelWithMain(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\nchange\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "add", "-A")
	gitInTest(t, dir, "commit", "-q", "-m", "item change")
	sha := gitInTest(t, dir, "rev-parse", "HEAD")
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	return short
}

// I-1651 regression: resolveCurrentSHA must return the HEAD of the repo that has
// an item diff, not the first existing worktree repo. Reproduces the I-1641 case
// where the first repo (repo-a) is untouched at origin/main and a later repo
// (repo-b) holds the item's commit.
func TestResolveCurrentSHAPicksRepoWithDiff(t *testing.T) {
	_, cfg := setupTestEnv(t)
	cfg.Worktree = &config.WorktreeConfig{Enabled: true, BaseDir: "worktrees", Repos: []string{"repo-a", "repo-b"}}

	// Pattern 1 layout: <worktree-base>/<item-id>/<repo>.
	base := filepath.Join(cfg.WorktreeBase(), "T-003")
	seedRepoLevelWithMain(t, filepath.Join(base, "repo-a")) // first repo: no diff
	wantSHA := seedRepoAheadOfMain(t, filepath.Join(base, "repo-b"))

	got := resolveCurrentSHA(cfg, "T-003", ReviewCheckOpts{})
	if got != wantSHA {
		t.Errorf("resolveCurrentSHA = %q, want %q (repo-b, the repo with the diff)", got, wantSHA)
	}
}

func seedDocField(t *testing.T, s interface {
	Mutate(string, func(*model.Item) error) error
}, id, field, value string) {
	t.Helper()
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField(field, value)
		return nil
	}); err != nil {
		t.Fatalf("seeding %s on %s: %v", field, id, err)
	}
}

func seedReviewEvidence(t *testing.T, s interface {
	Mutate(string, func(*model.Item) error) error
}, id, ev string) {
	t.Helper()
	seedDocField(t, s, id, "review_evidence", ev)
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

func seedReviewSkips(t *testing.T, s interface {
	Mutate(string, func(*model.Item) error) error
}, id, skips string) {
	t.Helper()
	seedDocField(t, s, id, "review_skips", skips)
}

func TestReviewCheckSkipsApplied(t *testing.T) {
	// fail verdict + non-empty review_skips + SHA match → returns 0.
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "fail abc1234 2026-06-14T10:00:00-06:00 evidence:")
	seedReviewSkips(t, s, "T-003", "- finding: mockReconcileMutate not wrapped with vi.hoisted()\n  reason: false positive in workspace-only item; file not in this PR\n  operator: jfinlinson")

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "abc1234", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 0 {
		t.Errorf("ReviewCheck with review_skips: got %d, want 0", code)
	}
}

func TestReviewCheckSkipsDoNotBypassSHAMismatch(t *testing.T) {
	// fail verdict + review_skips present but SHA mismatch → returns 1.
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "fail stale00 2026-06-14T10:00:00-06:00 evidence:")
	seedReviewSkips(t, s, "T-003", "- finding: some violation\n  reason: pre-approved\n  operator: jfinlinson")

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "newsha1", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("ReviewCheck review_skips with SHA mismatch: got %d, want 1", code)
	}
}

func TestReviewCheckEmptySkipsDoesNotBypass(t *testing.T) {
	// fail verdict + review_skips exists but empty value → gate still fails.
	s, cfg := setupTestEnv(t)
	seedReviewEvidence(t, s, "T-003", "fail abc1234 2026-06-14T10:00:00-06:00 evidence:")
	seedReviewSkips(t, s, "T-003", "") // explicit empty — must not bypass

	opts := ReviewCheckOpts{
		GitHeadSHA: func(dir string) (string, error) { return "abc1234", nil },
	}
	code := ReviewCheck(s, cfg, "T-003", opts)
	if code != 1 {
		t.Errorf("ReviewCheck empty review_skips: got %d, want 1 (empty field must not bypass gate)", code)
	}
}
