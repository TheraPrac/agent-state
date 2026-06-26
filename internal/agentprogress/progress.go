// Package agentprogress reads the per-agent progress records written by
// the workspace agent-progress.sh Stop hook (I-775). Each record is a
// tiny flat-YAML file at <root>/.as/agent-progress/<agent-id>.yaml
// capturing the one-line summary of the agent's most recent turn.
//
// `st watch` Loads these and emits them as live per-agent rows, closing
// the "I can't see the agents working" gap with the event-based half of
// agent observability (pairs with T-406's drain checkpoint).
//
// Format (one record per file, all fields top-level scalar):
//
//	agent_id: agent-b
//	session_id: <uuid>
//	updated: 2026-05-22T21:00:00Z
//	progress: "the one-line summary, YAML-escaped"
//
// The hook's escape (backslashes then quotes) is reversed in unquoteProgress.
// No new YAML dependency — matches the hand-rolled flat-YAML convention
// used by agent.parseRegistration / agentps.LoadRoster.
package agentprogress

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
)

// Record is one agent-progress entry.
type Record struct {
	AgentID   string
	SessionID string
	Updated   time.Time
	Progress  string
}

// ProgressDir returns the canonical location for agent-progress records:
// <root>/.as/agent-progress. The hook writes here on every Stop event.
// Returns "" if cfg is nil (callers treat as "no records yet").
func ProgressDir(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return filepath.Join(cfg.Root(), ".as", "agent-progress")
}

// Load reads every *.yaml file under dir and returns the records keyed
// by agent id. Best-effort: a missing dir is not an error (st watch
// starts before any agent has emitted a record); a single garbled /
// unreadable / structurally-empty file is skipped rather than dropping
// the whole map (mirrors agentps.LoadRoster's tolerance).
func Load(dir string) (map[string]Record, error) {
	out := map[string]Record{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		rec, ok := parseRecord(filepath.Join(dir, e.Name()))
		if !ok {
			continue // best-effort: one bad file does not blank the roster
		}
		if rec.AgentID == "" {
			// Filename-stem fallback (mirrors agentps).
			rec.AgentID = strings.TrimSuffix(e.Name(), ".yaml")
		}
		out[rec.AgentID] = rec
	}
	return out, nil
}

// parseRecord reads a flat-YAML agent-progress file. Returns ok=false
// for unreadable files or files that parse zero meaningful fields —
// callers (Load) skip such files rather than registering empty records.
func parseRecord(path string) (Record, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false
	}
	defer f.Close()
	var rec Record
	scanner := bufio.NewScanner(f)
	// Defensive 1 MiB line cap — assistant-text is capped to ~200 chars
	// by the hook, but a hand-edited file shouldn't be able to fail
	// Load() by exceeding bufio's default 64 KiB line limit.
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		k, v, ok := splitKV(scanner.Text())
		if !ok {
			continue
		}
		switch k {
		case "agent_id":
			rec.AgentID = v
		case "session_id":
			rec.SessionID = v
		case "updated":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				rec.Updated = t
			}
		case "progress":
			rec.Progress = unquoteProgress(v)
		}
	}
	if scanner.Err() != nil {
		return Record{}, false
	}
	// Empty parse ⇒ treat as garbled (don't register an empty record).
	if rec.AgentID == "" && rec.Updated.IsZero() && rec.SessionID == "" && rec.Progress == "" {
		return Record{}, false
	}
	return rec, true
}

// splitKV splits "key: value" on the first colon, returning trimmed key
// and value. Lines without the "<key>: <value>" shape are skipped.
func splitKV(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	if k == "" {
		return "", "", false
	}
	return k, v, true
}

// unquoteProgress reverses the hook's double-quoted form. The hook
// escapes `\` then `"` (order matters); we un-escape `\"` then `\\` (the
// reverse order) so a round-trip is exact. A bare value (no surrounding
// quotes) is returned as-is, to tolerate hand-edited records.
func unquoteProgress(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	return v
}
