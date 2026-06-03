package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/manifest"
	"github.com/jfinlinson/agent-state/internal/model"
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

	// E2E spec check: pages and key components should have E2E coverage
	var missingE2E []string
	for _, f := range files {
		if f.Action == "D" {
			continue
		}
		specPath := e2eSpecFor(f.Path)
		if specPath == "" {
			continue
		}
		if !opts.FileExists(filepath.Join(repoDir, specPath)) {
			missingE2E = append(missingE2E, fmt.Sprintf("  %s → %s", f.Path, specPath))
		}
	}
	if len(missingE2E) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d UI file(s) missing E2E specs:\n", len(missingE2E))
		for _, m := range missingE2E {
			fmt.Fprintln(os.Stderr, m)
		}
		fmt.Fprintln(os.Stderr, "  (E2E tests should cover user-facing pages and workflows)")
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

	// Precompute values needed in the Mutate closure (external calls already done above).
	prSummary := buildPRSummary(cfg.ManifestDir(), id)

	var fileSummary []string
	for _, f := range files {
		net := f.LinesAdded - f.LinesDeleted
		fileSummary = append(fileSummary, fmt.Sprintf("%s %s +%d/-%d (%+d) [%s]",
			f.Action, f.Path, f.LinesAdded, f.LinesDeleted, net, f.Type))
	}

	prRef := fmt.Sprintf("%s#%d", opts.Repo, opts.PRNumber)

	var testFiles []string
	for _, f := range files {
		if f.Type == "test" && f.Action != "D" {
			testFiles = append(testFiles, f.Path)
		}
	}
	for _, f := range files {
		if f.Type == "app" && f.Action != "D" {
			if tf := testFileFor(f.Path); tf != "" {
				if opts.FileExists(filepath.Join(repoDir, tf)) {
					testFiles = append(testFiles, tf)
				}
			}
		}
	}
	// Deduplicate test files
	seenTests := make(map[string]bool)
	var dedupedTestFiles []string
	for _, tf := range testFiles {
		if !seenTests[tf] {
			seenTests[tf] = true
			dedupedTestFiles = append(dedupedTestFiles, tf)
		}
	}

	var docFiles []string
	for _, f := range files {
		if f.Type == "doc" && f.Action != "D" {
			docFiles = append(docFiles, f.Path)
		}
	}

	var e2eSpecs []string
	for _, f := range files {
		if f.Action == "D" {
			continue
		}
		if spec := e2eSpecFor(f.Path); spec != "" {
			if opts.FileExists(filepath.Join(repoDir, spec)) {
				if !seenTests[spec] {
					seenTests[spec] = true
					e2eSpecs = append(e2eSpecs, spec)
				}
			}
		}
	}

	// Capture loop variables for closure.
	capturedPRSummary := prSummary
	capturedStats := stats
	capturedFileSummary := fileSummary
	capturedHeadSHA := headSHA
	capturedPRRef := prRef
	capturedDedupedTestFiles := dedupedTestFiles
	capturedDocFiles := docFiles
	capturedE2ESpecs := e2eSpecs
	capturedScopeSuites := scopeSuites

	if err := s.Mutate(id, func(item *model.Item) error {
		// Update item summary
		item.SetNested("manifest", "prs", capturedPRSummary)

		// Surface code stats on the item
		item.SetNested("manifest", "files_changed", fmt.Sprintf("%d", capturedStats.FilesChanged))
		item.SetNested("manifest", "insertions", fmt.Sprintf("%d", capturedStats.Insertions))
		item.SetNested("manifest", "deletions", fmt.Sprintf("%d", capturedStats.Deletions))
		item.SetNested("manifest", "net_lines", fmt.Sprintf("%+d", capturedStats.Insertions-capturedStats.Deletions))

		// Per-file change summary
		item.SetNested("manifest", "file_details", strings.Join(capturedFileSummary, "\n"))

		// Record head SHA
		item.SetNested("manifest", "head_sha", capturedHeadSHA)

		// Record PR in work_tracking
		item.Doc.AppendToNestedList("work_tracking", "pr", capturedPRRef)

		// I-447: opening a PR advances the lifecycle past `pushed`.
		// Only advance forward — never regress if the item is already
		// at merged/closed via a separate path.
		advanceDeliveryStage(item, "pr_open")

		// Record test files written
		for _, tf := range capturedDedupedTestFiles {
			item.Doc.AppendToNestedList("testing_evidence", "tests_written", tf)
		}

		// Record doc changes
		for _, df := range capturedDocFiles {
			item.Doc.AppendToNestedList("testing_evidence", "doc_changes", df)
		}

		// Record E2E spec coverage
		for _, spec := range capturedE2ESpecs {
			item.Doc.AppendToNestedList("testing_evidence", "tests_written", spec)
		}

		// Mark scope suites as required in testing_evidence
		for _, suite := range capturedScopeSuites {
			current, _ := getNestedField(item, "testing_evidence", suite)
			if current == "" || current == "null" {
				item.SetNested("testing_evidence", suite, "required")
			}
		}

		return nil
	}); err != nil {
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
	if err := autoSync(s, fmt.Sprintf("st pr: %s %s#%d", id, opts.Repo, opts.PRNumber)); err != nil {
		return 1
	}
	return 0
}

// --- Helpers ---

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

// e2eSpecFor returns the expected E2E spec path for a page or key component file.
// Returns "" if the file doesn't need E2E coverage.
func e2eSpecFor(path string) string {
	// Only check Next.js page files: src/app/(app)/app/{feature}/page.tsx
	// Convention: page.tsx → tests/e2e/{feature}.spec.ts
	if !strings.Contains(path, "/page.tsx") && !strings.Contains(path, "/page.ts") {
		return ""
	}
	if !strings.Contains(path, "src/app/") {
		return ""
	}
	// Skip layout files, error files, loading files
	base := filepath.Base(path)
	if base != "page.tsx" && base != "page.ts" {
		return ""
	}

	// Extract feature name from path
	// src/app/(app)/app/notes/page.tsx → notes
	// src/app/(app)/app/clients/[clientId]/page.tsx → clients
	// src/app/(app)/app/clinical/sessions/[sessionId]/page.tsx → clinical-sessions
	dir := filepath.Dir(path)
	parts := strings.Split(dir, "/")

	// Find the segment(s) after "app" that aren't route groups or params
	var featureParts []string
	inApp := false
	for _, p := range parts {
		if p == "app" && !inApp {
			inApp = true
			continue
		}
		if !inApp {
			continue
		}
		// Skip route groups like (app), (marketing)
		if strings.HasPrefix(p, "(") && strings.HasSuffix(p, ")") {
			continue
		}
		// Skip another "app" (nested)
		if p == "app" {
			continue
		}
		// Skip dynamic segments like [clientId]
		if strings.HasPrefix(p, "[") {
			continue
		}
		featureParts = append(featureParts, p)
	}

	if len(featureParts) == 0 {
		return ""
	}

	feature := strings.Join(featureParts, "-")
	return filepath.Join("tests", "e2e", feature+".spec.ts")
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
