package classify

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubModel returns a fixed Result for tests. Tracks call count so
// tests can verify cache behavior (model not called on a cache hit).
type stubModel struct {
	calls  int
	result Result
	err    error
}

func (m *stubModel) Classify(in Inputs) (Result, error) {
	m.calls++
	if m.err != nil {
		return Result{}, m.err
	}
	return m.result, nil
}

func fixedNow() time.Time {
	return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
}

// TestDenyList_PathPrefixMatch covers each PathPrefix entry in the
// production deny-list — every prefix should hard-red.
func TestDenyList_PathPrefixMatch(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string // expected matching label, empty = no match expected
	}{
		{"terraform state", "theraprac-infra/state/dev.tfstate", "theraprac-infra/state/"},
		{"auth handler", "theraprac-api/internal/auth/middleware.go", "theraprac-api/internal/auth/"},
		{"access handler", "theraprac-api/internal/access/rbac.go", "theraprac-api/internal/access/"},
		{"unrelated api file", "theraprac-api/internal/billing/stripe.go", ""},
		{"web file", "theraprac-web/src/app/page.tsx", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Match([]string{tc.file}, HardRedPatterns)
			if tc.want == "" {
				if got != nil {
					t.Errorf("Match(%q) = %s; want no match", tc.file, got.Label())
				}
				return
			}
			if got == nil {
				t.Fatalf("Match(%q) = nil; want %s", tc.file, tc.want)
			}
			if got.Label() != tc.want {
				t.Errorf("Match(%q) label = %s; want %s", tc.file, got.Label(), tc.want)
			}
		})
	}
}

// TestDenyList_BasenameGlobMatch covers BasenameGlob patterns: IAM
// terraform, secrets terraform, secrets-manifest, *.pem, *.key.
func TestDenyList_BasenameGlobMatch(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"theraprac-infra/aws/iam_admin.tf", "iam_*.tf"},
		{"theraprac-infra/modules/secrets_stripe.tf", "secrets_*.tf"},
		{"theraprac-workspace/secrets-manifest.yaml", "secrets-manifest.yaml"},
		{"theraprac-infra/keys/prod.pem", "*.pem"},
		{"theraprac-infra/keys/prod.key", "*.key"},
		// Glob matches basename, not arbitrary substring — these should NOT match.
		{"theraprac-infra/modules/iam_admin/main.tf", ""}, // basename is main.tf, not iam_*.tf
		{"theraprac-api/cmd/stripe-secrets-rotator.go", ""},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got := Match([]string{tc.file}, HardRedPatterns)
			if tc.want == "" {
				if got != nil {
					t.Errorf("Match(%q) = %s; want no match", tc.file, got.Label())
				}
				return
			}
			if got == nil {
				t.Fatalf("Match(%q) = nil; want %s", tc.file, tc.want)
			}
			if got.Label() != tc.want {
				t.Errorf("Match(%q) = %s; want %s", tc.file, got.Label(), tc.want)
			}
		})
	}
}

// TestDenyList_MultipleFilesFirstMatchWins verifies that Match returns
// the first matching pattern when several files trip the list.
func TestDenyList_MultipleFilesFirstMatchWins(t *testing.T) {
	files := []string{
		"theraprac-api/internal/billing/stripe.go",  // no match
		"theraprac-api/internal/auth/middleware.go", // matches "theraprac-api/internal/auth/"
		"theraprac-infra/state/dev.tfstate",         // would match too, but auth/ comes first in files
	}
	got := Match(files, HardRedPatterns)
	if got == nil {
		t.Fatal("Match = nil; want a match")
	}
	if got.Label() != "theraprac-api/internal/auth/" {
		t.Errorf("Match label = %s; want theraprac-api/internal/auth/", got.Label())
	}
}

// TestClassifier_DenyListShortCircuit covers AC 2: hard-red paths
// force red without calling the model. The stub model's call count
// must stay at zero.
func TestClassifier_DenyListShortCircuit(t *testing.T) {
	model := &stubModel{result: Result{Verdict: VerdictGreen, Reason: "stub green"}}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	in := Inputs{
		ItemID:       "T-345",
		Title:        "test",
		TouchedFiles: []string{"theraprac-infra/state/dev.tfstate"},
	}
	res, err := c.Classify(in, Result{}, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Errorf("verdict = %s; want red", res.Verdict)
	}
	if !strings.Contains(res.Reason, "deny-list match") {
		t.Errorf("reason = %q; want substring 'deny-list match'", res.Reason)
	}
	if res.ClassifiedBy != "deny-list" {
		t.Errorf("classified_by = %s; want deny-list", res.ClassifiedBy)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times; want 0 (deny-list should short-circuit)", model.calls)
	}
}

