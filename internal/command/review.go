package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/evidence"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// defaultReviewWallTimeout caps the review sub-agent. Reviews rarely need more
// than 10 minutes; the 2h global ceiling is far too loose for a diff-scoped pass.
// Operator override: AS_CLAUDE_WALL_TIMEOUT (duration string injected via ExtraEnv).
const defaultReviewWallTimeout = 10 * time.Minute

// ReviewOpts holds injectable dependencies for the review command.
type ReviewOpts struct {
	Engine  RunEngine
	Backend evidence.Backend
	// RunGit is injectable for tests; nil uses the real git binary.
	RunGit func(dir string, args ...string) (string, error)
	// CollectDiff overrides diff collection entirely for tests.
	// Returns (combinedDiff, primarySHA, error).
	CollectDiff func(cfg *config.Config, id string) (string, string, error)
}

// ReviewViolation is a single rule violation found in a changed file.
type ReviewViolation struct {
	RuleID      string `json:"rule_id"`
	Line        int    `json:"line"`
	Finding     string `json:"finding"`
	DiffContext string `json:"diff_context"`
}

// ReviewFile holds per-file review results emitted by the sub-agent.
type ReviewFile struct {
	Path                string            `json:"path"`
	ApplicableStandards []string          `json:"applicable_standards"`
	Violations          []ReviewViolation `json:"violations"`
	Status              string            `json:"status"` // "pass" or "fail"
}

// ReviewReport is the structured JSON the review sub-agent must emit.
type ReviewReport struct {
	ReviewedSHA string       `json:"reviewed_sha"`
	Verdict     string       `json:"verdict"` // "pass" or "fail"
	Files       []ReviewFile `json:"files"`
	Summary     string       `json:"summary"`
}

// fileReviewStandards maps a file path to the applicable bugbot rule sections.
// Returns nil for file types with no applicable rules (e.g. JSON, YAML config).
func fileReviewStandards(path string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	base := filepath.Base(path)
	slashed := filepath.ToSlash(path)

	// Go backend source
	if ext == ".go" && !strings.HasSuffix(base, "_test.go") {
		return []string{"bugbot-rules: Backend (Go) Rules 1-6, 10-15, 20-26"}
	}
	// Go test files — fewer applicable rules
	if strings.HasSuffix(base, "_test.go") {
		return []string{"bugbot-rules: Backend (Go) Rules 14 (dead code), 26 (comments match implementation)"}
	}
	// TypeScript / React
	if ext == ".ts" || ext == ".tsx" {
		return []string{"bugbot-rules: Frontend (React/Next.js) Rules 9, 11-19"}
	}
	// Liquibase SQL/XML changesets
	if (ext == ".sql" || ext == ".xml") && strings.Contains(slashed, "db/changelog") {
		return []string{"bugbot-rules: Liquibase Migration Rules 27-34"}
	}
	// Bash scripts
	if ext == ".sh" {
		return []string{"bugbot-rules: Bash Scripts Rules 16-20"}
	}
	// OpenAPI YAML
	if (ext == ".yaml" || ext == ".yml") && strings.Contains(slashed, "openapi") {
		return []string{"bugbot-rules: OpenAPI YAML Rule 8"}
	}
	return nil
}

