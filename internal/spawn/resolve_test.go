package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkExe writes an executable stub file and returns its path.
func mkExe(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestResolveClaudeEnvOverride — ST_CLAUDE_BIN, when it points at a
// usable executable, wins unconditionally over the version scan.
func TestResolveClaudeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	exe := mkExe(t, dir, "claude-override")
	t.Setenv(ClaudeBinEnv, exe)
	t.Setenv(versionsDirEnv, "") // irrelevant — override wins

	got, err := ResolveClaudeBinary()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != exe {
		t.Fatalf("got %q, want override %q", got, exe)
	}
}

// TestResolveClaudeEnvOverrideMissing — a typo'd ST_CLAUDE_BIN must
// fail loudly, NOT silently fall through to the version scan (which
// would pick a different binary than the operator asked for).
func TestResolveClaudeEnvOverrideMissing(t *testing.T) {
	t.Setenv(ClaudeBinEnv, "/no/such/claude")

	_, err := ResolveClaudeBinary()
	if err == nil {
		t.Fatal("expected error for missing ST_CLAUDE_BIN target, got nil")
	}
	if !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("error should explain the override is unusable, got %q", err)
	}
}

// TestResolveClaudeEnvOverrideNotExecutable — a non-executable target
// is rejected (a plain file is not a runnable binary).
func TestResolveClaudeEnvOverrideNotExecutable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notexe")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ClaudeBinEnv, p)

	if _, err := ResolveClaudeBinary(); err == nil {
		t.Fatal("expected error for non-executable override, got nil")
	}
}

// TestResolveClaudeNewestVersion — with no override, the resolver picks
// the highest MAJOR.MINOR.PATCH entry, ordered NUMERICALLY per
// component (2.1.143 > 2.1.99 > 2.1.9 — not lexical). Non-semver
// entries are ignored.
func TestResolveClaudeNewestVersion(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2.1.9", "2.1.99", "2.1.143", "1.9.999"} {
		mkExe(t, dir, n)
	}
	// Decoys the scan must ignore.
	mkExe(t, dir, "latest")
	mkExe(t, dir, "2.1") // wrong arity
	mkExe(t, dir, "2.1.144-rc1")

	t.Setenv(ClaudeBinEnv, "")
	t.Setenv(versionsDirEnv, dir)

	got, err := ResolveClaudeBinary()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(dir, "2.1.143"); got != want {
		t.Fatalf("got %q, want newest %q", got, want)
	}
}

// TestResolveClaudeAbsentErrors — an empty or missing versions dir is a
// hard error so callers spawn NOTHING (contract §13 f2). The message
// must name the directory it looked in.
func TestResolveClaudeAbsentErrors(t *testing.T) {
	t.Setenv(ClaudeBinEnv, "")

	t.Run("empty dir", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(versionsDirEnv, dir)
		_, err := ResolveClaudeBinary()
		if err == nil {
			t.Fatal("expected error for empty versions dir, got nil")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("error should name the dir, got %q", err)
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		t.Setenv(versionsDirEnv, filepath.Join(t.TempDir(), "nope"))
		if _, err := ResolveClaudeBinary(); err == nil {
			t.Fatal("expected error for missing versions dir, got nil")
		}
	})
}

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"2.1.144", [3]int{2, 1, 144}, true},
		{"0.0.0", [3]int{0, 0, 0}, true},
		{"10.20.30", [3]int{10, 20, 30}, true},
		{"2.1", [3]int{}, false},
		{"2.1.1.1", [3]int{}, false},
		{"2.1.x", [3]int{}, false},
		{"2.1.144-rc1", [3]int{}, false},
		{"latest", [3]int{}, false},
		{"", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSemver(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLessVer(t *testing.T) {
	if !lessVer([3]int{2, 1, 9}, [3]int{2, 1, 143}) {
		t.Error("2.1.9 should be < 2.1.143")
	}
	if lessVer([3]int{2, 1, 143}, [3]int{2, 1, 9}) {
		t.Error("2.1.143 should NOT be < 2.1.9")
	}
	if lessVer([3]int{2, 1, 1}, [3]int{2, 1, 1}) {
		t.Error("equal versions are not <")
	}
	if !lessVer([3]int{1, 9, 9}, [3]int{2, 0, 0}) {
		t.Error("1.9.9 should be < 2.0.0")
	}
}
