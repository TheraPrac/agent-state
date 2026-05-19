package agentps

import (
	"strings"
	"testing"
	"time"
)

func TestReltime(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "—"},
		{now.Add(-30 * time.Second), "30s ago"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
		{now.Add(10 * time.Minute), "0s ago"}, // future / clock skew clamps
	}
	for _, c := range cases {
		if got := reltime(now, c.t); got != c.want {
			t.Errorf("reltime(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestLiveness(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	fresh := 10 * time.Minute
	freshMod := now.Add(-2 * time.Minute) // within window
	coldMod := now.Add(-3 * time.Hour)    // outside window
	var zero time.Time
	alive := &Reg{Alive: true}
	dead := &Reg{Alive: false}

	cases := []struct {
		name string
		reg  *Reg
		mod  time.Time
		want string
	}{
		{"fresh jsonl wins over no reg", nil, freshMod, "live"},
		{"fresh jsonl wins over dead reg", dead, freshMod, "live"},
		{"fresh jsonl wins over alive reg", alive, freshMod, "live"},
		{"reg present pid dead, cold", dead, coldMod, "stale"},
		{"reg present pid dead, no jsonl", dead, zero, "stale"},
		{"reg present pid alive, cold", alive, coldMod, "idle"},
		{"reg present pid alive, no jsonl", alive, zero, "idle"},
		{"no reg, cold jsonl (seen before)", nil, coldMod, "idle"},
		{"no reg, no jsonl (never observed)", nil, zero, "—"},
	}
	for _, c := range cases {
		if got := Liveness(c.reg, c.mod, now, fresh); got != c.want {
			t.Errorf("%s: Liveness = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAgentPSJoin(t *testing.T) {
	roster := []RosterAgent{
		{"agent-b", "/x/theraprac-agent-b"},
		{"agent-a", "/x/theraprac-agent-a"},
		{"agent-c", "/x/theraprac-agent-c"},
	}
	regs := map[string]Reg{
		"agent-a": {PID: 1, SessionID: "s-a", Alive: true},
	}
	active := map[string]ItemRef{
		"agent-c": {ID: "I-9", Stage: "pr_open"},
	}
	sessions := map[string]WSSession{"agent-a": {SID: "sx", Mod: time.Unix(1000, 0)}}

	rows := Join(roster, regs, active, sessions)
	if len(rows) != 3 {
		t.Fatalf("Join produced %d rows, want 3 (every roster agent)", len(rows))
	}
	// Sorted by AgentID regardless of roster order.
	if rows[0].AgentID != "agent-a" || rows[1].AgentID != "agent-b" || rows[2].AgentID != "agent-c" {
		t.Fatalf("rows not sorted by AgentID: %v", []string{rows[0].AgentID, rows[1].AgentID, rows[2].AgentID})
	}
	if rows[0].Reg == nil || rows[0].Reg.SessionID != "s-a" || rows[0].LastMod.IsZero() || rows[0].WSSessionID != "sx" {
		t.Errorf("agent-a row missing reg/session ground truth: %+v", rows[0])
	}
	if rows[1].Reg != nil || rows[1].Item != nil { // idle agent still present
		t.Errorf("agent-b should be idle (no reg/item): %+v", rows[1])
	}
	if rows[2].Item == nil || rows[2].Item.ID != "I-9" {
		t.Errorf("agent-c row missing active item: %+v", rows[2])
	}
}

func TestAgentPSRender(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	rows := []Row{
		{AgentID: "agent-a", Workspace: "/x/theraprac-agent-a",
			Reg:  &Reg{PID: 1, Started: now.Add(-2 * time.Hour), SessionID: "abcdefgh1234", Alive: true},
			Item: &ItemRef{ID: "T-203", Stage: "coding"}, LastMod: now.Add(-3 * time.Minute)},
		{AgentID: "agent-b", Workspace: "/x/theraprac-agent-b"}, // idle
		{AgentID: "agent-c", Workspace: "/x/theraprac-agent-c",
			Reg: &Reg{PID: 2, Started: now.Add(-26 * time.Hour), SessionID: "s", Alive: false}}, // stale pid
	}
	out := Render(rows, now)
	if len(out) != 4 {
		t.Fatalf("want header + 3 rows = 4 lines, got %d:\n%s", len(out), strings.Join(out, "\n"))
	}
	header := out[0]
	for _, col := range []string{"AGENT", "WORKSPACE", "LIVE", "CURRENT", "UPTIME", "LAST-UPDATE", "SESSION"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing column %q: %q", col, header)
		}
	}
	mustContain := func(line string, subs ...string) {
		for _, s := range subs {
			if !strings.Contains(line, s) {
				t.Errorf("line %q missing %q", line, s)
			}
		}
	}
	mustContain(out[1], "agent-a", "theraprac-agent-a", "live", "T-203 (coding)", "2h ago", "3m ago", "abcdefgh")
	// agent-b: no reg, no workspace JSONL → never-observed. Still
	// LISTED (idle agents never omitted) with LIVE = "—". Assert it is
	// specifically NOT live/idle/stale (a bare "—"-contains check would
	// be vacuous — every column of a zero-state row is "—").
	mustContain(out[2], "agent-b", "theraprac-agent-b")
	for _, banned := range []string{"live", "idle", "stale"} {
		if strings.Contains(out[2], banned) {
			t.Errorf("agent-b LIVE should be \"—\" (never observed), row=%q", out[2])
		}
	}
	mustContain(out[3], "agent-c", "stale", "1d ago")        // dead pid, 26h→1d uptime

	// Columns are aligned: the WORKSPACE column begins at the same rune
	// index on every row (verifies alignment without hardcoding pads).
	col2 := func(line string) int { return strings.Index(line, "theraprac-agent-") }
	base := strings.Index(out[1], "theraprac-agent-a")
	for _, l := range out[1:] {
		if col2(l) != base {
			t.Errorf("WORKSPACE column misaligned: %q (want col %d)", l, base)
		}
	}

	// Workspace filter is a separate pure step (applied by the caller
	// before render/JSON so both honour it).
	fr := FilterByWorkspace(rows, "agent-b")
	if len(fr) != 1 || fr[0].AgentID != "agent-b" {
		t.Fatalf("FilterByWorkspace = %+v, want only agent-b", fr)
	}
	f := Render(fr, now)
	if len(f) != 2 { // header + only agent-b
		t.Fatalf("filtered render: want header+1, got %d:\n%s", len(f), strings.Join(f, "\n"))
	}
	if !strings.Contains(f[1], "agent-b") || strings.Contains(strings.Join(f, "\n"), "agent-a") {
		t.Errorf("workspace filter leaked/dropped rows:\n%s", strings.Join(f, "\n"))
	}
	// sub=="" ⇒ unchanged (every roster agent kept).
	if got := FilterByWorkspace(rows, ""); len(got) != len(rows) {
		t.Errorf("FilterByWorkspace(\"\") dropped rows: %d vs %d", len(got), len(rows))
	}
}
