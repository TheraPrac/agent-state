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
	"time"
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
// overrides it (tests, alternate-home agent layouts). If os.UserHomeDir
// cannot resolve the home directory, $HOME is tried before giving up.
// If both fail (a stripped environment) the result is the relative path
// ".claude/projects", which will not exist — so ResolveSessionJSONL's
// Stat/ReadDir simply find nothing and it returns an empty slice. That
// is the intended degradation: the caller sees a visible empty result,
// not a crash and not a swallowed error reported as success (operator
// silent-failure principle).
func ClaudeProjectsDir() string {
	if d := os.Getenv("CLAUDE_PROJECTS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "projects")
}

// ResolveSessionJSONL returns the on-disk JSONL files for one session:
// the parent transcript first, then every subagent transcript Claude Code
// stored under <parent_session>/subagents/agent-*.jsonl, in sorted order.
//
// The subagent filter is the precise "agent-*.jsonl" (Claude Code's
// actual subagent naming), deliberately narrower than
// cmd/reconcile-tokens' jsonlUsage walk, which accepts any "*.jsonl" in
// the subagents dir. The two are intentionally divergent: a future
// consolidation that points reconcile at this resolver must keep that in
// mind (a stray non-"agent-" .jsonl would no longer be summed). This is
// the correct precise filter for the renderer; it is documented here so
// the divergence is a deliberate choice, not a silent trap.
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

	paths = append(paths, subagentJSONL(base, sid)...)
	return paths
}

// subagentJSONL returns the sorted subagents/agent-*.jsonl files for a
// session whose parent transcript lives under base/. The "agent-*"
// filter is the precise Claude Code naming (see ResolveSessionJSONL's
// note on the deliberate divergence from reconcile's looser walk).
func subagentJSONL(base, sid string) []string {
	subDir := filepath.Join(base, sid, "subagents")
	entries, err := os.ReadDir(subDir)
	if err != nil {
		return nil
	}
	var subs []string
	for _, ent := range entries {
		n := ent.Name()
		if ent.IsDir() || !strings.HasPrefix(n, "agent-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		subs = append(subs, filepath.Join(subDir, n))
	}
	sort.Strings(subs) // deterministic ordering across runs
	return subs
}

// ResolveSessionByID resolves a bare session id with NO project-dir
// context: it scans every ~/.claude/projects/<slug>/ directory for
// <sid>.jsonl (+ that session's subagents). Used by `st transcript
// <session-id>` and the agent-id path (a registration carries the
// session id but not the project dir). Returns nil (never an error)
// when the id is empty or not found anywhere — the caller surfaces the
// absence explicitly (operator silent-failure principle).
func ResolveSessionByID(sid string) []string {
	if sid == "" {
		return nil
	}
	root := ClaudeProjectsDir()
	slugs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var paths []string
	for _, sl := range slugs {
		if !sl.IsDir() {
			continue
		}
		base := filepath.Join(root, sl.Name())
		parent := filepath.Join(base, sid+".jsonl")
		if fi, err := os.Stat(parent); err == nil && !fi.IsDir() {
			paths = append(paths, parent)
		}
		paths = append(paths, subagentJSONL(base, sid)...)
	}
	return paths
}

// NewestSessionForProjectDir resolves a workspace/project directory to
// its most-recently-active Claude session. It considers BOTH each
// top-level parent session JSONL and that session's
// subagents/agent-*.jsonl — an agent orchestrating subagents has a cold
// parent file while the subagents are hot, so parent-only would falsely
// report it idle. The returned mtime is the newest across the whole
// tree; sid is always the PARENT session id (subagents belong to it);
// path is the actual newest file (may be a subagent). ("", "", zero)
// when projectDir is empty or the project has no session on disk —
// never an error; the absence is the caller's to surface (operator
// silent-failure principle). This is the ground-truth "when did this
// workspace's agent last do something" signal (contract §13 finding 3:
// liveness reads the session JSONL, not a self-report) used by
// `st agent ps` / `st watch` independent of any registration.
func NewestSessionForProjectDir(projectDir string) (path, sid string, mod time.Time) {
	if projectDir == "" {
		return "", "", time.Time{}
	}
	slug := ProjectSlug(projectDir)
	if slug == "" {
		return "", "", time.Time{}
	}
	base := filepath.Join(ClaudeProjectsDir(), slug)
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", "", time.Time{}
	}
	consider := func(p, parentSID string, m time.Time) {
		if m.After(mod) {
			mod, path, sid = m, p, parentSID
		}
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		s := strings.TrimSuffix(n, ".jsonl")
		if fi, err := e.Info(); err == nil {
			consider(filepath.Join(base, n), s, fi.ModTime())
		}
		// Subagent activity counts as the parent session being active.
		for _, sp := range subagentJSONL(base, s) {
			if fi, err := os.Stat(sp); err == nil {
				consider(sp, s, fi.ModTime())
			}
		}
	}
	return path, sid, mod
}
