package command

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/store"
)

// defaultMailWindow is how far back the coordination block looks for
// "recent" mail. Newer messages get surfaced; older ones are ignored
// (they're stale signal). 30 minutes per the T-314 spec.
const defaultMailWindow = 30 * time.Minute

// buildCoordinationBlock assembles the markdown that st run injects
// near the top of every Claude prompt. Three sections per T-314:
//
//   - Active Agents: live registrations from .as/agents/, with each
//     agent's currently-claimed item resolved against the store.
//   - Recent Mail: unconsumed pending mail for self, newer than the
//     window cutoff. SURFACING IS A CONSUMING READ — every message
//     listed here is moved to archive before this function returns.
//     One-time delivery; the next prompt only sees newly-arrived mail.
//   - Coordination Rules: static guidance that codifies the read/send
//     asymmetry (Claude SENDS via `st mail send`; st run READS for it).
//
// Returns the block (newline-prefixed and -suffixed for clean
// concatenation) and an error only on hard failures — soft failures
// (e.g. a bad registration file) are logged and skipped.
//
// Pass selfAgentID = cfg.Identity().ID at the call site so this
// function is testable without mutating env. T-314.
func buildCoordinationBlock(s *store.Store, cfg *config.Config, selfAgentID, selfItemID string) string {
	return buildCoordinationBlockWithWindow(s, cfg, selfAgentID, selfItemID, defaultMailWindow)
}

// CoordinationShow prints the coordination block (active agents + pending
// mail + coordination rules) to stdout using the given mail window. Returns
// 0 on success. I-568: called by the session-start hook so every
// interactive Claude session sees peer state without requiring st run.
//
// selfAgentID is passed explicitly (rather than resolved internally) so
// callers can inject a test identity without mutating the environment.
// T-314 testability contract.
func CoordinationShow(s *store.Store, cfg *config.Config, selfAgentID string, mailWindow time.Duration) int {
	block := buildCoordinationBlockWithWindow(s, cfg, selfAgentID, "", mailWindow)
	fmt.Print(block)
	return 0
}

// buildCoordinationBlockWithWindow is the configurable variant of
// buildCoordinationBlock. Pass a larger window (e.g. 7*24*time.Hour)
// when surfacing at session-start so messages sent hours or days ago
// are still shown to the newly-started agent. I-568.
func buildCoordinationBlockWithWindow(s *store.Store, cfg *config.Config, selfAgentID, selfItemID string, mailWindow time.Duration) string {
	var b strings.Builder
	b.WriteString("\n\n## Active Agents\n")
	writeActiveAgents(&b, s, cfg, selfAgentID, selfItemID)

	b.WriteString(fmt.Sprintf("\n## Recent Mail (last %s, unconsumed)\n", formatMailWindow(mailWindow)))
	writeRecentMailAndConsume(&b, cfg, selfAgentID, mailWindow)

	b.WriteString(coordinationRulesText)
	return b.String()
}

// formatMailWindow renders a duration as a concise human-readable label
// for the Recent Mail section header. Produces "N min" for sub-hour
// values (matching the original "last 30 min" label for defaultMailWindow)
// and "Nh" for hour-aligned values.
func formatMailWindow(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// writeActiveAgents enumerates live agent registrations and the item
// each is currently claiming. Dead PIDs are skipped — they're treated
// as not-actually-active even if their registration file is still on
// disk pending the next sweep.
func writeActiveAgents(b *strings.Builder, s *store.Store, cfg *config.Config, selfAgentID, selfItemID string) {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		fmt.Fprintf(b, "  (registry unavailable: %v)\n", err)
		return
	}
	type row struct{ id, item, title, suffix string }
	var rows []row
	for _, r := range regs {
		if !agent.IsPIDLive(r.PID) {
			continue
		}
		// Best-effort claim resolution: scan items for one whose
		// claimed_by matches this registration's session.
		var itemID, title string
		if r.SessionID != "" {
			for _, it := range s.All() {
				if it.ClaimedBy == r.SessionID {
					itemID = it.ID
					title = it.Title
					break
				}
			}
		}
		// Fall back to the item passed in for self when registration
		// hasn't yet been linked to the claim.
		if r.AgentID == selfAgentID && itemID == "" && selfItemID != "" {
			itemID = selfItemID
			if it, ok := s.Get(selfItemID); ok {
				title = it.Title
			}
		}
		suffix := ""
		if r.AgentID == selfAgentID {
			suffix = " (you)"
		}
		rows = append(rows, row{id: r.AgentID, item: itemID, title: title, suffix: suffix})
	}
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	for _, r := range rows {
		if r.item == "" {
			fmt.Fprintf(b, "  %s%s: (no item claimed)\n", r.id, r.suffix)
		} else {
			fmt.Fprintf(b, "  %s%s: %s — %s\n", r.id, r.suffix, r.item, r.title)
		}
	}
}

// writeRecentMailAndConsume is the consuming surface for the prompt
// injection. Mail older than the cutoff is ignored entirely (left in
// pending, not surfaced); mail within the window is rendered AND
// archived so the next prompt doesn't re-surface it.
func writeRecentMailAndConsume(b *strings.Builder, cfg *config.Config, selfAgentID string, window time.Duration) {
	if selfAgentID == "" {
		b.WriteString("  (no self identity — skipped)\n")
		return
	}
	msgs, err := mail.List(cfg, selfAgentID)
	if err != nil {
		fmt.Fprintf(b, "  (mailbox unavailable: %v)\n", err)
		return
	}
	cutoff := time.Now().Add(-window)
	var fresh []mail.Message
	for _, m := range msgs {
		if t, err := time.Parse(time.RFC3339, m.At); err == nil && t.After(cutoff) {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, m := range fresh {
		// Render in the spec's format: [kind] from <sender> at HH:MM — body
		ts := m.At
		if t, err := time.Parse(time.RFC3339, m.At); err == nil {
			ts = t.Format("15:04")
		}
		itemTag := ""
		if m.Item != "" {
			itemTag = " (" + m.Item + ")"
		}
		fmt.Fprintf(b, "  [%s] from %s at %s%s — %q\n", m.Kind, m.From, ts, itemTag, m.Body)
	}
	// Consume — one-time delivery per spec.
	for _, m := range fresh {
		if err := mail.Archive(cfg, selfAgentID, m.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: coordination block: archive %s: %v\n", m.ID, err)
		}
	}
}

// coordinationRulesText is the static guidance the prompt always
// carries. Versioned as a const (rather than loaded from a file) so
// the rules ship with the binary and stay in sync with the read/send
// asymmetry the rest of the code enforces.
const coordinationRulesText = `
## Coordination Rules
- If you discover something that affects another agent's work, send mail:
    st mail send <agent-id> --kind <warning|request|alert|pause|resume> --body "..."
- Do NOT call ` + "`st mail list`" + ` or ` + "`st mail show`" + ` yourself — st run reads on your behalf
  and the messages above are the only ones you need to react to right now.
- Status changes (claimed, completed, blocked) live in agent-state, not mail —
  do not broadcast those.
- Mail kinds:
    warning   — informational FYI, may affect their work
    request   — asking for a code review or opinion
    need_help — you're blocked and want a peer to step in
    alert     — stop everything, critical (security, broken main)
    pause     — stop touching this repo (force-push imminent, schema change)
    resume    — OK to continue after a prior pause
`
