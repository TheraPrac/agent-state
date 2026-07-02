package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/session"
)

// PairLogEvent is one line of the per-session pairing audit log
// (.as/pairing/<session-id>.jsonl — design-pair-live-iteration.md §6).
// CQRS-lite evidence: input to /pair --off's plan-seed + browser-verification
// credit, never a substitute for the merge gate. Fields are shared across all
// four types and left empty when not applicable to keep the encoder/decoder
// (and any future consumer) to one shape rather than a tagged union.
type PairLogEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"` // edit | command | server | observation

	// edit
	File string `json:"file,omitempty"`
	Repo string `json:"repo,omitempty"`

	// command / server
	Command  string `json:"command,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`

	// server
	Action   string `json:"action,omitempty"` // tp subcommand: up | down | restart | status
	Worktree string `json:"worktree,omitempty"`

	// observation
	Text string `json:"text,omitempty"`
}

// PairLogOpts holds flags for `st pair-log` (hook-invoked, never run by an
// operator directly — matches the capture-decision.sh precedent).
type PairLogOpts struct {
	Type     string
	File     string
	Repo     string
	Command  string
	ExitCode *int
	Action   string
	Worktree string
	Text     string
}

// pairingLogPath returns the audit-log path for a session, under the
// workspace root (cfg.Root()) alongside .as/sessions/ — .as/pairing/ is new
// session-scoped ephemeral state, gitignored like .as/agents/, .as/cache/.
func pairingLogPath(cfg *config.Config, sessionID string) string {
	return filepath.Join(cfg.Root(), ".as", "pairing", sessionID+".jsonl")
}

// PairLog appends one event to the current session's pairing audit log —
// but ONLY when this session's pairing marker is active (re-checked here in
// Go, independent of the bash hook's own pairing_active gate — defense in
// depth, matching "written by PostToolUse hooks that no-op unless
// pairing.active=true"). A no-op (exit 0, silent) when not paired: this is
// a hook-invoked command and a PostToolUse hook must never fail loudly for
// the overwhelmingly common unpaired case.
func PairLog(cfg *config.Config, sessMgr *session.Manager, sessionID string, opts PairLogOpts) int {
	if sessionID == "" {
		return 0
	}
	sess, err := sessMgr.Load(sessionID)
	if err != nil || sess == nil || sess.Pairing == nil || !sess.Pairing.Active {
		return 0
	}

	switch opts.Type {
	case "edit", "command", "server", "observation":
	default:
		fmt.Fprintf(os.Stderr, "st pair-log: unknown --type %q (want edit|command|server|observation)\n", opts.Type)
		return 2
	}

	event := PairLogEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      opts.Type,
		File:      opts.File,
		Repo:      opts.Repo,
		Command:   opts.Command,
		ExitCode:  opts.ExitCode,
		Action:    opts.Action,
		Worktree:  opts.Worktree,
		Text:      opts.Text,
	}

	line, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st pair-log: encoding event: %v\n", err)
		return 1
	}

	path := pairingLogPath(cfg, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "st pair-log: creating pairing log dir: %v\n", err)
		return 1
	}
	// O_APPEND: a single JSON line here is well under PIPE_BUF, so
	// concurrent appends (unlikely within one interactive session, but
	// possible with a spawned sub-agent) stay atomic without an explicit
	// lock — the same assumption the rest of the hook fleet's append-only
	// writes rely on.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "st pair-log: opening pairing log: %v\n", err)
		return 1
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "st pair-log: writing event: %v\n", err)
		return 1
	}
	return 0
}

// readPairLogEvents reads and decodes every event in a session's pairing
// audit log. A missing file is not an error — it means nothing was recorded
// (pairing was toggled with no tool calls in between) — returns an empty
// slice. A malformed line is skipped rather than aborting the whole read
// (best-effort evidence; one corrupt line must not lose every other event).
func readPairLogEvents(cfg *config.Config, sessionID string) ([]PairLogEvent, error) {
	path := pairingLogPath(cfg, sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var events []PairLogEvent
	for _, line := range splitLines(string(data)) {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		var ev PairLogEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, nil
}
