package command

import (
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// stubbed git/gh injectables for a happy-path PRCreate (slug, branch, manifest).
func prCreateHappyOpts(ghCalls *[]string) PRCreateOpts {
	return PRCreateOpts{
		Repo:  "api",
		Title: "fix(I-003): a thing",
		Body:  "body text",
		RunGh: func(args []string) (string, error) {
			*ghCalls = append(*ghCalls, strings.Join(args, " "))
			return "https://github.com/TheraPrac/theraprac-api/pull/42\n", nil
		},
		GitRemoteURL: func(dir string) (string, error) { return "git@github.com:TheraPrac/theraprac-api.git", nil },
		GitBranch:    func(dir string) (string, error) { return "fix/I-003-a-thing\n", nil },
		GitHeadSHA:   func(dir string) (string, error) { return "abc1234", nil },
		// Manifest analysis (forwarded to PR()).
		GitNameStatus: func(dir string) (string, error) { return "A\tinternal/foo.go\n", nil },
		GitNumstat:    func(dir string) (string, error) { return "5\t0\tinternal/foo.go\n", nil },
		GitBlobHash:   func(dir, path string) (string, error) { return "deadbeef", nil },
		FileExists:    func(path string) bool { return true },
	}
}

func seedLiveAcceptance(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.Mutate(id, func(it *model.Item) error {
		it.SetNested("testing_evidence", "live_acceptance", "pass abc1234 2026-06-28T10:00:00-06:00")
		return nil
	}); err != nil {
		t.Fatalf("seed live_acceptance: %v", err)
	}
}

// 1. Non-draft with no live_acceptance → refuse, gh never called.
func TestPRCreateNonDraftMissingLiveAcceptance(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	// review_evidence present + matching, but live_acceptance absent.
	seedReviewEvidence(t, s, "T-003", "pass abc1234 2026-06-28T10:00:00-06:00 evidence:mock://x")

	var ghCalls []string
	opts := prCreateHappyOpts(&ghCalls)
	code := PRCreate(s, cfg, "T-003", opts)
	if code == 0 {
		t.Errorf("expected non-zero (missing live_acceptance), got 0")
	}
	if len(ghCalls) != 0 {
		t.Errorf("gh must NOT be called when a gate fails; got %v", ghCalls)
	}
}

// 2. Non-draft with live_acceptance but no review_evidence → refuse, gh never called.
func TestPRCreateNonDraftMissingReviewEvidence(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	seedLiveAcceptance(t, s, "T-003")
	// no review_evidence seeded.

	var ghCalls []string
	opts := prCreateHappyOpts(&ghCalls)
	code := PRCreate(s, cfg, "T-003", opts)
	if code == 0 {
		t.Errorf("expected non-zero (missing review_evidence), got 0")
	}
	if len(ghCalls) != 0 {
		t.Errorf("gh must NOT be called when review gate fails; got %v", ghCalls)
	}
}

// 3. Non-draft, review SHA mismatch → refuse, gh never called.
func TestPRCreateNonDraftReviewSHAMismatch(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	seedLiveAcceptance(t, s, "T-003")
	seedReviewEvidence(t, s, "T-003", "pass stale00 2026-06-28T10:00:00-06:00 evidence:mock://x")

	var ghCalls []string
	opts := prCreateHappyOpts(&ghCalls) // GitHeadSHA returns abc1234 ≠ stale00
	code := PRCreate(s, cfg, "T-003", opts)
	if code == 0 {
		t.Errorf("expected non-zero (review SHA mismatch), got 0")
	}
	if len(ghCalls) != 0 {
		t.Errorf("gh must NOT be called on SHA mismatch; got %v", ghCalls)
	}
}

// 4. Non-draft, both gates satisfied → creates PR, records manifest, stage pr_open.
func TestPRCreateNonDraftSuccess(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	seedLiveAcceptance(t, s, "T-003")
	seedReviewEvidence(t, s, "T-003", "pass abc1234 2026-06-28T10:00:00-06:00 evidence:mock://x")

	var ghCalls []string
	opts := prCreateHappyOpts(&ghCalls)
	code := PRCreate(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("expected 0 (all gates pass), got %d", code)
	}
	if len(ghCalls) != 1 {
		t.Fatalf("expected exactly one gh call, got %v", ghCalls)
	}
	got := ghCalls[0]
	for _, want := range []string{
		"pr create", "-R TheraPrac/theraprac-api", "--base main",
		"--head fix/I-003-a-thing", "--title fix(I-003): a thing", "--body body text",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gh args missing %q; got: %s", want, got)
		}
	}
	if strings.Contains(got, "--draft") {
		t.Errorf("non-draft must not pass --draft; got: %s", got)
	}
	// Manifest recorded + stage advanced.
	item, _ := s.Get("T-003")
	if stage, _ := getNestedField(item, "delivery", "stage"); stage != "pr_open" {
		t.Errorf("stage = %q, want pr_open", stage)
	}
	if prs, _ := getNestedField(item, "manifest", "prs"); !strings.Contains(prs, "42") {
		t.Errorf("manifest.prs = %q, want it to mention PR 42", prs)
	}
}

// 5. Draft skips BOTH gates (no evidence at all) and passes --draft to gh.
func TestPRCreateDraftSkipsGates(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	// No live_acceptance, no review_evidence.

	var ghCalls []string
	opts := prCreateHappyOpts(&ghCalls)
	opts.Draft = true
	code := PRCreate(s, cfg, "T-003", opts)
	if code != 0 {
		t.Fatalf("draft should succeed without gates, got %d", code)
	}
	if len(ghCalls) != 1 || !strings.Contains(ghCalls[0], "--draft") {
		t.Errorf("expected one gh call with --draft, got %v", ghCalls)
	}
}

// 6. Arg validation: repo, title, and a body source are required.
func TestPRCreateArgValidation(t *testing.T) {
	s, cfg := setupPRTestEnvWithManifest(t)
	seedLiveAcceptance(t, s, "T-003")
	seedReviewEvidence(t, s, "T-003", "pass abc1234 2026-06-28T10:00:00-06:00 evidence:mock://x")

	cases := []struct {
		name string
		mut  func(*PRCreateOpts)
	}{
		{"no repo", func(o *PRCreateOpts) { o.Repo = "" }},
		{"no title", func(o *PRCreateOpts) { o.Title = "" }},
		{"no body", func(o *PRCreateOpts) { o.Body = ""; o.BodyFile = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ghCalls []string
			opts := prCreateHappyOpts(&ghCalls)
			tc.mut(&opts)
			if code := PRCreate(s, cfg, "T-003", opts); code == 0 {
				t.Errorf("%s: expected non-zero, got 0", tc.name)
			}
			if len(ghCalls) != 0 {
				t.Errorf("%s: gh must not be called on bad args", tc.name)
			}
		})
	}
}

// 7. PR-number parse from gh's create output (URL on its own / last line).
func TestParsePRNumberFromCreateOutput(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"https://github.com/TheraPrac/theraprac-api/pull/42\n", 42},
		{"Warning: 3 uncommitted changes\nhttps://github.com/TheraPrac/theraprac-web/pull/1007\n", 1007},
		{"https://github.com/o/r/pull/5", 5},
		{"no url here", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := parsePRNumberFromCreateOutput(tc.in); got != tc.want {
			t.Errorf("parse(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
