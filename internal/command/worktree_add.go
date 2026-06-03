package command

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// WorktreeAdd provisions a new repo worktree onto an already-active item's
// branch, env-wires it, and updates .workinfo so st finish cleans it up.
func WorktreeAdd(s *store.Store, cfg *config.Config, id, repo string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}
	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — worktree add requires an active item\n", id, item.Status)
		return 1
	}
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		fmt.Fprintln(os.Stderr, "worktree integration not enabled in config")
		return 1
	}

	baseDir := cfg.WorktreeBase()
	workDir := filepath.Join(baseDir, id)
	parentDir := cfg.RepoParent()

	wi, err := readWorkinfo(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading .workinfo for %s: %v\n", id, err)
		return 1
	}

	// Idempotency: skip if already registered.
	for _, r := range wi.Repos {
		if r == repo {
			fmt.Printf("%s: %s worktree already registered\n", id, repo)
			return 0
		}
	}

	if err := provisionSingleRepoWorktree(cfg, workDir, wi.Branch, repo, parentDir); err != nil {
		fmt.Fprintf(os.Stderr, "provisioning worktree: %v\n", err)
		return 1
	}

	// Update .workinfo to include the new repo so st finish removes it.
	wi.Repos = append(wi.Repos, repo)
	writeWorkinfo(workDir, wi.ID, wi.Branch, wi.Repos)

	fmt.Printf("Added %s worktree to %s (branch: %s)\n", repo, id, wi.Branch)
	return 0
}

// workinfoData holds the parsed fields of a .workinfo file.
type workinfoData struct {
	ID     string
	Branch string
	Repos  []string
}

// readWorkinfo parses the .workinfo YAML-ish file written by writeWorkinfo.
func readWorkinfo(workDir string) (workinfoData, error) {
	path := filepath.Join(workDir, ".workinfo")
	f, err := os.Open(path)
	if err != nil {
		return workinfoData{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var wi workinfoData
	inRepos := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "name:") {
			wi.ID = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			inRepos = false
			continue
		}
		if strings.HasPrefix(trimmed, "branch:") {
			wi.Branch = strings.TrimSpace(strings.TrimPrefix(trimmed, "branch:"))
			inRepos = false
			continue
		}
		if trimmed == "repos:" {
			inRepos = true
			continue
		}
		if inRepos && strings.HasPrefix(trimmed, "- ") {
			wi.Repos = append(wi.Repos, strings.TrimPrefix(trimmed, "- "))
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "-") {
			inRepos = false
		}
	}
	if err := scanner.Err(); err != nil {
		return workinfoData{}, err
	}
	if wi.Branch == "" {
		return workinfoData{}, fmt.Errorf("branch not found in .workinfo")
	}
	return wi, nil
}
