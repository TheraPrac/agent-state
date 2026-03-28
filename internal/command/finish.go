package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// FinishOpts holds flags for the finish command.
type FinishOpts struct {
	DryRun  bool
	Force   bool
	ListAll bool
}

func Finish(s *store.Store, cfg *config.Config, id string, opts FinishOpts) int {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		fmt.Fprintln(os.Stderr, "worktree integration not enabled in config")
		return 1
	}

	baseDir := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir)

	if opts.ListAll {
		return listWorktrees(baseDir)
	}

	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: as finish <id> [--dry-run] [--force] [--list]")
		return 2
	}

	wtDir := filepath.Join(baseDir, id)

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "worktree directory not found: %s\n", wtDir)
		return 1
	}

	// For each repo in the worktree
	for _, repo := range cfg.Worktree.Repos {
		repoDir := filepath.Join(wtDir, repo)
		if _, err := os.Stat(repoDir); os.IsNotExist(err) {
			continue
		}

		// Check for uncommitted changes
		if !opts.Force {
			out, err := gitOutputDir(repoDir, "status", "--porcelain")
			if err == nil && strings.TrimSpace(out) != "" {
				fmt.Fprintf(os.Stderr, "%s has uncommitted changes in %s\n", id, repo)
				fmt.Fprintln(os.Stderr, "use --force to remove anyway")
				return 1
			}

			// Check for unpushed commits
			out, err = gitOutputDir(repoDir, "log", "--oneline", "@{u}..HEAD")
			if err == nil && strings.TrimSpace(out) != "" {
				fmt.Fprintf(os.Stderr, "%s has unpushed commits in %s\n", id, repo)
				fmt.Fprintln(os.Stderr, "use --force to remove anyway")
				return 1
			}
		}

		if opts.DryRun {
			fmt.Printf("[dry-run] would remove worktree: %s\n", repoDir)
			continue
		}

		// Get the branch name before removing
		branch, _ := gitOutputDir(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		branch = strings.TrimSpace(branch)

		// Remove the worktree — resolve main repo via config
		mainRepoDir := resolveRepoDir(cfg, repo)
		if err := gitCmdDir(mainRepoDir, "worktree", "remove", repoDir); err != nil {
			fmt.Fprintf(os.Stderr, "removing worktree %s: %v\n", repoDir, err)
			// Try force remove
			if opts.Force {
				gitCmdDir(mainRepoDir, "worktree", "remove", "--force", repoDir)
			}
		}

		// Delete local branch (use main repo, not worktree)
		if branch != "" && branch != "main" && branch != "master" {
			gitCmdDir(mainRepoDir, "branch", "-d", branch)
		}

		// Force remove if worktree dir still exists
		if _, err := os.Stat(repoDir); err == nil {
			gitCmdDir(mainRepoDir, "worktree", "remove", "--force", repoDir)
		}

		fmt.Printf("Removed worktree: %s/%s (branch: %s)\n", id, repo, branch)
	}

	if !opts.DryRun {
		// Remove the worktree directory
		os.RemoveAll(wtDir)
		fmt.Printf("Finished %s — worktrees cleaned up\n", id)
	}

	return 0
}

func listWorktrees(baseDir string) int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No worktrees.")
			return 0
		}
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", baseDir, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println("No worktrees.")
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// List repos in this worktree
		subEntries, _ := os.ReadDir(filepath.Join(baseDir, name))
		var repos []string
		for _, sub := range subEntries {
			if sub.IsDir() && !strings.HasPrefix(sub.Name(), ".") {
				repos = append(repos, sub.Name())
			}
		}
		fmt.Printf("%-12s  repos: %s\n", name, strings.Join(repos, ", "))
	}

	return 0
}

func gitCmdDir(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitOutputDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
