package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// I-736 integration tests: confirm the CLAUDE.md drift sentinel is
// wired into Check, surfaces as warnings (no `issues++`), respects the
// `quiet` flag, and silently skips when the file is absent.

func setupClaudeMdEnv(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"tasks", "issues", "archive", ".as", "claude-config"} {
		os.MkdirAll(filepath.Join(root, dir), 0755)
	}
	os.WriteFile(filepath.Join(root, ".as", "config.yaml"), []byte("paths:\n  root: .\n"), 0644)
	os.WriteFile(filepath.Join(root, "index.md"), []byte("# index\n"), 0644)

	// Force cfg to use this tempdir root via Load().
	cfg, err := config.Load(filepath.Join(root, ".as", "config.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	return s, cfg, root
}

// stubGitForCheck replaces execGit / execGitNoOutput with no-ops so
// checkGitStatus doesn't muddy stdout with real git output.
func stubGitForCheck(t *testing.T) {
	t.Helper()
	origExecGit := execGit
	origExecGitNoOutput := execGitNoOutput
	t.Cleanup(func() {
		execGit = origExecGit
		execGitNoOutput = origExecGitNoOutput
	})
	execGit = func(dir string, args ...string) ([]byte, error) {
		return []byte(""), nil
	}
	execGitNoOutput = func(dir string, args ...string) error { return nil }
}

func TestCheckClaudeMdDriftWarns(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	claudeMd := filepath.Join(root, "claude-config", "CLAUDE.md")
	content := "Some content.\nOperator 2026-05-20: stop doing that thing\nMore content.\n"
	if err := os.WriteFile(claudeMd, []byte(content), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	var rc int
	out := captureStdout(t, func() {
		rc = Check(s, cfg, false, false)
	})

	if !strings.Contains(out, "claude-md drift") {
		t.Errorf("expected 'claude-md drift' in stdout, got: %s", out)
	}
	if !strings.Contains(out, "operator-quote") {
		t.Errorf("expected 'operator-quote' pattern label in stdout, got: %s", out)
	}
	if rc != 0 {
		t.Errorf("CLAUDE.md drift should be warnings only, got rc=%d", rc)
	}
}

func TestCheckClaudeMdCleanSilent(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	claudeMd := filepath.Join(root, "claude-config", "CLAUDE.md")
	content := "# Plain content\n\nNo bad patterns here.\nJust some prose.\n"
	if err := os.WriteFile(claudeMd, []byte(content), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if strings.Contains(out, "claude-md drift") {
		t.Errorf("clean CLAUDE.md should produce no drift warnings, got: %s", out)
	}
}

func TestCheckClaudeMdMissingFileSkipped(t *testing.T) {
	s, cfg, _ := setupClaudeMdEnv(t)
	stubGitForCheck(t)
	// No CLAUDE.md written.

	out := captureStdout(t, func() {
		_ = Check(s, cfg, false, false)
	})

	if strings.Contains(out, "claude-md drift") {
		t.Errorf("missing CLAUDE.md should be silently skipped, got: %s", out)
	}
}

func TestCheckClaudeMdQuietSuppressed(t *testing.T) {
	s, cfg, root := setupClaudeMdEnv(t)
	stubGitForCheck(t)

	claudeMd := filepath.Join(root, "claude-config", "CLAUDE.md")
	content := "Operator 2026-05-20: drift\n"
	if err := os.WriteFile(claudeMd, []byte(content), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	out := captureStdout(t, func() {
		_ = Check(s, cfg, true, false) // quiet=true
	})

	if strings.Contains(out, "claude-md drift") {
		t.Errorf("quiet mode should suppress drift warnings, got: %s", out)
	}
}