// Review performs autonomous code review for an item's diff against the bugbot rules.
// Evidence is written to the review_evidence field and uploaded to S3.
// Returns 0 on pass, 1 on fail or error.
func Review(s *store.Store, cfg *config.Config, id string, opts ReviewOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active to review\n", id, item.Status)
		return 1
	}

	// Collect diffs across all worktree repos.
	collectFn := opts.CollectDiff
	if collectFn == nil {
		collectFn = func(c *config.Config, itemID string) (string, string, error) {
			return collectItemDiff(c, itemID, opts)
		}
	}
	diff, sha, err := collectFn(cfg, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: collecting diff: %v\n", id, err)
		return 1
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Fprintf(os.Stderr, "%s: no diff found against origin/main — nothing to review\n", id)
		return 1
	}
	if sha == "" {
		fmt.Fprintf(os.Stderr, "%s: no HEAD SHA resolved for the reviewed diff — cannot record evidence\n", id)
		return 1
	}

	// Build and execute the review sub-agent prompt.
	prompt := buildReviewPrompt(id, sha, diff)
	cwd := cfg.Root()
	reviewSessionID := generateSessionID()
	step := config.RunStepDef{
		Type:     "claude",
		Prompt:   prompt,
		ExtraEnv: []string{"AS_CLAUDE_WALL_TIMEOUT=" + defaultReviewWallTimeout.String()},
	}
	step.SetName("as_review")

	sr := executeClaude(s, cfg, id, "", step, RunOpts{NoCoordination: true}, opts.Engine, cwd, reviewSessionID, false)
	if !sr.Passed {
		fmt.Fprintf(os.Stderr, "%s: review sub-agent failed: %s\n", id, sr.Error)
		return writeReviewEvidence(s, cfg, id, sha, "fail", nil, opts)
	}

	// Parse JSON report from sub-agent output.
	report, parseErr := parseReviewReport(sr.FullOutput)
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "%s: parsing review report: %v\nRaw output:\n%s\n", id, parseErr, sr.FullOutput)
		return writeReviewEvidence(s, cfg, id, sha, "fail", nil, opts)
	}

	// Reject stale-SHA reports — the sub-agent echoes back the SHA we gave it,
	// so a mismatch means the output is from a previous stale session.
	if report.ReviewedSHA != "" && report.ReviewedSHA != sha {
		fmt.Fprintf(os.Stderr, "%s: review report SHA mismatch (report=%s, current=%s)\n",
			id, report.ReviewedSHA, sha)
		return writeReviewEvidence(s, cfg, id, sha, "fail", report, opts)
	}

	printReviewReport(id, report)

	verdict := report.Verdict
	if verdict != "pass" && verdict != "fail" {
		verdict = "fail"
	}
	return writeReviewEvidence(s, cfg, id, sha, verdict, report, opts)
}

// collectItemDiff collects git diffs from all worktree repos for the item.
// Returns the combined diff text and the HEAD SHA from the first repo with a diff.
func collectItemDiff(cfg *config.Config, id string, opts ReviewOpts) (diff, sha string, err error) {
	if cfg.Worktree == nil {
		return "", "", fmt.Errorf("worktree not configured")
	}

	gitFn := func(dir string, args ...string) (string, error) {
		if opts.RunGit != nil {
			return opts.RunGit(dir, args...)
		}
		return runGit(dir, args...)
	}

	var parts []string
	for _, repo := range cfg.Worktree.Repos {
		dir := resolveRepoDirForItem(cfg, id, repo)
		if dir == "" || dir == repo {
			continue
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}

		// Get HEAD SHA (short).
		headOut, gitErr := gitFn(dir, "rev-parse", "HEAD")
		if gitErr != nil {
			continue
		}
		headSHA := strings.TrimSpace(headOut)
		if len(headSHA) > 7 {
			headSHA = headSHA[:7]
		}

		// Prefer diff vs origin/main; fall back to HEAD^ for orphan branches.
		diffOut, gitErr := gitFn(dir, "diff", "origin/main...HEAD")
		if gitErr != nil || strings.TrimSpace(diffOut) == "" {
			diffOut, gitErr = gitFn(dir, "diff", "HEAD^...HEAD")
			if gitErr != nil {
				continue
			}
		}
		if strings.TrimSpace(diffOut) == "" {
			continue
		}
		// Set SHA from the first repo that actually contributed to the diff,
		// so the staleness sentinel tracks the reviewed code, not an unrelated repo.
		if sha == "" {
			sha = headSHA
		}
		parts = append(parts, fmt.Sprintf("=== Repo: %s (SHA: %s) ===\n%s", repo, headSHA, diffOut))
	}
	return strings.Join(parts, "\n\n"), sha, nil
}

