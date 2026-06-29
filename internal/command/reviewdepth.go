package command

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// Depth bucketing thresholds (see I-147 SBAR recommendation).
const (
	reviewDepthSmallLines = 50
	reviewDepthSmallFiles = 3
	reviewDepthHighLines  = 200
	reviewDepthHighFiles  = 6
)

// blastRadiusPaths contains path fragments that unconditionally route to "high"
// regardless of diff size (from CLAUDE.md rule 5 + I-147 SBAR).
var blastRadiusPaths = []string{
	"internal/auth/",
	"db/changelog/",
	"claude-config/hooks/",
	".github/workflows/",
	"theraprac-infra/",
	"ansible/",
}

// statLineRe matches the trailing summary line of `git diff --stat`:
// " N files changed, M insertions(+), K deletions(-)"
var statLineRe = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// ReviewDepthOpts holds injectable dependencies for unit testing.
type ReviewDepthOpts struct {
	// RunGit is injectable for tests; nil uses the real git binary.
	RunGit func(dir string, args ...string) (string, error)
}

// ReviewDepth computes a recommended /code-review depth for the item based on
// the combined diff across all its worktree repos. Prints one of "low",
// "medium", or "high" to stdout and returns 0 on success.
func ReviewDepth(s *store.Store, cfg *config.Config, id string, opts ReviewDepthOpts) int {
	if _, ok := s.Get(id); !ok {
		fmt.Fprintf(os.Stderr, "review-depth: %s not found\n", id)
		return 1
	}

	gitFn := opts.RunGit
	if gitFn == nil {
		gitFn = func(dir string, args ...string) (string, error) { return runGit(dir, args...) }
	}

	var totalFiles, totalLines int
	var changedPaths []string

	for _, repo := range worktreeRepos(cfg) {
		dir := resolveRepoDirForItem(cfg, id, repo)
		if dir == "" || dir == repo {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		statOut, err := gitFn(dir, "diff", "--stat", "origin/main...HEAD")
		if err != nil || strings.TrimSpace(statOut) == "" {
			statOut, err = gitFn(dir, "diff", "--stat", "HEAD^...HEAD")
			if err != nil {
				continue
			}
		}
		files, lines := parseDiffStat(statOut)
		totalFiles += files
		totalLines += lines

		namesOut, err := gitFn(dir, "diff", "--name-only", "origin/main...HEAD")
		if err == nil {
			for _, p := range strings.Split(strings.TrimSpace(namesOut), "\n") {
				if p != "" {
					changedPaths = append(changedPaths, p)
				}
			}
		}
	}

	fmt.Println(computeDepth(totalFiles, totalLines, changedPaths))
	return 0
}

// worktreeRepos returns the list of repo aliases from the worktree config.
func worktreeRepos(cfg *config.Config) []string {
	if cfg == nil || cfg.Worktree == nil {
		return nil
	}
	out := make([]string, len(cfg.Worktree.Repos))
	copy(out, cfg.Worktree.Repos)
	return out
}

// computeDepth applies the bucketing logic. Pure function for testability.
func computeDepth(files, lines int, paths []string) string {
	if hasBlastRadiusPath(paths) {
		return "high"
	}
	if lines >= reviewDepthHighLines || files >= reviewDepthHighFiles {
		return "high"
	}
	if lines <= reviewDepthSmallLines && files <= reviewDepthSmallFiles {
		return "low"
	}
	return "medium"
}

// hasBlastRadiusPath reports whether any path in paths matches a
// blast-radius fragment (case-insensitive substring match).
func hasBlastRadiusPath(paths []string) bool {
	for _, p := range paths {
		pl := strings.ToLower(p)
		for _, frag := range blastRadiusPaths {
			if strings.Contains(pl, strings.ToLower(frag)) {
				return true
			}
		}
	}
	return false
}

// parseDiffStat extracts total file count and total line delta from
// `git diff --stat` output. Returns (0, 0) when the summary line is absent.
func parseDiffStat(stat string) (files, lines int) {
	m := statLineRe.FindStringSubmatch(stat)
	if m == nil {
		return 0, 0
	}
	files, _ = strconv.Atoi(m[1])
	ins, _ := strconv.Atoi(m[2])
	del, _ := strconv.Atoi(m[3])
	return files, ins + del
}
