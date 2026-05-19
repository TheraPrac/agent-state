package agentps

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Reg is the agentps-local projection of a live agent registration —
// kept dependency-free (the command layer adapts agent.Registration
// into this) so Join/Render are pure and golden-testable.
type Reg struct {
	PID       int
	Started   time.Time // zero if unparseable/absent
	SessionID string
	Role      string
	Alive     bool // agent.IsPIDLive(PID) result
}

// ItemRef is the agent's current active agent-state item.
type ItemRef struct {
	ID    string
	Stage string
}

// Row is one fully-joined fleet entry (raw — formatting/relative-time
// happens in Render with an injected now).
type Row struct {
	AgentID   string
	Workspace string
	Reg       *Reg      // nil ⇒ no live registration (idle)
	Item      *ItemRef  // nil ⇒ no active item
	LastMod   time.Time // newest session-JSONL mtime; zero ⇒ unknown
}

// Join merges the canonical roster with live registrations, active
// items, and per-agent session-JSONL mtimes. EVERY roster agent yields
// exactly one Row (idle agents are never omitted — operator
// silent-failure principle); rows are sorted by AgentID (deterministic,
// no map-iteration order leaks). Maps are keyed by AgentID.
func Join(roster []RosterAgent, regs map[string]Reg, active map[string]ItemRef, mtime map[string]time.Time) []Row {
	rows := make([]Row, 0, len(roster))
	for _, ra := range roster {
		row := Row{AgentID: ra.AgentID, Workspace: ra.Workspace}
		if r, ok := regs[ra.AgentID]; ok {
			rc := r
			row.Reg = &rc
		}
		if it, ok := active[ra.AgentID]; ok {
			ic := it
			row.Item = &ic
		}
		if t, ok := mtime[ra.AgentID]; ok {
			row.LastMod = t
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].AgentID < rows[j].AgentID })
	return rows
}

// Opts are render options.
// FilterByWorkspace narrows rows to those whose Workspace path contains
// sub (sub=="" ⇒ unchanged). Applied by the caller BEFORE the
// render/JSON split so `--workspace` is honoured in BOTH outputs (the
// filter is an explicit operator narrowing; the "every roster agent is
// listed" guarantee is about the DEFAULT unfiltered view — a
// non-matching or workspace-less agent is correctly excluded here).
func FilterByWorkspace(rows []Row, sub string) []Row {
	if sub == "" {
		return rows
	}
	out := rows[:0:0]
	for _, r := range rows {
		if strings.Contains(r.Workspace, sub) {
			out = append(out, r)
		}
	}
	return out
}

// Render produces the aligned process-table, one string per output
// line (header first). Deterministic given (rows, now). reltime turns
// timestamps into "3m ago" / "—"; a registration whose PID is dead
// shows "stale" (visible, not dropped). Filtering is the caller's job
// (FilterByWorkspace) so JSON and table outputs stay consistent.
func Render(rows []Row, now time.Time) []string {
	type cell struct{ agent, ws, live, cur, up, last, sess string }
	cells := make([]cell, 0, len(rows))
	for _, r := range rows {
		c := cell{agent: r.AgentID, ws: dirBase(r.Workspace), live: "—", cur: "—", up: "—", last: reltime(now, r.LastMod), sess: "—"}
		if r.Reg != nil {
			if r.Reg.Alive {
				c.live = "✓"
			} else {
				c.live = "stale" // registration present but PID dead
			}
			c.up = reltime(now, r.Reg.Started)
			if r.Reg.SessionID != "" {
				c.sess = shortID(r.Reg.SessionID)
			}
		}
		if r.Item != nil {
			c.cur = r.Item.ID
			if r.Item.Stage != "" {
				c.cur += " (" + r.Item.Stage + ")"
			}
		}
		cells = append(cells, c)
	}

	headers := cell{"AGENT", "WORKSPACE", "LIVE", "CURRENT", "UPTIME", "LAST-UPDATE", "SESSION"}
	all := append([]cell{headers}, cells...)
	w := make([]int, 7)
	for _, c := range all {
		for i, s := range []string{c.agent, c.ws, c.live, c.cur, c.up, c.last, c.sess} {
			if n := dispWidth(s); n > w[i] {
				w[i] = n
			}
		}
	}
	var out []string
	for _, c := range all {
		fields := []string{c.agent, c.ws, c.live, c.cur, c.up, c.last, c.sess}
		var b strings.Builder
		for i, s := range fields {
			b.WriteString(s)
			if i < len(fields)-1 {
				b.WriteString(strings.Repeat(" ", w[i]-dispWidth(s)+2))
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// reltime renders a coarse "ago" string. Zero time ⇒ "—" (unknown is
// shown as unknown, not faked as "now"). Future times ⇒ "0s ago"
// (clock skew shouldn't produce a negative).
func reltime(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func dirBase(p string) string {
	if p == "" {
		return "—"
	}
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dispWidth counts runes (not bytes) so multibyte glyphs like ✓/— do
// not over-pad the columns.
func dispWidth(s string) int { return len([]rune(s)) }
