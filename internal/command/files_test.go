package command

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/testutil"
)

// testFilesEnv extends the standard env with a worktree config that has two
// fabricated repos, plus fake git and resolve functions so the test is
// deterministic and doesn't need real git worktrees.
type testFilesEnv struct {
	*testutil.Env
	opts FilesOpts
}

func newTestFilesEnv(t *testing.T, gitResponses map[string]string) testFilesEnv {
	t.Helper()
	env := testutil.NewEnv(t)

	// Inject a minimal worktree config. Worktree.Enabled gates nothing we use
	// here; the Repos list drives iteration order.
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api", "web", "infra"},
		RepoMap: map[string]string{
			"api":   "theraprac-api",
			"web":   "theraprac-web",
			"infra": "theraprac-infra",
		},
	}

	resolve := func(cfg *config.Config, itemID, repo string) string {
		// Map short name → a fake dir path that our fake runGit will see.
		return filepath.Join("/fake", itemID, repo)
	}
	runG := func(dir string, args ...string) (string, error) {
		key := dir + "|" + strings.Join(args, " ")
		if v, ok := gitResponses[key]; ok {
			if v == "ERR" {
				return "", fmt.Errorf("synthetic error")
			}
			return v, nil
		}
		return "", nil
	}
	return testFilesEnv{Env: env, opts: FilesOpts{ResolveRepo: resolve, RunGit: runG}}
}

func TestFiles_MultiRepoRollup(t *testing.T) {
	// isGitDir uses real filesystem; point our resolve at paths that exist.
	// Simpler: stub isGitDir indirectly by returning a path our fake RunGit can
	// see. Since our resolve returns /fake/<id>/<repo>, and isGitDir checks for
	// .git there, we need to make those dirs exist with .git/ subdirs.
	tmpBase := t.TempDir()
	env := testutil.NewEnv(t)
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api", "web", "infra"},
	}

	makeFake := func(repo string) string {
		dir := filepath.Join(tmpBase, "T-003", repo)
		if err := fakeGitRepo(dir); err != nil {
			t.Fatalf("fakeGitRepo %s: %v", repo, err)
		}
		return dir
	}
	apiDir := makeFake("api")
	webDir := makeFake("web")
	infraDir := makeFake("infra")

	resolve := func(cfg *config.Config, itemID, repo string) string {
		switch repo {
		case "api":
			return apiDir
		case "web":
			return webDir
		case "infra":
			return infraDir
		}
		return ""
	}

	// Build the fake git responses keyed by (dir, args) in a single map.
	gitResponses := map[string]string{
		apiDir + "|merge-base origin/main HEAD":   "abc123\n",
		apiDir + "|diff --name-status abc123":     "M\tinternal/a.go\nA\tinternal/b.go\nD\tinternal/c.go\n",
		apiDir + "|diff --numstat abc123":         "45\t12\tinternal/a.go\n30\t0\tinternal/b.go\n0\t87\tinternal/c.go\n",
		webDir + "|merge-base origin/main HEAD":   "def456\n",
		webDir + "|diff --name-status def456":     "M\tsrc/App.tsx\n",
		webDir + "|diff --numstat def456":         "10\t5\tsrc/App.tsx\n",
		infraDir + "|merge-base origin/main HEAD": "ghi789\n",
		infraDir + "|diff --name-status ghi789":   "",
		infraDir + "|diff --numstat ghi789":       "",
	}

	runG := func(dir string, args ...string) (string, error) {
		key := dir + "|" + strings.Join(args, " ")
		v, ok := gitResponses[key]
		if !ok {
			return "", fmt.Errorf("unexpected git call: %s %v", dir, args)
		}
		return v, nil
	}

	res, code := ComputeFileChanges(env.S, env.Cfg, "T-003",
		FilesOpts{ResolveRepo: resolve, RunGit: runG})
	if code != 0 {
		t.Fatalf("ComputeFileChanges exit=%d", code)
	}

	// 4 files across 2 repos with changes (api + web); infra has zero.
	if res.Totals.Files != 4 {
		t.Errorf("total files = %d, want 4", res.Totals.Files)
	}
	if res.Totals.Added != 85 { // 45+30+0+10
		t.Errorf("total added = %d, want 85", res.Totals.Added)
	}
	if res.Totals.Removed != 104 { // 12+0+87+5
		t.Errorf("total removed = %d, want 104", res.Totals.Removed)
	}
	if res.Totals.Net != -19 {
		t.Errorf("total net = %d, want -19", res.Totals.Net)
	}

	// infra must still appear in Repos rollup with zeros
	var infra *RepoRollup
	for i := range res.Repos {
		if res.Repos[i].Repo == "infra" {
			infra = &res.Repos[i]
			break
		}
	}
	if infra == nil {
		t.Fatal("infra rollup missing")
	}
	if infra.Files != 0 || infra.Added != 0 || infra.Removed != 0 {
		t.Errorf("infra should be zero-change: %+v", infra)
	}
}

