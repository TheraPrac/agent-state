package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
)

// AgentRegisterOpts are the `st agent register` flags.
type AgentRegisterOpts struct {
	PID       int    // 0 → this process; the hook passes the Claude PID
	SessionID string // the Claude session id (hook stdin .session_id)
}

// AgentRegister is the T-357 producer: it records this workspace agent's
// live session in .as/agents/<id>.yaml so the registration-derived
// columns (UPTIME, authoritative SESSION, PID liveness) populate in
// `st agent ps` / `st watch`. Invoked from the SessionStart hook.
//
// It is hook-safe: it ALWAYS returns 0. A missing identity or a write
// failure is a stderr warning, never a non-zero exit — a registration
// problem must never break session start (and T-356's JSONL
// ground-truth overlay still works without it).
//
// It sweeps dead-PID registrations first so .as/agents/ self-cleans
// without a deregister hook (Claude Code has no SessionEnd event and
// Stop fires per-turn — see as/.plans/T-357.md).
func AgentRegister(cfg *config.Config, opts AgentRegisterOpts) int {
	id := cfg.Identity().ID
	if id == "" {
		fmt.Fprintln(os.Stderr, "agent register: no resolvable agent identity — skipping (st agent ps still works via the JSONL overlay)")
		return 0
	}
	if _, err := agent.Sweep(cfg); err != nil {
		// Non-fatal: a stale file we couldn't remove doesn't stop us
		// writing our own; T-356 renders it "stale" meanwhile.
		fmt.Fprintf(os.Stderr, "agent register: warning: sweep: %v\n", err)
	}
	reg, err := agent.RegisterSelf(cfg, agent.SelfOptions{
		AgentID:   id,
		PID:       opts.PID,
		SessionID: opts.SessionID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent register: warning: %v\n", err)
		return 0 // hook-safe: never break session start
	}
	fmt.Printf("registered %s (pid %d, session %s)\n", reg.AgentID, reg.PID, shortSession(reg.SessionID))
	return 0
}

// AgentDeregister removes this workspace agent's base-id registration.
// Idempotent; for explicit/scripted use and future `st spawn` workers —
// deliberately NOT wired to any hook (see as/.plans/T-357.md).
func AgentDeregister(cfg *config.Config) int {
	id := cfg.Identity().ID
	if id == "" {
		fmt.Fprintln(os.Stderr, "agent deregister: no resolvable agent identity")
		return 1
	}
	if err := agent.DeregisterSelf(cfg, id); err != nil {
		fmt.Fprintf(os.Stderr, "agent deregister: %v\n", err)
		return 1
	}
	fmt.Printf("deregistered %s\n", id)
	return 0
}

func shortSession(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