// buildReviewPrompt constructs the prompt for the code-review sub-agent.
// The sub-agent must emit ONLY the ReviewReport JSON.
func buildReviewPrompt(id, sha, diff string) string {
	return fmt.Sprintf(`You are performing an automated code review for item %s (current HEAD SHA: %s).

Review the diff below against the TheraPrac bugbot rules and emit a ReviewReport JSON object.

## Applicable Rules Reference

### Backend (Go) — Rules 1-6, 10-15, 20-26
1. RLS policies for ALL granted roles — every role needs explicit policies; implicit deny = silent data loss.
2. Derived values consistent — read actual stored value, don't re-derive from partial inputs.
3. Errors vs skips — use (bool, error) not just bool. (false, nil)=skip, (false, err)=failure.
4. FOR UPDATE on read-then-write — any read-modify-write needs locking.
5. GRANT matches operations — if code does INSERT+UPDATE+SELECT, migration must grant all three.
6. gocritic ifElseChain — 3+ branches must be switch.
10. sql.DB vs sql.Conn — session-scoped state (SET, temp tables) requires db.Conn(ctx), not pool.
11. pq.QuoteIdentifier on dotted names — split on ".", quote each part separately.
12. No fmt.Sprintf with SQL identifiers — use pq.QuoteIdentifier or parameterized queries.
13. Backward compatibility — new required params need defaults/fallbacks for existing callers.
14. Dead code cleanup — remove unused vars, imports, copy-paste leftovers.
15. Credential pairs must match — DB_USER/DB_PASSWORD must correspond to same role.
20. Shared stateful services = single instance — inject via setter, never create ephemeral instances in handlers.
21. In-memory shared state needs sync.RWMutex — maps + struct fields accessed from multiple goroutines.
22. CloudWatch alarms need alarm_actions — alarm without SNS = silent state changes.
23. Nil guard consistency — if one method has nil fallback, all methods using that dependency must too.
24. ON CONFLICT must update ALL mutable columns — omitting a column preserves stale value.
25. sql.ErrNoRows vs transient errors — check ErrNoRows explicitly, propagate other errors.
26. Comments must match implementation — verify every safety/concurrency claim by grepping actual code paths.

### Bash Scripts — Rules 16-20
16. set -u safety — use ${VAR:-} default syntax for potentially unset vars.
17. pipefail + grep — append || true when grep-no-match is expected.
18. source propagation — source script.sh propagates set -euo pipefail to caller.
19. Secret masking — never echo credentials.
20. Safety mechanisms — dry_run/confirmation checked for ALL code paths.

### Frontend (React/Next.js) — Rules 9, 11-19
9. Generated types only — import from @/lib/generated/models, never hand-write duplicates.
11. Cross-entity cache invalidation — mutation onSuccess must invalidate ALL entities backend modifies.
12. Query key factories — use xxxKeys.all, never hardcode key arrays.
13. Strict numeric checks — if (numericValue !== undefined), not if (numericValue).
14. Loading state gates — never act on feature flags without checking isLoading.
15. SSR safety — dynamic import() in useEffect for browser-only libs, never static import.
16. Hidden inputs don't reset — useEffect to clear state when feature becomes unavailable.
17. useRef for one-time defaults — prevent re-triggers when user clears input.
18. vi.hoisted() — mock variables in vi.mock factories must use vi.hoisted().
19. OpenAPI schema completeness — missing field in schema = silently dropped by generated *ToJSON().

### Liquibase Migration — Rules 27-34
27. DO blocks need splitStatements="false" — Liquibase can't parse $$ dollar-quoted strings.
28. ADD CONSTRAINT needs idempotency — wrap in DO block with pg_constraint check.
29. Never clearCheckSums on remote DBs — destroys changelog history.
30. Never changelogSync as repair — creates phantom state.
31. TRUNCATE CASCADE ignores session_replication_role — use backup/restore pattern.
32. INSERT seed data must use ON CONFLICT — for re-runnability.
33. validCheckSum ANY — add to modified changesets after initial deployment.
34. Pre-push idempotency gate — CREATE TABLE/INDEX must use IF NOT EXISTS.

### OpenAPI YAML — Rule 8
8. YAML indentation false positives — verify with cat -A before accepting indentation findings.

## File-to-Rules Mapping
- .go (non-test) → Backend (Go) Rules 1-6, 10-15, 20-26
- _test.go → Rules 14, 26
- .ts / .tsx → Frontend Rules 9, 11-19
- db/changelog/*.sql or .xml → Liquibase Migration Rules 27-34
- .sh → Bash Scripts Rules 16-20
- openapi/*.yaml → OpenAPI Rule 8
- Other file types → no applicable rules; omit from output

## Diff to Review

%s

## Output Requirements

Emit ONLY valid JSON (no prose, no markdown, no code blocks). The JSON must exactly match:

{
  "reviewed_sha": "%s",
  "verdict": "pass",
  "files": [
    {
      "path": "relative/path/to/file.go",
      "applicable_standards": ["bugbot-rules: Backend (Go) Rules 1-6, 10-15, 20-26"],
      "violations": [],
      "status": "pass"
    }
  ],
  "summary": "N files reviewed, M violations found."
}

Rules:
- reviewed_sha MUST be exactly %q
- verdict is "pass" only when ALL files have zero violations; "fail" otherwise
- Include only files that have applicable rules per the mapping above
- violations is [] when no violations found for a file
- Each violation must set rule_id (e.g. "11"), line (0 if unknown), finding, diff_context
- Emit ONLY the JSON object — nothing before or after it
`, id, sha, diff, sha, sha)
}

