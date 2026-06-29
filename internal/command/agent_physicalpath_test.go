package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhysicalPathCanonicalizesCase(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Target")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Symlink resolution is unconditional on all platforms.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	got := physicalPath(link)
	if got == link {
		t.Errorf("physicalPath(%q) = %q — symlink not resolved", link, got)
	}
	// Compare against the OS-resolved form of real, since the tmp dir itself
	// may be a symlink on macOS (/var → /private/var).
	wantResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks(real): %v", err)
	}
	if !strings.EqualFold(got, wantResolved) {
		t.Errorf("physicalPath(%q) = %q, want path equivalent to %q", link, got, wantResolved)
	}

	// Non-existent path falls back to Abs+Clean without error.
	missing := filepath.Join(dir, "does-not-exist")
	fallback := physicalPath(missing)
	if fallback == "" {
		t.Error("physicalPath of non-existent path returned empty string")
	}
	if !filepath.IsAbs(fallback) {
		t.Errorf("physicalPath(%q) fallback %q is not absolute", missing, fallback)
	}
}

func TestBuildAgentWorkspacePlanWritesPhysicalPath(t *testing.T) {
	// Build:  <base>/real/theraprac-agents   (real dir)
	//         <base>/link/theraprac-agents   (symlink → real/theraprac-agents)
	// Set THERAPRAC_AGENTS_ROOT to the symlink path (still ends in "theraprac-agents"
	// so the basename guard passes). resolveAgentsRoot must return the physical path.
	base := t.TempDir()
	realDir := filepath.Join(base, "real", "theraprac-agents")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.MkdirAll(linkParent, 0o755); err != nil {
		t.Fatalf("mkdir link parent: %v", err)
	}
	linkPath := filepath.Join(linkParent, "theraprac-agents")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	t.Setenv("THERAPRAC_AGENTS_ROOT", linkPath)
	got, err := resolveAgentsRoot(nil)
	if err != nil {
		t.Fatalf("resolveAgentsRoot: %v", err)
	}
	if got == linkPath {
		t.Errorf("resolveAgentsRoot returned symlink path %q — expected physical path", linkPath)
	}
	want, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("resolveAgentsRoot = %q, want %q (resolved symlink)", got, want)
	}
}
