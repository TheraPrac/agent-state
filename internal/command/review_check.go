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

	if verdict != "pass" {
		// Check for operator-approved review_skips before failing.
		// GetField returns (value, true) when the key exists (even as a block scalar),
		// so check the bool rather than the string value.
		hasSkips := false
		if item.Doc != nil {
			_, hasSkips = item.Doc.GetField("review_skips")
		}
		if !hasSkips {
			fmt.Fprintf(os.Stderr, "%s: review did not pass (verdict=%s, SHA=%s) — re-run `st review %s`\n",
				id, verdict, recordedSHA, id)
			return 1
		}
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

	fmt.Printf("%s: review_evidence OK (pass %s)\n", id, recordedSHA)
	return 0
}

// resolveCurrentSHA returns the short HEAD SHA for the item's primary worktree repo.
// Returns "" when the SHA cannot be resolved (non-blocking — skip SHA check).
// When GitHeadSHA is injected (tests), falls back to calling it with "." when
// no worktree repo dir is found, so tests can exercise the SHA-mismatch path.
func resolveCurrentSHA(cfg *config.Config, id string, opts ReviewCheckOpts) string {
	gitFn := func(dir string) (string, error) {
		if opts.GitHeadSHA != nil {
			return opts.GitHeadSHA(dir)
		}
		return runGit(dir, "rev-parse", "HEAD")
	}

	if cfg.Worktree != nil && len(cfg.Worktree.Repos) > 0 {
		for _, repo := range cfg.Worktree.Repos {
			dir := resolveRepoDirForItem(cfg, id, repo)
			if dir == "" || dir == repo {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			out, err := gitFn(dir)
			if err != nil {
				continue
			}
			sha := strings.TrimSpace(out)
			if len(sha) > 7 {
				sha = sha[:7]
			}
			return sha
		}
	}

	// No worktree dir found — if GitHeadSHA is injected, call it with "." so
	// tests that inject a specific SHA can still exercise the SHA-mismatch path.
	if opts.GitHeadSHA != nil {
		out, err := gitFn(".")
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
