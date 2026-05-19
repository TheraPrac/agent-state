// Package transcript is the shared substrate for rendering Claude Code
// session JSONL into a human-readable form (contract §8.1: "JSON-L is
// rendered, never read"). This file is Phase 1 of T-353: session-path
// resolution. Phase 2 adds the pure renderer; Phase 3/4 add the
// `st transcript` / `st watch` commands on top.
//
// The session JSONL substrate already exists on disk; cmd/reconcile-tokens
// previously carried private copies of projectSlug / claudeProjectsDir.
// Those are promoted here so every JSONL consumer resolves paths the same
// way (single source of truth — the I-569 reconcile path now delegates).
package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectSlug derives the ~/.claude/projects/<slug> directory name from a
// project_dir. Mirrors the bash hooks' transformation exactly:
//
//	echo "$PROJECT_DIR" | sed 's|^/|-|; s|/|-|g'
//
// Empty in → empty out (callers treat "" as "no resolvable session").
func ProjectSlug(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	s := projectDir
	if strings.HasPrefix(s, "/") {
		s = "-" + s[1:]
	}
	return strings.ReplaceAll(s, "/", "-")
}

// ClaudeProjectsDir returns ~/.claude/projects. CLAUDE_PROJECTS_DIR
// overrides it (used by tests and by alternate-home agent layouts).
func ClaudeProjectsDir() string {
	if d := os.Getenv("CLAUDE_PROJECTS_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// ResolveSessionJSONL returns the on-disk JSONL files for one session:
// the parent transcript first, then every subagent transcript Claude Code
// stored under <parent_session>/subagents/agent-*.jsonl, in sorted order.
//
// Only files that actually exist are returned, so callers can range over
// the result without per-path existence checks. An empty projectDir or
// sid (or a project with no transcript yet) yields nil — never an error;
// "no session on disk yet" is a normal state, not a failure (operator
// silent-failure principle: the absence is visible to the caller as an
// empty slice, not swallowed).
func ResolveSessionJSONL(projectDir, sid string) []string {
	if projectDir == "" || sid == "" {
		return nil
	}
	slug := ProjectSlug(projectDir)
	if slug == "" {
		return nil
	}
	base := filepath.Join(ClaudeProjectsDir(), slug)

	var paths []string
	parent := filepath.Join(base, sid+".jsonl")
	if fi, err := os.Stat(parent); err == nil && !fi.IsDir() {
		paths = append(paths, parent)
	}

	subDir := filepath.Join(base, sid, "subagents")
	if entries, err := os.ReadDir(subDir); err == nil {
		var subs []string
		for _, ent := range entries {
			n := ent.Name()
			if ent.IsDir() || !strings.HasPrefix(n, "agent-") || !strings.HasSuffix(n, ".jsonl") {
				continue
			}
			subs = append(subs, filepath.Join(subDir, n))
		}
		sort.Strings(subs) // deterministic ordering across runs
		paths = append(paths, subs...)
	}
	return paths
}
