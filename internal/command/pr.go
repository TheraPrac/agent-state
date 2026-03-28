package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/store"
)

// PROpts holds flags and injectable functions for the pr command.
type PROpts struct {
	Repo     string
	PRNumber int
	// Injectable for testing (nil = use real git)
	GitNameStatus func(repoDir string) (string, error)
	GitNumstat    func(repoDir string) (string, error)
	GitHeadSHA    func(repoDir string) (string, error)
	GitBlobHash   func(repoDir, path string) (string, error)
	FileExists    func(path string) bool
}

// PR records a PR manifest with file analysis, scope suite computation, and test-file gates.
func PR(s *store.Store, cfg *config.Config, id string, opts PROpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Status != "active" {
		fmt.Fprintf(os.Stderr, "%s is %s — must be active to record a PR\n", id, item.Status)
		return 1
	}

	if opts.Repo == "" {
		fmt.Fprintln(os.Stderr, "--repo is required")
		return 2
	}
	if opts.PRNumber == 0 {
		fmt.Fprintln(os.Stderr, "--pr is required")
		return 2
	}

	// Resolve repo directory — prefer worktree if it exists for this item
	repoDir := resolveRepoDirForItem(cfg, id, opts.Repo)

	// Default git functions
	if opts.GitNameStatus == nil {
		opts.GitNameStatus = func(dir string) (string, error) {
			return runGit(dir, "diff", "--name-status", "origin/main...HEAD")
		}
	}
	if opts.GitNumstat == nil {
		opts.GitNumstat = func(dir string) (string, error) {
			return runGit(dir, "diff", "--numstat", "origin/main...HEAD")
		}
	}
	if opts.GitHeadSHA == nil {
		opts.GitHeadSHA = func(dir string) (string, error) {
			return runGit(dir, "rev-parse", "HEAD")
		}
	}
	if opts.GitBlobHash == nil {
		opts.GitBlobHash = func(dir, path string) (string, error) {
			out, err := runGit(dir, "ls-tree", "HEAD", "--", path)
			if err != nil || out == "" {
				return "", err
			}
			// Format: <mode> <type> <hash>\t<path>
			parts := strings.Fields(out)
			if len(parts) >= 3 {
				return parts[2], nil
			}
			return "", nil
		}
	}
	if opts.FileExists == nil {
		opts.FileExists = func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		}
	}

	// Get name-status
	nameStatusOut, err := opts.GitNameStatus(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git diff --name-status: %v\n", err)
		return 1
	}

	// Get numstat
	numstatOut, err := opts.GitNumstat(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git diff --numstat: %v\n", err)
		return 1
	}

	// Get HEAD SHA
	headSHA, err := opts.GitHeadSHA(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git rev-parse HEAD: %v\n", err)
		return 1
	}
	headSHA = strings.TrimSpace(headSHA)

	// Parse name-status into FileRecords
	files := parseNameStatus(nameStatusOut)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no changed files found")
		return 1
	}

	// Merge numstat line counts
	mergeNumstat(files, numstatOut)

	// Classify files and get blob hashes
	var stats manifest.CodeStats
	stats.FilesChanged = len(files)
	for i := range files {
		files[i].Type = classifyFile(files[i].Path)
		if files[i].Action != "D" {
			hash, _ := opts.GitBlobHash(repoDir, files[i].Path)
			files[i].BlobHash = hash
		}
		stats.Insertions += files[i].LinesAdded
		stats.Deletions += files[i].LinesDeleted
	}

	// Test-file-existence warning: note missing test files but don't block.
	// Real enforcement is per-file coverage at st close/st test --coverage.
	var missing []string
	for _, f := range files {
		if f.Type != "app" || f.Action == "D" {
			continue
		}
		testPath := testFileFor(f.Path)
		if testPath == "" {
			continue
		}
		if !opts.FileExists(filepath.Join(repoDir, testPath)) {
			missing = append(missing, fmt.Sprintf("  %s → %s", f.Path, testPath))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d app file(s) missing dedicated test files:\n", len(missing))
		for _, m := range missing {
			fmt.Fprintln(os.Stderr, m)
		}
		fmt.Fprintln(os.Stderr, "  (per-file coverage will be enforced at test time)")
	}

	// Compute scope suites
	scopeSuites := computeScopeSuites(cfg, opts.Repo, files)

	// Build PR record
	record := manifest.PRRecord{
		Repo:        opts.Repo,
		PRNumber:    opts.PRNumber,
		HeadSHA:     headSHA,
		Files:       toManifestFiles(files),
		CodeStats:   stats,
		ScopeSuites: scopeSuites,
		RecordedAt:  time.Now().Format(time.RFC3339),
	}

	// Append to manifest sidecar
	if err := manifest.AppendPR(cfg.ManifestDir(), id, record); err != nil {
		fmt.Fprintf(os.Stderr, "saving manifest: %v\n", err)
		return 1
	}

	// Update item summary
	prSummary := buildPRSummary(cfg.ManifestDir(), id)
	setNestedField(item, "manifest", "prs", prSummary)

	// Mark scope suites as required in testing_evidence
	for _, suite := range scopeSuites {
		current, _ := getNestedField(item, "testing_evidence", suite)
		if current == "" || current == "null" {
			setNestedField(item, "testing_evidence", suite, "required")
		}
	}

	item.Doc.SetField("last_touched", time.Now().Format(time.RFC3339))

	if err := s.Write(item); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, err)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op:       "pr_recorded",
		Field:    "manifest",
		NewValue: fmt.Sprintf("%s#%d", opts.Repo, opts.PRNumber),
	})

	fmt.Printf("Recorded PR %s#%d on %s (%d files, +%d/-%d)\n",
		opts.Repo, opts.PRNumber, id, len(files), stats.Insertions, stats.Deletions)
	if len(scopeSuites) > 0 {
		fmt.Printf("  scope suites: %s\n", strings.Join(scopeSuites, ", "))
	}
	return 0
}