func TestFiles_ItemNotFound(t *testing.T) {
	env := newTestFilesEnv(t, nil)
	if _, code := ComputeFileChanges(env.S, env.Cfg, "T-999", env.opts); code != 1 {
		t.Errorf("expected exit 1 for missing item, got %d", code)
	}
}

func TestFiles_ClassifiesFileTypes(t *testing.T) {
	tmpBase := t.TempDir()
	env := testutil.NewEnv(t)
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api"},
	}
	apiDir := filepath.Join(tmpBase, "T-003", "api")
	if err := fakeGitRepo(apiDir); err != nil {
		t.Fatalf("fakeGitRepo: %v", err)
	}

	gitResponses := map[string]string{
		apiDir + "|merge-base origin/main HEAD": "abc\n",
		apiDir + "|diff --name-status abc":      "M\tinternal/foo.go\nA\tinternal/foo_test.go\nM\tdocs/api.md\nM\tMakefile\nM\tdb/changelog/001-init.sql\n",
		apiDir + "|diff --numstat abc":          "10\t0\tinternal/foo.go\n20\t0\tinternal/foo_test.go\n5\t0\tdocs/api.md\n1\t0\tMakefile\n50\t0\tdb/changelog/001-init.sql\n",
	}
	runG := func(dir string, args ...string) (string, error) {
		return gitResponses[dir+"|"+strings.Join(args, " ")], nil
	}
	resolve := func(cfg *config.Config, itemID, repo string) string { return apiDir }

	res, code := ComputeFileChanges(env.S, env.Cfg, "T-003",
		FilesOpts{ResolveRepo: resolve, RunGit: runG})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}

	wantTypes := map[string]string{
		"internal/foo.go":           "app",
		"internal/foo_test.go":      "test",
		"docs/api.md":               "doc",
		"Makefile":                  "config",
		"db/changelog/001-init.sql": "migration",
	}
	for _, f := range res.Files {
		if want, ok := wantTypes[f.Path]; ok {
			if f.Type != want {
				t.Errorf("%s classified as %q, want %q", f.Path, f.Type, want)
			}
		}
	}
}

func TestFiles_WarningsRenderEvenWhenZeroFiles(t *testing.T) {
	// Regression: renderFilesHuman must print warnings BEFORE the
	// "no file changes" bail, so an operator sees why every repo returned 0.
	tmpBase := t.TempDir()
	env := testutil.NewEnv(t)
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api"},
	}
	apiDir := filepath.Join(tmpBase, "T-003", "api")
	if err := fakeGitRepo(apiDir); err != nil {
		t.Fatalf("fakeGitRepo: %v", err)
	}

	runG := func(dir string, args ...string) (string, error) {
		return "", fmt.Errorf("every git call fails in this test")
	}
	resolve := func(cfg *config.Config, itemID, repo string) string { return apiDir }

	res, code := ComputeFileChanges(env.S, env.Cfg, "T-003",
		FilesOpts{ResolveRepo: resolve, RunGit: runG})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	buf := &bytes.Buffer{}
	renderFilesHuman(buf, res)
	out := buf.String()
	if !strings.Contains(out, "warning:") {
		t.Errorf("expected warning to be rendered; got:\n%s", out)
	}
	if !strings.Contains(out, "No file changes") {
		t.Errorf("expected 'No file changes' line; got:\n%s", out)
	}
	// Warning must appear BEFORE the "No file changes" line
	wIdx := strings.Index(out, "warning:")
	nIdx := strings.Index(out, "No file changes")
	if wIdx < 0 || nIdx < 0 || wIdx > nIdx {
		t.Errorf("warning should precede 'No file changes'; got:\n%s", out)
	}
}

func TestFiles_GitErrorsBecomeWarningsNotFailure(t *testing.T) {
	tmpBase := t.TempDir()
	env := testutil.NewEnv(t)
	env.Cfg.Worktree = &config.WorktreeConfig{
		Enabled: true,
		BaseDir: "worktrees",
		Repos:   []string{"api"},
	}
	apiDir := filepath.Join(tmpBase, "T-003", "api")
	if err := fakeGitRepo(apiDir); err != nil {
		t.Fatalf("fakeGitRepo: %v", err)
	}

	runG := func(dir string, args ...string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "merge-base") {
			return "", fmt.Errorf("no merge base")
		}
		return "", nil
	}
	resolve := func(cfg *config.Config, itemID, repo string) string { return apiDir }

	res, code := ComputeFileChanges(env.S, env.Cfg, "T-003",
		FilesOpts{ResolveRepo: resolve, RunGit: runG})
	if code != 0 {
		t.Fatalf("expected exit 0 with warnings, got %d", code)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning on merge-base failure")
	}
	// Repo still appears, just with zero counts
	if len(res.Repos) != 1 || res.Repos[0].Files != 0 {
		t.Errorf("unexpected repos: %+v", res.Repos)
	}
}

// fakeGitRepo creates a directory with a .git marker so isGitDir returns true.
// Files command assumes real git is callable — we stub git via RunGit so we
// only need the directory structure here.
func fakeGitRepo(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		return err
	}
	return nil
}
