package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// ReviewCheckOpts holds injectable dependencies for review-check.
type ReviewCheckOpts struct {
	GitHeadSHA func(repoDir string) (string, error)
}

// ReviewCheck verifies that the active item has a passing review_evidence entry
// whose SHA matches the current HEAD. Returns 0 on pass, 1 on fail.
func ReviewCheck(s *store.Store, cfg *config.Config, id string, opts ReviewCheckOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s: review-check on non-active item (status=%s) — stale evidence is not valid\n", id, item.Status)
		return 1
	}

	// Read review_evidence field.
	evField := ""
	if item.Doc != nil {
		evField, _ = item.Doc.GetField("review_evidence")
	}
	if evField == "" {
		fmt.Fprintf(os.Stderr, "%s: no review_evidence recorded — run `st review %s` first\n", id, id)
		return 1
	}

	// Parse: "<verdict> <sha> <timestamp> evidence:<uri>"
	parts := strings.Fields(evField)
	if len(parts) < 2 {
		fmt.Fprintf(os.Stderr, "%s: malformed review_evidence: %q\n", id, evField)
		return 1
	}
	verdict := parts[0]
	recordedSHA := parts[1]

	// item.Doc is non-nil here: a nil Doc leaves evField empty, triggering the
	// early return above before we reach this point.
	usedSkips := false
	if verdict != "pass" {
		// Check for operator-approved review_skips before failing.
		// HasFieldContent distinguishes a field with actual content from an
		// explicit empty value (review_skips: ""), which must not bypass the gate.
		if !item.Doc.HasFieldContent("review_skips") {
			fmt.Fprintf(os.Stderr, "%s: review did not pass (verdict=%s, SHA=%s) — re-run `st review %s`\n",
				id, verdict, recordedSHA, id)
			return 1
		}
		usedSkips = true
		fmt.Printf("%s: review verdict=%s but review_skips recorded — operator-approved skips applied (SHA=%s)\n",
			id, verdict, recordedSHA)
	}

	// Validate SHA against current HEAD in the primary worktree repo.
	currentSHA := resolveCurrentSHA(cfg, id, opts)
	if currentSHA != "" && currentSHA != recordedSHA {
		fmt.Fprintf(os.Stderr,
			"%s: review_evidence SHA mismatch (recorded=%s, current HEAD=%s) — re-run `st review %s`\n",
			id, recordedSHA, currentSHA, id)
		return 1
	}

	if usedSkips {
		fmt.Printf("%s: review_evidence OK (verdict=fail, skips applied, SHA=%s)\n", id, recordedSHA)
	} else {
		fmt.Printf("%s: review_evidence OK (pass %s)\n", id, recordedSHA)
	}
	return 0
}

// resolveCurrentSHA returns the short HEAD SHA of the repo whose code was
// reviewed — the first worktree repo with an item diff against origin/main,
// matching how `st review` records reviewed_sha (collectItemDiff). Picking the
// "first repo with a diff" rather than the "first existing repo" is the I-1651
// fix: with per-item worktrees created for every repo, the first repo (often an
// untouched theraprac-api at main) used to win, so an as-only item's recorded
// SHA was compared against the wrong repo's HEAD and spuriously mismatched.
// When no repo has a diff, falls back to the first existing repo's HEAD
// (prior behavior). Returns "" when the SHA cannot be resolved (non-blocking —
// skip SHA check). When GitHeadSHA is injected (tests), falls back to calling
// it with "." when no worktree repo dir is found, so tests can exercise the
// SHA-mismatch path.
func resolveCurrentSHA(cfg *config.Config, id string, opts ReviewCheckOpts) string {
	// Route the injected rev-parse-only GitHeadSHA through a full gitFn shim so
	// reviewedRepoSHA's diff/log commands still hit real git in production
	// (GitHeadSHA is nil there) while tests can stub HEAD resolution.
	gitFn := func(dir string, args ...string) (string, error) {
		if opts.GitHeadSHA != nil && len(args) > 0 && args[0] == "rev-parse" {
			return opts.GitHeadSHA(dir)
		}
		return runGit(dir, args...)
	}

	if cfg.Worktree != nil && len(cfg.Worktree.Repos) > 0 {
		firstExisting := ""
		for _, repo := range cfg.Worktree.Repos {
			dir := resolveRepoDirForItem(cfg, id, repo)
			if dir == "" || dir == repo {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			sha, _, hasDiff := reviewedRepoSHA(dir, gitFn)
			if sha == "" {
				continue
			}
			if hasDiff {
				return sha // the repo whose code was reviewed — matches reviewed_sha
			}
			if firstExisting == "" {
				firstExisting = sha
			}
		}
		// No repo had an item diff — fall back to the first existing repo's HEAD.
		if firstExisting != "" {
			return firstExisting
		}
	}

	// No worktree dir found — if GitHeadSHA is injected, call it with "." so
	// tests that inject a specific SHA can still exercise the SHA-mismatch path.
	if opts.GitHeadSHA != nil {
		out, err := opts.GitHeadSHA(".")
		if err != nil || strings.TrimSpace(out) == "" {
			return ""
		}
		sha := strings.TrimSpace(out)
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return sha
	}
	return ""
}
