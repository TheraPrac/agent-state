// Package agentps backs `st agent ps` — the read-only global
// process-table of the agent fleet (T-354), the static-snapshot
// sibling of T-353's `st watch` (live) / `st transcript` (history).
// It joins the canonical workspace roster with live registrations,
// agent-state active work, and the session-JSONL freshness signal.
// Pure join/render so the table is golden-testable.
package agentps

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
)

// RosterAgent is one entry from the canonical agent-workspaces
// registry — the authoritative "which agents exist and where".
type RosterAgent struct {
	AgentID   string
	Workspace string // absolute path to the agent's workspace dir
}

// AgentWorkspacesDir resolves the directory holding the
// agent-workspaces registry (`<theraprac-agents>/.as/agent-workspaces`,
// the PARENT of any single workspace — not under cfg.Root()).
//
// Resolution order, robust to layout and test-injectable:
//  1. $ST_AGENT_WORKSPACES_DIR (explicit override; tests use this);
//  2. cfg.Root() itself or its nearest ancestor (then $PWD or its
//     ancestors) that contains a `.as/agent-workspaces` directory.
//
// Returns "" if none found — the caller surfaces that explicitly
// rather than rendering a misleading empty table.
func AgentWorkspacesDir(cfg *config.Config) string {
	if d := os.Getenv("ST_AGENT_WORKSPACES_DIR"); d != "" {
		return d
	}
	starts := []string{}
	if cfg != nil && cfg.Root() != "" {
		starts = append(starts, cfg.Root())
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, start := range starts {
		if d := searchUp(start); d != "" {
			return d
		}
	}
	return ""
}

// searchUp walks from start toward the filesystem root (bounded) and
// returns the first `<ancestor>/.as/agent-workspaces` that exists.
func searchUp(start string) string {
	dir := start
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, ".as", "agent-workspaces")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached filesystem root
		}
		dir = parent
	}
	return ""
}

// LoadRoster reads every agent-*.yaml in dir into RosterAgents, sorted
// by AgentID (deterministic). It uses os.ReadDir so a missing OR
// unreadable directory surfaces as an error (the caller reports the
// absence — never a silent empty table; filepath.Glob would have hidden
// a permissions failure as zero matches). An individual unparseable
// file is skipped (best-effort, one bad file must not blank the whole
// fleet view).
func LoadRoster(dir string) ([]RosterAgent, error) {
	if dir == "" {
		return nil, os.ErrNotExist
	}
	entries, err := os.ReadDir(dir) // errors on missing/unreadable dir
	if err != nil {
		return nil, err
	}
	var out []RosterAgent
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		ra := parseRosterFile(body)
		if ra.AgentID == "" {
			// Fall back to the filename stem so the agent is still
			// listed even with a malformed body (visible, not dropped).
			ra.AgentID = strings.TrimSuffix(name, ".yaml")
		}
		out = append(out, ra)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

// parseRosterFile extracts agent_id / path from the flat
// agent-workspace yaml without a full yaml dep (the file is a simple
// top-level key: value document; only these two keys are needed).
func parseRosterFile(body []byte) RosterAgent {
	var ra RosterAgent
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "agent_id":
			ra.AgentID = v
		case "path":
			ra.Workspace = v
		}
	}
	return ra
}