// --- Helpers ---

func resolveRepoDir(cfg *config.Config, repo string) string {
	if cfg.Worktree != nil && cfg.Worktree.ParentDir != "" {
		parentDir := cfg.Worktree.ParentDir
		if !filepath.IsAbs(parentDir) {
			parentDir = filepath.Join(cfg.Root(), parentDir)
		}
		if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
			return filepath.Join(parentDir, mapped)
		}
		return filepath.Join(parentDir, repo)
	}
	return repo
}

// resolveRepoDirForItem checks for a worktree first, falls back to main repo.
func resolveRepoDirForItem(cfg *config.Config, itemID, repo string) string {
	if cfg.Worktree != nil && cfg.Worktree.BaseDir != "" {
		wtRoot := filepath.Join(cfg.Root(), cfg.Worktree.BaseDir)

		// Pattern 1: <base_dir>/<item-id>/<repo> (st start pattern)
		wtBase := filepath.Join(wtRoot, itemID)
		for _, name := range []string{repo} {
			candidate := filepath.Join(wtBase, name)
			if isGitDir(candidate) {
				return candidate
			}
		}
		if cfg.Worktree.RepoMap != nil {
			if mapped, ok := cfg.Worktree.RepoMap[repo]; ok {
				candidate := filepath.Join(wtBase, mapped)
				if isGitDir(candidate) {
					return candidate
				}
			}
		}

		// Pattern 2: <base_dir>/<repo> (manual/legacy worktree)
		candidate := filepath.Join(wtRoot, repo)
		if isGitDir(candidate) {
			return candidate
		}

		// Pattern 3: scan all worktree dirs for a repo matching the name
		entries, err := os.ReadDir(wtRoot)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				// Check direct match
				if strings.Contains(e.Name(), repo) {
					candidate := filepath.Join(wtRoot, e.Name())
					if isGitDir(candidate) {
						return candidate
					}
				}
				// Check subdirs (st start creates <id>/<repo>)
				subEntries, err := os.ReadDir(filepath.Join(wtRoot, e.Name()))
				if err == nil {
					for _, sub := range subEntries {
						if sub.IsDir() && strings.Contains(sub.Name(), repo) {
							candidate := filepath.Join(wtRoot, e.Name(), sub.Name())
							if isGitDir(candidate) {
								return candidate
							}
						}
					}
				}
			}
		}
	}
	return resolveRepoDir(cfg, repo)
}

func isGitDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// fileEntry is an intermediate struct during PR analysis.
type fileEntry struct {
	Path         string
	Action       string
	Type         string
	BlobHash     string
	LinesAdded   int
	LinesDeleted int
}

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
				path = parts[2] // use the new path
			}
		}
		files = append(files, fileEntry{Path: path, Action: action})
	}
	return files
}

func mergeNumstat(files []fileEntry, output string) {
	stats := map[string][2]int{} // path → [added, deleted]
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

func testFileFor(path string) string {
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)

	// Skip files that don't need tests
	if strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return ""
	}
	// Generated Go files
	if strings.HasSuffix(base, "_gen.go") || strings.HasSuffix(base, ".gen.go") {
		return ""
	}
	// TypeScript declarations and barrel files
	if strings.HasSuffix(base, ".d.ts") || base == "index.ts" || base == "index.tsx" {
		return ""
	}
	// Type-only files
	if base == "types.ts" || base == "types.tsx" {
		return ""
	}

	// Go: foo.go → foo_test.go
	if ext == ".go" {
		name := strings.TrimSuffix(base, ".go")
		return filepath.Join(dir, name+"_test.go")
	}

	// TypeScript/TSX: Foo.tsx → Foo.test.tsx
	if ext == ".ts" || ext == ".tsx" {
		name := strings.TrimSuffix(base, ext)
		return filepath.Join(dir, name+".test"+ext)
	}

	return "" // no convention
}

func computeScopeSuites(cfg *config.Config, repo string, files []fileEntry) []string {
	if cfg.Testing == nil {
		return nil
	}

	var suites []string
	for name, suite := range cfg.Testing.ScopeSuites {
		// If triggers are configured, match against file paths
		if len(suite.Triggers) > 0 {
			if matchesTriggers(files, suite.Triggers) {
				suites = append(suites, name)
			}
			continue
		}
		// Convention: suite prefix matches repo name (api_integration → api repo)
		if strings.HasPrefix(name, repo+"_") {
			suites = append(suites, name)
		}
	}
	return suites
}

func matchesTriggers(files []fileEntry, triggers []string) bool {
	for _, f := range files {
		for _, pattern := range triggers {
			matched, _ := filepath.Match(pattern, f.Path)
			if matched {
				return true
			}
			// Also try matching with ** wildcard (simplified: check prefix)
			if strings.Contains(pattern, "**") {
				prefix := strings.Split(pattern, "**")[0]
				if strings.HasPrefix(f.Path, prefix) {
					return true
				}
			}
		}
	}
	return false
}

func toManifestFiles(files []fileEntry) []manifest.FileRecord {
	result := make([]manifest.FileRecord, len(files))
	for i, f := range files {
		result[i] = manifest.FileRecord{
			Path:         f.Path,
			Action:       f.Action,
			Type:         f.Type,
			BlobHash:     f.BlobHash,
			LinesAdded:   f.LinesAdded,
			LinesDeleted: f.LinesDeleted,
		}
	}
	return result
}

func buildPRSummary(manifestDir, id string) string {
	m, err := manifest.Load(manifestDir, id)
	if err != nil || len(m.PRs) == 0 {
		return ""
	}
	var parts []string
	for _, pr := range m.PRs {
		parts = append(parts, fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber))
	}
	return strings.Join(parts, ", ")
}

