package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/theraprac/agent-state/internal/config"
)

// fileEntry represents one file in a git diff analysis.
type fileEntry struct {
	Path         string
	Action       string
	Type         string
	BlobHash     string
	LinesAdded   int
	LinesDeleted int
}

// resolveRepoDir returns the directory for a repo, respecting worktree.parent_dir config.
// I-778: parent resolution routed through cfg.RepoParent() so an ST_ROOT-leaked
// cfg.Root() can't redirect this to a peer agent's clone, while preserving the
// pre-PR "no worktree.parent_dir → bare repo (CWD-relative)" semantic.
func resolveRepoDir(cfg *config.Config, repo string) string {
	if cfg.Worktree == nil || cfg.Worktree.ParentDir == "" {
		return repo
	}
	parentDir := cfg.RepoParent()
	if parentDir == "" {
		return repo
	}
	if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
		return filepath.Join(parentDir, mapped)
	}
	return filepath.Join(parentDir, repo)
}

// resolveItemWorktree returns the item's OWN worktree path for (itemID, repo)
// using the I-407 Pattern 1/2/3 lookup, or "" when no item worktree exists on
// disk. It does NOT fall back to the main repo — callers wanting that fallback
// use resolveRepoDirForItem. Shared by resolveRepoDirForItem and the strict
// close-time resolver (itemWorktreeRepoDir) so the worktree-location logic lives
// in one place (I-1477).
func resolveItemWorktree(cfg *config.Config, itemID, repo string) string {
	if cfg.Worktree == nil || cfg.Worktree.BaseDir == "" {
		return ""
	}
	// Pattern 1: <base_dir>/<item-id>/<repo> (st start pattern), with RepoMap.
	if wtBase := cfg.WorktreeForItem(itemID); wtBase != "" {
		if candidate := filepath.Join(wtBase, repo); isGitDir(candidate) {
			return candidate
		}
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				if candidate := filepath.Join(wtBase, mapped); isGitDir(candidate) {
					return candidate
				}
			}
		}
	}

	// Patterns 2 & 3 scan the worktree base dir — check both the new
	// (agent-root) and legacy (workspace) bases since manual/legacy
	// worktrees may live in either location during the I-407 migration window.
	for _, wtRoot := range []string{cfg.WorktreeBase(), cfg.WorktreeBaseLegacy()} {
		if wtRoot == "" {
			continue
		}
		// Pattern 2: <base_dir>/<repo> (manual/legacy worktree)
		if candidate := filepath.Join(wtRoot, repo); isGitDir(candidate) {
			return candidate
		}
		// Pattern 3: scan all worktree dirs for a repo matching the name
		entries, err := os.ReadDir(wtRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if strings.Contains(e.Name(), repo) {
				if candidate := filepath.Join(wtRoot, e.Name()); isGitDir(candidate) {
					return candidate
				}
			}
			subEntries, err := os.ReadDir(filepath.Join(wtRoot, e.Name()))
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if sub.IsDir() && strings.Contains(sub.Name(), repo) {
					if candidate := filepath.Join(wtRoot, e.Name(), sub.Name()); isGitDir(candidate) {
						return candidate
					}
				}
			}
		}
	}
	return ""
}

// resolveRepoDirForItem checks for the item's worktree first, falls back to the
// main repo. I-407: WorktreeForItem prefers the new agent-root location, falls
// back to the legacy shared-workspace location for old worktrees.
func resolveRepoDirForItem(cfg *config.Config, itemID, repo string) string {
	if cfg.Worktree != nil && cfg.Worktree.BaseDir != "" {
		// Legacy quirk preserved: a configured base with no per-item worktree
		// dir resolves to the bare repo name (callers depend on this).
		if cfg.WorktreeForItem(itemID) == "" {
			return repo
		}
		if wt := resolveItemWorktree(cfg, itemID, repo); wt != "" {
			return wt
		}
	}
	return resolveRepoDir(cfg, repo)
}

// itemWorktreeRepoDir resolves where close-time Tier-2 revalidation should look
// for the item's diff (I-1477(e)). When worktree integration is ENABLED it
// returns the item's OWN worktree (Patterns 1-3) and "" when that worktree is
// gone — a merged item, where the pre-push gate already enforced Tier 2 and
// diffing the main repo would pick up unrelated divergence (the phantom-diff bug
// that falsely flagged api_integration on a hooks-only item). When worktree
// integration is DISABLED the agent works on a feature branch in the main clone,
// so the item's changes live there and we diff the main repo directly.
func itemWorktreeRepoDir(cfg *config.Config, itemID, repo string) string {
	if cfg.Worktree == nil || !cfg.Worktree.Enabled {
		return resolveRepoDir(cfg, repo)
	}
	return resolveItemWorktree(cfg, itemID, repo)
}

// isGitDir returns true if the path contains a .git directory or file.
func isGitDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// runGit invokes git in dir with args and returns stdout.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// parseNameStatus parses `git diff --name-status` output into fileEntry records.
func parseNameStatus(output string) []fileEntry {
	var files []fileEntry
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		action := parts[0]
		path := parts[1]
		// Handle renames: R100\told\tnew
		if strings.HasPrefix(action, "R") {
			action = "R"
			if len(parts) >= 3 {
				path = parts[2]
			}
		}
		files = append(files, fileEntry{Path: path, Action: action})
	}
	return files
}

// mergeNumstat parses `git diff --numstat` output and populates LinesAdded/LinesDeleted on files.
func mergeNumstat(files []fileEntry, output string) {
	stats := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		path := parts[2]
		stats[path] = [2]int{added, deleted}
	}
	for i := range files {
		if s, ok := stats[files[i].Path]; ok {
			files[i].LinesAdded = s[0]
			files[i].LinesDeleted = s[1]
		}
	}
}

// classifyFile returns a file category: test, migration, spec, doc, config, or app.
func classifyFile(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(path)
	dir := filepath.Dir(path)

	// Test files
	if strings.HasSuffix(base, "_test.go") {
		return "test"
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return "test"
	}
	if strings.Contains(dir, "__tests__") {
		return "test"
	}

	// Migrations
	if strings.Contains(dir, "db/changelog") || strings.Contains(dir, "migrations") {
		return "migration"
	}

	// OpenAPI spec
	if strings.Contains(path, "openapi") && (ext == ".yaml" || ext == ".yml" || ext == ".json") {
		return "spec"
	}

	// Documentation
	if ext == ".md" {
		return "doc"
	}
	if strings.HasPrefix(path, "docs/") || strings.Contains(path, "/docs/") {
		return "doc"
	}

	// Config files
	switch base {
	case "Makefile", "Dockerfile", ".gitignore", ".eslintrc.js", ".eslintrc.json",
		"tsconfig.json", "package.json", "package-lock.json", "go.mod", "go.sum",
		"docker-compose.yml", "docker-compose.yaml":
		return "config"
	}
	if strings.HasPrefix(base, "docker-compose") {
		return "config"
	}
	if strings.HasPrefix(base, "Dockerfile") {
		return "config"
	}
	// Root-level yaml/toml are config
	if (ext == ".yaml" || ext == ".yml" || ext == ".toml") && !strings.Contains(dir, "/") {
		return "config"
	}

	return "app"
}