// TestClassifier_CacheHitSkipsModel covers AC 3: a repeat call with
// the same Inputs (same hash) returns the cached Result and skips
// the model.
func TestClassifier_CacheHitSkipsModel(t *testing.T) {
	model := &stubModel{result: Result{Verdict: VerdictGreen, Reason: "fresh"}}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	in := Inputs{
		ItemID:       "T-345",
		Title:        "test",
		TouchedFiles: []string{"theraprac-api/internal/billing/stripe.go"},
	}
	cached := Result{
		Verdict:      VerdictGreen,
		Reason:       "cached",
		ClassifiedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ClassifiedBy: "model",
		InputHash:    in.Hash(),
	}

	res, err := c.Classify(in, cached, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Reason != "cached" {
		t.Errorf("reason = %q; want cached", res.Reason)
	}
	if !res.ClassifiedAt.Equal(cached.ClassifiedAt) {
		t.Errorf("ClassifiedAt changed; want cache to be returned verbatim")
	}
	if model.calls != 0 {
		t.Errorf("model called %d times; want 0 (cache should short-circuit)", model.calls)
	}
}

// TestClassifier_ForceBypassesCache covers AC 3 part 2: --force
// causes the model to be re-consulted even when the cached hash
// matches.
func TestClassifier_ForceBypassesCache(t *testing.T) {
	model := &stubModel{result: Result{Verdict: VerdictGreen, Reason: "fresh"}}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	in := Inputs{ItemID: "T-345", Title: "test", TouchedFiles: []string{"foo.go"}}
	cached := Result{
		Verdict:      VerdictRed,
		Reason:       "stale",
		InputHash:    in.Hash(),
		ClassifiedBy: "model",
	}

	res, err := c.Classify(in, cached, true)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Reason != "fresh" {
		t.Errorf("reason = %q; want fresh (force should bypass cache)", res.Reason)
	}
	if model.calls != 1 {
		t.Errorf("model called %d times; want 1 (force should call model)", model.calls)
	}
}

// TestClassifier_CacheInvalidatesOnInputChange covers AC 3 part 3:
// changing the inputs busts the cache (different hash → model called).
func TestClassifier_CacheInvalidatesOnInputChange(t *testing.T) {
	model := &stubModel{result: Result{Verdict: VerdictGreen, Reason: "fresh"}}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	oldIn := Inputs{ItemID: "T-345", Title: "old", TouchedFiles: []string{"foo.go"}}
	newIn := Inputs{ItemID: "T-345", Title: "new", TouchedFiles: []string{"foo.go"}}

	cached := Result{Verdict: VerdictRed, Reason: "stale", InputHash: oldIn.Hash(), ClassifiedBy: "model"}
	res, err := c.Classify(newIn, cached, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Reason != "fresh" {
		t.Errorf("reason = %q; want fresh (input change should bust cache)", res.Reason)
	}
	if model.calls != 1 {
		t.Errorf("model called %d times; want 1", model.calls)
	}
}

// TestClassifier_NilModelReturnsErrModelNotWired covers the phase-1
// ship state: without a Model, any non-deny-list classification
// returns ErrModelNotWired. The deny-list path still works (verified
// above).
func TestClassifier_NilModelReturnsErrModelNotWired(t *testing.T) {
	c := Classifier{DenyList: HardRedPatterns, Model: nil, Now: fixedNow}

	in := Inputs{ItemID: "T-345", TouchedFiles: []string{"safe-file.go"}}
	_, err := c.Classify(in, Result{}, false)
	if !errors.Is(err, ErrModelNotWired) {
		t.Errorf("err = %v; want ErrModelNotWired", err)
	}
}

// TestClassifier_ModelErrorPropagates verifies that a model failure
// surfaces to the caller wrapped (not swallowed).
func TestClassifier_ModelErrorPropagates(t *testing.T) {
	model := &stubModel{err: fmt.Errorf("model boom")}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	in := Inputs{ItemID: "T-345", TouchedFiles: []string{"safe.go"}}
	_, err := c.Classify(in, Result{}, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "model boom") {
		t.Errorf("err = %v; want wrapped 'model boom'", err)
	}
}

// TestClassifier_DenyListWinsOverCachedGreen covers an important
// defense-in-depth case: if the deny-list grows to cover a pattern
// that previously cached green, the next classification must return
// red — the deny-list runs BEFORE the cache check.
func TestClassifier_DenyListWinsOverCachedGreen(t *testing.T) {
	model := &stubModel{}
	c := Classifier{DenyList: HardRedPatterns, Model: model, Now: fixedNow}

	in := Inputs{
		ItemID:       "T-345",
		TouchedFiles: []string{"theraprac-infra/state/dev.tfstate"},
	}
	cached := Result{
		Verdict:      VerdictGreen,
		Reason:       "cached green from before deny-list added this prefix",
		InputHash:    in.Hash(),
		ClassifiedBy: "model",
	}

	res, err := c.Classify(in, cached, false)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Verdict != VerdictRed {
		t.Errorf("verdict = %s; want red (deny-list must beat cached green)", res.Verdict)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times; want 0", model.calls)
	}
}

// TestInputs_HashStableAcrossOrdering verifies the cache key is
// independent of slice ordering for Tags and TouchedFiles — agents
// can pass them in any order without busting the cache.
func TestInputs_HashStableAcrossOrdering(t *testing.T) {
	a := Inputs{
		ItemID:       "T-345",
		Tags:         []string{"st-cli", "agent-tooling"},
		TouchedFiles: []string{"a.go", "b.go", "c.go"},
	}
	b := Inputs{
		ItemID:       "T-345",
		Tags:         []string{"agent-tooling", "st-cli"},
		TouchedFiles: []string{"c.go", "a.go", "b.go"},
	}
	if a.Hash() != b.Hash() {
		t.Errorf("hash mismatch on reordered slices:\n  a.Hash() = %s\n  b.Hash() = %s", a.Hash(), b.Hash())
	}
}

// TestInputs_HashChangesOnContentChange verifies the inverse: any
// content change yields a different hash.
func TestInputs_HashChangesOnContentChange(t *testing.T) {
	base := Inputs{ItemID: "T-345", Title: "original"}
	other := base
	other.Title = "edited"
	if base.Hash() == other.Hash() {
		t.Errorf("hash unchanged after Title change; cache would never invalidate")
	}
}

// TestCorpus_EmptyReadReturnsNil covers the first-run case: an
// unbuilt corpus file is not an error, just no entries.
func TestCorpus_EmptyReadReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	entries, err := ReadCorpus(path, 0)
	if err != nil {
		t.Fatalf("ReadCorpus on missing file: %v", err)
	}
	if entries != nil {
		t.Errorf("entries = %v; want nil", entries)
	}
}

// TestCorpus_AppendThenReadRoundtrip verifies AppendCorpus + ReadCorpus
// work together (and that AppendCorpus creates parent dirs).
func TestCorpus_AppendThenReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "corpus.jsonl")

	in1 := CorpusEntry{
		ItemID:         "I-100",
		DecidedAt:      time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		TouchedFiles:   []string{"a.go"},
		Verdict:        VerdictGreen,
		OperatorAction: "approved",
		OperatorReason: "trivial",
	}
	in2 := CorpusEntry{
		ItemID:         "I-101",
		DecidedAt:      time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
		TouchedFiles:   []string{"b.go", "c.go"},
		Verdict:        VerdictRed,
		OperatorAction: "rejected",
		OperatorReason: "RBAC change",
	}

	if err := AppendCorpus(path, in1); err != nil {
		t.Fatalf("AppendCorpus #1: %v", err)
	}
	if err := AppendCorpus(path, in2); err != nil {
		t.Fatalf("AppendCorpus #2: %v", err)
	}

	got, err := ReadCorpus(path, 0)
	if err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d; want 2", len(got))
	}
	if got[0].ItemID != "I-100" || got[1].ItemID != "I-101" {
		t.Errorf("entries out of order: %v", got)
	}
	if got[0].Verdict != VerdictGreen || got[1].Verdict != VerdictRed {
		t.Errorf("verdicts wrong: %v / %v", got[0].Verdict, got[1].Verdict)
	}
}

// TestCorpus_LimitReturnsMostRecent verifies the limit window keeps
// the tail of the log (most recent entries) — the in-context-example
// path wants recency, not the oldest decisions.
func TestCorpus_LimitReturnsMostRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	for i := 0; i < 5; i++ {
		if err := AppendCorpus(path, CorpusEntry{ItemID: fmt.Sprintf("I-%d", i)}); err != nil {
			t.Fatalf("AppendCorpus: %v", err)
		}
	}
	got, err := ReadCorpus(path, 2)
	if err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d; want 2", len(got))
	}
	if got[0].ItemID != "I-3" || got[1].ItemID != "I-4" {
		t.Errorf("got %v; want last two (I-3, I-4)", []string{got[0].ItemID, got[1].ItemID})
	}
}
