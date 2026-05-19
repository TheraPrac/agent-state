// Package spawn launches a budget-capped, JSONL-observable reasoning
// worker (`claude -p`) on an agent-state item — the Shape-3 §10/§13
// linchpin. This file resolves the reasoning binary.
//
// LOAD-BEARING (contract §13 finding 2): the PATH `claude` is the cmux
// shim, which HANGS when invoked nested. A spawned worker MUST invoke
// the resolved binary at ~/.local/share/claude/versions/<version>,
// never PATH `claude`. The resolver here is the single chokepoint that
// guarantees that invariant.
package spawn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ClaudeBinEnv overrides binary resolution entirely. Set by tests (the
// real versions dir is machine-state we don't want unit tests touching)
// and available as an operator escape hatch. When set and the target is
// a usable executable, it wins unconditionally.
const ClaudeBinEnv = "ST_CLAUDE_BIN"

// versionsDirEnv overrides the directory scanned for version-named
// binaries. Tests point this at a temp dir to exercise the
// newest-version pick deterministically without ST_CLAUDE_BIN. Not a
// documented operator knob — ST_CLAUDE_BIN is the supported override.
const versionsDirEnv = "ST_CLAUDE_VERSIONS_DIR"

// ResolveClaudeBinary returns an absolute path to the reasoning binary
// that `st spawn` must exec. Resolution order:
//
//  1. $ST_CLAUDE_BIN — if set, it MUST point at an existing regular
//     executable file; otherwise a clear error (no silent fallthrough
//     to the version scan, so a typo'd override fails loudly rather
//     than silently picking a different binary).
//  2. The newest semver-named entry in ~/.local/share/claude/versions/
//     (e.g. .../versions/2.1.144). Each entry is the ~200 MB binary
//     itself, not a directory.
//
// Every failure path returns a non-nil error and an empty string so
// callers spawn NOTHING when the binary cannot be resolved (§13 f2).
func ResolveClaudeBinary() (string, error) {
	if override := strings.TrimSpace(os.Getenv(ClaudeBinEnv)); override != "" {
		if err := checkExecutable(override); err != nil {
			return "", fmt.Errorf("%s=%q is not usable: %w", ClaudeBinEnv, override, err)
		}
		// The contract is an ABSOLUTE path: the caller exec's this with
		// cwd set to the item worktree, so a relative override would
		// resolve against the worktree (wrong binary) or ENOENT. Anchor
		// it to an absolute path against the current cwd now, before the
		// cwd changes.
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("%s=%q cannot be made absolute: %w", ClaudeBinEnv, override, err)
		}
		return abs, nil
	}

	dir := strings.TrimSpace(os.Getenv(versionsDirEnv))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home dir to find claude binary: %w", err)
		}
		dir = filepath.Join(home, ".local", "share", "claude", "versions")
	}

	bin, err := pickNewestVersion(dir)
	if err != nil {
		return "", err
	}
	if err := checkExecutable(bin); err != nil {
		return "", fmt.Errorf("resolved claude binary %q is not usable: %w", bin, err)
	}
	return bin, nil
}

// pickNewestVersion returns the absolute path to the highest
// MAJOR.MINOR.PATCH-named entry in dir. Non-semver entries are ignored
// (the installer also drops lockfiles / partial downloads there).
// Ordering is numeric per component so 2.1.144 > 2.1.99 > 2.1.9.
func pickNewestVersion(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no claude install found: cannot read %s: %w", dir, err)
	}

	var bestName string
	var best [3]int
	found := false
	for _, e := range entries {
		v, ok := parseSemver(e.Name())
		if !ok {
			continue
		}
		if !found || lessVer(best, v) {
			best, bestName, found = v, e.Name(), true
		}
	}
	if !found {
		return "", fmt.Errorf("no semver-named claude binary in %s (looked for MAJOR.MINOR.PATCH entries)", dir)
	}
	return filepath.Join(dir, bestName), nil
}

// parseSemver parses "2.1.144" into [3]int{2,1,144}. A trailing
// pre-release/build suffix (e.g. "2.1.144-rc1") is rejected — release
// channels only ship clean MAJOR.MINOR.PATCH dirs and accepting suffixes
// would make ordering ambiguous.
func parseSemver(s string) ([3]int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// lessVer reports a < b component-wise.
func lessVer(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// checkExecutable returns nil iff path is an existing regular file with
// at least one execute bit set.
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory, not an executable")
	}
	if info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("not executable (mode %s)", info.Mode().Perm())
	}
	return nil
}
