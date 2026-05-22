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

// pln is a small helper for tests that build a *plan.Plan with
// a specific FilesToModify / FilesToCreate / ScopeRepos slice.
func pln(scope []string, modify []string, create []string) *plan.Plan {
	return &plan.Plan{
		ScopeRepos:    scope,
		FilesToModify: modify,
		FilesToCreate: create,
	}
}

func TestCheckFileExistence_FlagsMissing(t *testing.T) {
	p := pln(nil, []string{"internal/foo/bar.go"}, nil)
	statter := func(path string) error {
		if strings.Contains(path, "internal/foo/bar.go") {
			return errors.New("not exist")
		}
		return nil
	}
	findings := checkFileExistence(p, "/wsroot", func(string) (string, bool) { return "", false }, statter)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %d (%v)", len(findings), findings)
	}
	if findings[0].Category != CategoryFileMissing {
		t.Errorf("expected CategoryFileMissing; got %s", findings[0].Category)
	}
}

func TestCheckFileExistence_NoFindingsWhenAllExist(t *testing.T) {
	p := pln(nil, []string{"internal/foo/bar.go"}, nil)
	statter := func(string) error { return nil }
	if got := checkFileExistence(p, "/wsroot", func(string) (string, bool) { return "", false }, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
}

// TestCheckFileExistence_RoutesScopedPathToRepoRoot — I-719: when a
// path begins with a known repo prefix (e.g. "as/..."), the stat
// must happen inside the repo root, not under workspaceRoot.
func TestCheckFileExistence_RoutesScopedPathToRepoRoot(t *testing.T) {
	p := pln(nil, []string{"as/internal/foo.go"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil // file exists
	}
	repoRoot := func(name string) (string, bool) {
		if name == "as" {
			return "/agent-root/as", true
		}
		return "", false
	}
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	want := "/agent-root/as/internal/foo.go"
	if statted != want {
		t.Errorf("statter called with %q; want %q (routed via repoRoot, not workspace)", statted, want)
	}
}

// TestCheckFileExistence_FallsBackToWorkspaceForUnknownPrefix —
// paths without a recognized repo prefix AND without a single
// scope_repo continue to resolve under workspaceRoot.
func TestCheckFileExistence_FallsBackToWorkspaceForUnknownPrefix(t *testing.T) {
	// No scope_repos → bare path falls back to workspaceRoot.
	p := pln(nil, []string{"docs/runbook.md"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil
	}
	repoRoot := func(string) (string, bool) { return "", false }
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	want := "/wsroot/docs/runbook.md"
	if statted != want {
		t.Errorf("statter called with %q; want %q (workspace-rooted fallback)", statted, want)
	}
}

// TestCheckFileExistence_StillFlagsMissingScopedFile — when the
// scope repo exists but the referenced file inside it does not,
// the gate must still emit a file-missing finding.
func TestCheckFileExistence_StillFlagsMissingScopedFile(t *testing.T) {
	p := pln(nil, []string{"as/internal/missing.go"}, nil)
	statter := func(string) error { return errors.New("not exist") }
	repoRoot := func(name string) (string, bool) {
		if name == "as" {
			return "/agent-root/as", true
		}
		return "", false
	}
	findings := checkFileExistence(p, "/wsroot", repoRoot, statter)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %d (%v)", len(findings), findings)
	}
	if findings[0].Category != CategoryFileMissing {
		t.Errorf("expected CategoryFileMissing; got %s", findings[0].Category)
	}
}

// TestCheckFileExistence_AbsolutePathStatsAsIs — review F6: the
// absolute-path branch in checkFileExistence's resolution priority
// stats the path verbatim without any prefix or workspace join.
func TestCheckFileExistence_AbsolutePathStatsAsIs(t *testing.T) {
	p := pln(nil, []string{"/etc/hosts.go"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil
	}
	repoRoot := func(string) (string, bool) {
		t.Errorf("repoRoot should NOT be consulted for absolute paths; was called")
		return "", false
	}
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	if statted != "/etc/hosts.go" {
		t.Errorf("statter called with %q; want absolute path as-is", statted)
	}
}

// TestCheckFileExistence_SkipsScopedPathWhenRepoRootUnknown — when
// the closure returns (false) for a known prefix, the path is
// SKIPPED entirely (fail-open).
func TestCheckFileExistence_SkipsScopedPathWhenRepoRootUnknown(t *testing.T) {
	p := pln(nil, []string{"theraprac-api/internal/foo.go"}, nil)
	statted := false
	statter := func(string) error {
		statted = true
		return errors.New("not exist")
	}
	repoRoot := func(string) (string, bool) { return "", false }
	findings := checkFileExistence(p, "/wsroot", repoRoot, statter)
	if len(findings) != 0 {
		t.Errorf("expected fail-open (no findings) when repo absent; got %v", findings)
	}
	if statted {
		t.Errorf("statter should NOT have been called when repo is unknown")
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
	findings := checkGitChurn(p, p.RawText, approved, repoRoot, DefaultThresholds(), runner)
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
	if got := checkGitChurn(p, p.RawText, approved, repoRoot, DefaultThresholds(), runner); len(got) != 0 {
		t.Errorf("2 commits < churn cutoff; expected no findings; got %v", got)
	}
}

// TestCheckGitChurn_StripsRepoPrefix verifies the review F1 fix:
// workspace-prefixed paths (e.g. "theraprac-api/internal/foo.go")
// are stripped to repo-relative form before being passed to `git
// log` inside the per-repo root.
func TestCheckGitChurn_StripsRepoPrefix(t *testing.T) {
	p := &plan.Plan{
		ScopeRepos: []string{"theraprac-api"},
		RawText:    "Touch theraprac-api/internal/auth/middleware.go",
	}
	approved := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	repoRoot := func(name string) (string, bool) { return "/wsroot/" + name, true }

	var capturedArgs []string
	runner := func(root string, args []string) ([]byte, error) {
		capturedArgs = append([]string(nil), args...)
		// Return enough commits to trip churn.
		var lines []string
		for i := 0; i < 12; i++ {
			lines = append(lines, "abc123 c")
		}
		return []byte(strings.Join(lines, "\n")), nil
	}
	findings := checkGitChurn(p, p.RawText, approved, repoRoot, DefaultThresholds(), runner)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %v", findings)
	}
	// Args end with the path list after the `--` sentinel.
	foundStripped := false
	for _, a := range capturedArgs {
		if a == "internal/auth/middleware.go" {
			foundStripped = true
		}
		if a == "theraprac-api/internal/auth/middleware.go" {
			t.Errorf("workspace-prefixed path was not stripped: %v", capturedArgs)
		}
	}
	if !foundStripped {
		t.Errorf("expected stripped path `internal/auth/middleware.go` in git args; got %v", capturedArgs)
	}
}

// === I-720: structured FilesToModify tests ===

// TestCheckFileExistence_IgnoresFilesToCreate — paths in
// Plan.FilesToCreate are never checked (those are by definition
// future files; flagging them was the root I-720 bug).
func TestCheckFileExistence_IgnoresFilesToCreate(t *testing.T) {
	p := pln(nil, []string{"internal/foo/exists.go"}, []string{"internal/foo/new.go"})
	statted := []string{}
	statter := func(path string) error {
		statted = append(statted, path)
		return nil
	}
	if got := checkFileExistence(p, "/wsroot", func(string) (string, bool) { return "", false }, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	for _, s := range statted {
		if strings.Contains(s, "new.go") {
			t.Errorf("FilesToCreate entry was statted (should be ignored): %s", s)
		}
	}
}

// TestCheckFileExistence_BarePathRoutedViaSingleScopeRepo —
// I-720's core fix: a bare path with a single scope_repo routes
// via repoRoot(scope_repos[0]) instead of falling back to
// workspaceRoot.
func TestCheckFileExistence_BarePathRoutedViaSingleScopeRepo(t *testing.T) {
	p := pln([]string{"as"}, []string{"internal/command/plan.go"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil
	}
	repoRoot := func(name string) (string, bool) {
		if name == "as" {
			return "/agent-root/as", true
		}
		return "", false
	}
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	want := "/agent-root/as/internal/command/plan.go"
	if statted != want {
		t.Errorf("statter called with %q; want %q (routed via scope_repos[0])", statted, want)
	}
}

// TestCheckFileExistence_BarePathFailOpenWhenSingleScopeRepoUnknown
// — the unambiguous-scope routing also fail-opens when the
// repoRoot closure returns false (e.g., agent without that
// sibling repo checked out).
func TestCheckFileExistence_BarePathFailOpenWhenSingleScopeRepoUnknown(t *testing.T) {
	p := pln([]string{"theraprac-api"}, []string{"internal/handlers/foo.go"}, nil)
	statted := false
	statter := func(string) error {
		statted = true
		return errors.New("not exist")
	}
	repoRoot := func(string) (string, bool) { return "", false }
	findings := checkFileExistence(p, "/wsroot", repoRoot, statter)
	if len(findings) != 0 {
		t.Errorf("expected fail-open (no findings); got %v", findings)
	}
	if statted {
		t.Errorf("statter should NOT have been called when single-scope repoRoot returns false")
	}
}

// TestCheckFileExistence_BarePathFallsBackUnderMultiScope —
// when the plan has 2+ scope_repos, bare paths are ambiguous and
// the heuristic falls back to workspace-relative resolution.
func TestCheckFileExistence_BarePathFallsBackUnderMultiScope(t *testing.T) {
	p := pln([]string{"as", "theraprac-api"}, []string{"docs/runbook.md"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil
	}
	repoRoot := func(name string) (string, bool) {
		t.Errorf("repoRoot should NOT be consulted for bare path with multi-scope; was called with %q", name)
		return "/never", true
	}
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	if statted != "/wsroot/docs/runbook.md" {
		t.Errorf("statter called with %q; want workspace-rooted fallback", statted)
	}
}

// TestCheckFileExistence_PrefixedPathRoutesViaRepoRoot — explicit
// known-repo prefix routes via repoRoot regardless of
// scope_repos. The explicit prefix wins over the single-scope
// inference.
func TestCheckFileExistence_PrefixedPathRoutesViaRepoRoot(t *testing.T) {
	// Scope says only `as` but the path explicitly names `theraprac-api`.
	p := pln([]string{"as"}, []string{"theraprac-api/internal/foo.go"}, nil)
	var statted string
	statter := func(path string) error {
		statted = path
		return nil
	}
	repoRoot := func(name string) (string, bool) {
		if name == "theraprac-api" {
			return "/agent-root/theraprac-api", true
		}
		return "", false
	}
	if got := checkFileExistence(p, "/wsroot", repoRoot, statter); len(got) != 0 {
		t.Errorf("expected no findings; got %v", got)
	}
	if statted != "/agent-root/theraprac-api/internal/foo.go" {
		t.Errorf("explicit prefix should win over single-scope inference; statted %q", statted)
	}
}

// TestCheckFileExistence_EmptyFilesToModifyNoFindings — an empty
// FilesToModify slice produces no findings (and no statter calls).
// Legitimate case: plan only creates files.
func TestCheckFileExistence_EmptyFilesToModifyNoFindings(t *testing.T) {
	p := pln([]string{"as"}, nil, []string{"internal/foo/new.go"})
	statted := false
	statter := func(string) error {
		statted = true
		return nil
	}
	findings := checkFileExistence(p, "/wsroot", func(string) (string, bool) { return "", false }, statter)
	if len(findings) != 0 {
		t.Errorf("expected no findings on empty FilesToModify; got %v", findings)
	}
	if statted {
		t.Errorf("statter should NOT be called when FilesToModify is empty")
	}
}

// TestNormalizeModifyPath — table-driven coverage of the bullet-
// to-path extraction the I-777 fix introduced. Each authored bullet
// shape encountered in real plans plus a couple of degenerate cases.
func TestNormalizeModifyPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"backtick path with em-dash description",
			"`as/internal/freshness/heuristics.go` — add normalizeModifyPath helper",
			"as/internal/freshness/heuristics.go"},
		{"backtick path with en-dash description",
			"`internal/foo.go` – does the thing",
			"internal/foo.go"},
		{"backtick path with double-hyphen description",
			"`internal/foo.go` -- markdown-safe variant",
			"internal/foo.go"},
		{"backtick path with spaced single-hyphen description",
			"`internal/foo.go` - terse description",
			"internal/foo.go"},
		{"bare path with em-dash description (no backticks)",
			"internal/foo.go — adds the bar helper",
			"internal/foo.go"},
		{"bare backticked path (no description)",
			"`internal/foo.go`",
			"internal/foo.go"},
		{"plain bare path stays untouched",
			"internal/foo.go",
			"internal/foo.go"},
		{"internal-hyphen path is preserved (no spaced-hyphen match)",
			"internal/foo-bar/baz.go",
			"internal/foo-bar/baz.go"},
		{"backticked internal-hyphen path with description",
			"`internal/foo-bar/baz.go` — desc",
			"internal/foo-bar/baz.go"},
		{"prose-then-backtick path",
			"see `internal/foo.go` for details",
			"internal/foo.go"},
		{"empty input → empty (caller skips)",
			"",
			""},
		{"whitespace-only → empty",
			"   \t  ",
			""},
		{"description fully inside one backtick span",
			"`internal/foo.go — desc`",
			"internal/foo.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeModifyPath(tc.in); got != tc.want {
				t.Errorf("normalizeModifyPath(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckFileExistence_ParsesBacktickedDescribedBullet — the
// regression case for I-777. A FilesToModify entry that arrives
// as the authored "`path` — description" prose must be statted at
// just the path, not the whole bullet. Statter succeeds only for
// the bare path; expect zero findings.
func TestCheckFileExistence_ParsesBacktickedDescribedBullet(t *testing.T) {
	const bullet = "`as/internal/command/watch.go` — add the progress channel to poll()"
	const wantPath = "internal/command/watch.go"

	p := pln([]string{"as"}, []string{bullet}, nil)
	var statted []string
	statter := func(path string) error {
		statted = append(statted, path)
		if strings.HasSuffix(path, wantPath) {
			return nil
		}
		return errors.New("not exist")
	}
	repoRoot := func(name string) (string, bool) {
		if name == "as" {
			return "/agent-root/as", true
		}
		return "", false
	}
	findings := checkFileExistence(p, "/wsroot", repoRoot, statter)
	if len(findings) != 0 {
		t.Errorf("expected zero findings (path should be extracted from bullet); got %v", findings)
	}
	if len(statted) != 1 || !strings.HasSuffix(statted[0], wantPath) || strings.Contains(statted[0], "—") {
		t.Errorf("statter calls: %v — want exactly one ending in %q with no em-dash", statted, wantPath)
	}
}
