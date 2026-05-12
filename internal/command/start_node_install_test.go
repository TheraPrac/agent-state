package command

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withFakeInstaller swaps nodeInstaller for the duration of t, and
// restores the production implementation when t finishes.
func withFakeInstaller(t *testing.T, fake installerFunc) {
	t.Helper()
	orig := nodeInstaller
	nodeInstaller = fake
	t.Cleanup(func() { nodeInstaller = orig })
}

func TestMaybeInstallNodeDeps_NoPackageJSONIsNoOp(t *testing.T) {
	dir := t.TempDir() // empty — no package.json

	called := false
	withFakeInstaller(t, func(p string) error {
		called = true
		return nil
	})

	maybeInstallNodeDeps(dir)

	if called {
		t.Errorf("installer should not run when package.json is absent")
	}
}

func TestMaybeInstallNodeDeps_NodeModulesAlreadyPresentIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}

	called := false
	withFakeInstaller(t, func(p string) error {
		called = true
		return nil
	})

	maybeInstallNodeDeps(dir)

	if called {
		t.Errorf("installer should not run when node_modules already exists")
	}
}

func TestMaybeInstallNodeDeps_RunsInstallerWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT create node_modules.

	var gotPath string
	withFakeInstaller(t, func(p string) error {
		gotPath = p
		return nil
	})

	maybeInstallNodeDeps(dir)

	if gotPath != dir {
		t.Errorf("installer called with %q, want %q", gotPath, dir)
	}
}

func TestMaybeInstallNodeDeps_InstallerFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	withFakeInstaller(t, func(p string) error {
		return errors.New("simulated npm ci failure")
	})

	// Should not panic / not return anything — just print a hint and
	// hand the operator a partially-set-up worktree.
	maybeInstallNodeDeps(dir)
}
