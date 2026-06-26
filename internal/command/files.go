package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
)

// FilesOpts controls st files output.
type FilesOpts struct {
	JSON bool
	// Injectable for tests: if nil, uses resolveRepoDirForItem + real git.
	ResolveRepo func(cfg *config.Config, itemID, repo string) string
	RunGit      func(dir string, args ...string) (string, error)
}

// FileChange is a per-file diff entry reported by Files.
type FileChange struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Action  string `json:"action"` // M, A, D, R
	Type    string `json:"type"`   // app, test, doc, config, spec, migration
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Net     int    `json:"net"`
}

// RepoRollup is the per-repo summary of file changes.
type RepoRollup struct {
	Repo    string `json:"repo"`
	Files   int    `json:"files"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Net     int    `json:"net"`
}

// FilesResult is what Files returns (also emitted as JSON when --json is set).
type FilesResult struct {
	ItemID   string       `json:"item_id"`
	Files    []FileChange `json:"files"`
	Repos    []RepoRollup `json:"repos"`
	Totals   RepoRollup   `json:"totals"`
	Warnings []string     `json:"warnings,omitempty"`
}

// Files reports the live diff across all worktrees the item knows about.
// Exit codes: 0 success, 1 item-not-found, 2 usage.
//
// Diff range: merge-base(origin/main, HEAD) → working tree — captures both
// committed and uncommitted work. This is the "what am I changing in this
// task" view used during delivery and by st close to freeze LOC.
func Files(s *store.Store, cfg *config.Config, itemID string, opts FilesOpts) int {
	res, code := ComputeFileChanges(s, cfg, itemID, opts)
	if code != 0 {
		return code
	}
	if opts.JSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	renderFilesHuman(os.Stdout, res)
	return 0
}

// ComputeFileChanges is the pure-data version callable from st close and
// tests without writing to stdout.
func ComputeFileChanges(s *store.Store, cfg *config.Config, itemID string, opts FilesOpts) (FilesResult, int) {
	if _, ok := s.Get(itemID); !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", itemID)
		return FilesResult{}, 1
	}

	resolve := opts.ResolveRepo
	if resolve == nil {
		resolve = resolveRepoDirForItem
	}
	gitFn := opts.RunGit
	if gitFn == nil {
		gitFn = runGit
	}

	result := FilesResult{ItemID: itemID}
	repos := configuredRepos(cfg)

	for _, repo := range repos {
		dir := resolve(cfg, itemID, repo)
		if !isGitDir(dir) {
			// Repo has no worktree for this item — record zero-diff rollup.
			result.Repos = append(result.Repos, RepoRollup{Repo: repo})
			continue
		}

		mergeBase, err := gitFn(dir, "merge-base", "origin/main", "HEAD")
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s: merge-base failed: %v", repo, err))
			result.Repos = append(result.Repos, RepoRollup{Repo: repo})
			continue
		}
		mergeBase = strings.TrimSpace(mergeBase)

		nameStatusOut, err := gitFn(dir, "diff", "--name-status", mergeBase)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s: name-status failed: %v", repo, err))
			result.Repos = append(result.Repos, RepoRollup{Repo: repo})
			continue
		}
		numstatOut, err := gitFn(dir, "diff", "--numstat", mergeBase)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s: numstat failed: %v", repo, err))
			result.Repos = append(result.Repos, RepoRollup{Repo: repo})
			continue
		}

		entries := parseNameStatus(nameStatusOut)
		mergeNumstat(entries, numstatOut)

		rollup := RepoRollup{Repo: repo, Files: len(entries)}
		for _, e := range entries {
			net := e.LinesAdded - e.LinesDeleted
			result.Files = append(result.Files, FileChange{
				Repo:    repo,
				Path:    e.Path,
				Action:  e.Action,
				Type:    classifyFile(e.Path),
				Added:   e.LinesAdded,
				Removed: e.LinesDeleted,
				Net:     net,
			})
			rollup.Added += e.LinesAdded
			rollup.Removed += e.LinesDeleted
			rollup.Net += net
		}
		result.Repos = append(result.Repos, rollup)
	}

	for _, r := range result.Repos {
		result.Totals.Files += r.Files
		result.Totals.Added += r.Added
		result.Totals.Removed += r.Removed
		result.Totals.Net += r.Net
	}
	result.Totals.Repo = "total"

	// Stable output ordering
	sort.SliceStable(result.Files, func(i, j int) bool {
		if result.Files[i].Repo != result.Files[j].Repo {
			return result.Files[i].Repo < result.Files[j].Repo
		}
		return result.Files[i].Path < result.Files[j].Path
	})
	sort.SliceStable(result.Repos, func(i, j int) bool {
		return result.Repos[i].Repo < result.Repos[j].Repo
	})

	return result, 0
}

// configuredRepos returns the list of repo short names to diff. Prefers the
// explicit Repos list in Worktree config; falls back to keys of RepoMap.
func configuredRepos(cfg *config.Config) []string {
	if cfg.Worktree == nil {
		return nil
	}
	if len(cfg.Worktree.Repos) > 0 {
		out := make([]string, len(cfg.Worktree.Repos))
		copy(out, cfg.Worktree.Repos)
		return out
	}
	if len(cfg.Worktree.RepoMap) > 0 {
		out := make([]string, 0, len(cfg.Worktree.RepoMap))
		for k := range cfg.Worktree.RepoMap {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func renderFilesHuman(w io.Writer, res FilesResult) {
	for _, rollup := range res.Repos {
		filesInRepo := []FileChange{}
		for _, f := range res.Files {
			if f.Repo == rollup.Repo {
				filesInRepo = append(filesInRepo, f)
			}
		}
		if rollup.Files == 0 && len(filesInRepo) == 0 {
			continue // skip zero-change repos in human output; they appear in JSON
		}
		fmt.Fprintf(w, "%s  (%d files, +%d -%d, net %+d)\n",
			rollup.Repo, rollup.Files, rollup.Added, rollup.Removed, rollup.Net)
		for _, f := range filesInRepo {
			fmt.Fprintf(w, "  %-3s %-60s +%-5d -%-5d (%+d) [%s]\n",
				f.Action, f.Path, f.Added, f.Removed, f.Net, f.Type)
		}
	}

	// Print warnings BEFORE the totals / zero-change bail. Warnings explain
	// why repos show zero changes (e.g. every merge-base failed), and must
	// always be visible to the operator.
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}

	if len(res.Repos) == 0 || res.Totals.Files == 0 {
		fmt.Fprintln(w, "No file changes across configured repos.")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Total: %d files across %d repo(s)\n",
		res.Totals.Files, countRepoWithFiles(res.Repos))
	fmt.Fprintf(w, "  +%d added\n", res.Totals.Added)
	fmt.Fprintf(w, "  -%d removed\n", res.Totals.Removed)
	fmt.Fprintf(w, "  %+d net\n", res.Totals.Net)
}

func countRepoWithFiles(repos []RepoRollup) int {
	n := 0
	for _, r := range repos {
		if r.Files > 0 {
			n++
		}
	}
	return n
}