// parseReviewReport extracts and validates a ReviewReport JSON from Claude's output.
// Uses json.Decoder to read exactly one top-level JSON object starting from the
// first '{', ignoring any leading prose or trailing content (including stray braces
// that appear in diff context or Claude's annotations).
func parseReviewReport(output string) (*ReviewReport, error) {
	output = strings.TrimSpace(output)
	start := strings.Index(output, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found in output")
	}
	dec := json.NewDecoder(strings.NewReader(output[start:]))
	var report ReviewReport
	if err := dec.Decode(&report); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}
	return &report, nil
}

// printReviewReport prints a human-readable review summary.
func printReviewReport(id string, report *ReviewReport) {
	violationCount := 0
	for _, f := range report.Files {
		for _, v := range f.Violations {
			violationCount++
			fmt.Fprintf(os.Stderr, "%s: VIOLATION rule %s in %s (line %d): %s\n",
				id, v.RuleID, f.Path, v.Line, v.Finding)
		}
	}
	if report.Verdict == "pass" {
		fmt.Printf("%s: review PASSED (SHA %s) — %s\n", id, report.ReviewedSHA, report.Summary)
	} else {
		fmt.Fprintf(os.Stderr, "%s: review FAILED (SHA %s, %d violation(s)) — %s\n",
			id, report.ReviewedSHA, violationCount, report.Summary)
	}
}

// writeReviewEvidence uploads the report and records review_evidence on the item.
// Returns 0 when verdict=="pass", 1 otherwise.
func writeReviewEvidence(s *store.Store, cfg *config.Config, id, sha, verdict string, report *ReviewReport, opts ReviewOpts) int {
	backend := opts.Backend
	if backend == nil {
		var berr error
		backend, berr = evidence.New(evidenceConfigFromCfg(cfg))
		if berr != nil {
			fmt.Fprintf(os.Stderr, "warning: evidence backend unavailable: %v\n", berr)
		}
	}

	now := time.Now()
	logURI := ""
	if backend != nil && report != nil {
		keyPrefix := fmt.Sprintf("%s/review/%s/%s", id, sha, now.Format("20060102T150405"))
		reportJSON, _ := json.MarshalIndent(report, "", "  ")
		uri, uploadErr := evidence.GzipUpload(backend, keyPrefix+"/report.json", reportJSON)
		if uploadErr != nil {
			fmt.Fprintf(os.Stderr, "warning: review evidence upload failed: %v\n", uploadErr)
		} else {
			logURI = uri
		}
	}

	ev := fmt.Sprintf("%s %s %s evidence:%s", verdict, sha, now.Format(time.RFC3339), logURI)
	if err := s.Mutate(id, func(it *model.Item) error {
		it.Doc.SetField("review_evidence", ev)
		it.Doc.SetField("last_touched", now.Format(time.RFC3339))
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "review_recorded", Field: "review_evidence", NewValue: ev,
	})

	if err := autoSync(s, fmt.Sprintf("st review: %s", id)); err != nil {
		// Sync failure is non-fatal — evidence is already written; don't mask the verdict.
		fmt.Fprintf(os.Stderr, "warning: %s: sync failed (evidence already written): %v\n", id, err)
	}

	if verdict == "pass" {
		return 0
	}
	return 1
}
