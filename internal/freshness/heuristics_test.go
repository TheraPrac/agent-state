package freshness

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jfinlinson/agent-state/internal/plan"
)

func TestExtractReferencedPaths_FindsSourcePaths(t *testing.T) {
	body := `## Approach

Touch internal/foo/bar.go and theraprac-web/components/Baz.tsx and
docs/runbook.md, plus the prose "the authentication system".`

	got := extractReferencedPaths(body)
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}
	want := []string{
		"internal/foo/bar.go",
		"theraprac-web/components/Baz.tsx",
		"docs/runbook.md",
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("expected path %q in extraction; got %v", w, got)
		}
	}
}

func TestCheckFileExistence_FlagsMissing(t *testing.T) {
	body := "see internal/foo/bar.go for context"
	statter := func(path string) error {
		if strings.Contains(path, "internal/foo/bar.go") {
			return errors.New("not exist")
		}
		return nil
	}
	findings := checkFileExistence(body, "/wsroot", statter)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %d (%v)", len(findings), findings)
	}
	if findings[0].Category != CategoryFileMissing {
		t.Errorf("expected CategoryFileMissing; got %s", findings[0].Category)
	}
}

func TestCheckFileExistence_NoFindingsWhenAllExist(t *testing.T) {
	body := "see internal/foo/bar.go"
	statter := func(string) error { return nil }
	if got := checkFileExistence(body, "/wsroot", statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
}

func TestCheckAge_FreshWithinDriftWindow(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	approved := now.Add(-1 * 24 * time.Hour) // 1 day ago
	if got := checkAge(approved, now, DefaultThresholds()); len(got) != 0 {
		t.Errorf("expected no findings within drift window; got %v", got)
	}
}

func TestCheckAge_DriftAfterSoftThreshold(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	approved := now.Add(-8 * 24 * time.Hour) // 8 days ago
	got := checkAge(approved, now, DefaultThresholds())
	if len(got) != 1 || got[0].Category != CategoryAgeThreshold {
		t.Errorf("expected one age finding; got %v", got)
	}
}

func TestCheckAge_StaleAfterHardThreshold(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	approved := now.Add(-20 * 24 * time.Hour) // 20 days ago
	got := checkAge(approved, now, DefaultThresholds())
	if len(got) != 1 || got[0].Category != CategoryAgeThreshold {
		t.Errorf("expected one age finding; got %v", got)
	}
	if !strings.Contains(got[0].Message, "stale cutoff") {
		t.Errorf("expected stale-cutoff wording; got %q", got[0].Message)
	}
}

func TestApproachKeywords_StopsShortWords(t *testing.T) {
	kws := approachKeywords("Refactor the auth middleware to drop session tokens.")
	for _, want := range []string{"refactor", "auth", "middleware", "session", "tokens"} {
		if !kws[want] {
			t.Errorf("expected keyword %q; got %v", want, kws)
		}
	}
	if kws["the"] || kws["to"] {
		t.Errorf("short stopwords should be dropped; got %v", kws)
	}
}

func TestClassifyHeuristics_FreshOnEmpty(t *testing.T) {
	if got := classifyHeuristics(nil, 0, DefaultThresholds()); got != VerdictFresh {
		t.Errorf("empty findings → expected Fresh; got %s", got)
	}
}

func TestClassifyHeuristics_StaleOnFileMissing(t *testing.T) {
	findings := []Finding{{Category: CategoryFileMissing}}
	if got := classifyHeuristics(findings, 0, DefaultThresholds()); got != VerdictStale {
		t.Errorf("file-missing → expected Stale; got %s", got)
	}
}

func TestClassifyHeuristics_StaleOnAgeOverHard(t *testing.T) {
	th := DefaultThresholds()
	findings := []Finding{{Category: CategoryAgeThreshold}}
	if got := classifyHeuristics(findings, th.StaleAfter+time.Hour, th); got != VerdictStale {
		t.Errorf("age over hard cutoff → expected Stale; got %s", got)
	}
}

func TestClassifyHeuristics_DriftOnAgeBetweenCutoffs(t *testing.T) {
	th := DefaultThresholds()
	findings := []Finding{{Category: CategoryAgeThreshold}}
	if got := classifyHeuristics(findings, th.DriftAfter+time.Hour, th); got != VerdictDrift {
		t.Errorf("age between cutoffs → expected Drift; got %s", got)
	}
}

func TestCheckGitChurn_FlagsAboveThreshold(t *testing.T) {
	p := &plan.Plan{
		ScopeRepos: []string{"as"},
		RawText:    "Touch internal/foo/bar.go and internal/baz.go",
	}
	approved := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repoRoot := func(name string) (string, bool) {
		if name == "as" {
			return "/wsroot/as", true
		}
		return "", false
	}
	runner := func(root string, args []string) ([]byte, error) {
		// Simulate 12 commits.
		var lines []string
		for i := 0; i < 12; i++ {
			lines = append(lines, "abc123 commit message")
		}
		return []byte(strings.Join(lines, "\n")), nil
	}
	findings := checkGitChurn(p, approved, repoRoot, DefaultThresholds(), runner)
	if len(findings) != 1 || findings[0].Category != CategoryGitChurn {
		t.Errorf("expected 1 git-churn finding; got %v", findings)
	}
}

func TestCheckGitChurn_NoFindingBelowThreshold(t *testing.T) {
	p := &plan.Plan{
		ScopeRepos: []string{"as"},
		RawText:    "Touch internal/foo/bar.go",
	}
	approved := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repoRoot := func(name string) (string, bool) { return "/wsroot/as", true }
	runner := func(root string, args []string) ([]byte, error) {
		return []byte("abc123 c1\ndef456 c2"), nil
	}
	if got := checkGitChurn(p, approved, repoRoot, DefaultThresholds(), runner); len(got) != 0 {
		t.Errorf("2 commits < churn cutoff; expected no findings; got %v", got)
	}
}
