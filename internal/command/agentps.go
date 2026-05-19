package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/agentps"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// AgentPSOpts are the `st agent ps` flags.
type AgentPSOpts struct {
	Workspace string // substring filter on workspace path; "" = all
	JSON      bool   // emit the joined []Row as JSON (pre-render)
	Invoked   string // how the user invoked it ("agent ps" / "agents") for messages; defaults to "agent ps"
}

// AgentPS prints the global agent process-table (T-354). Read-only.
// A missing/empty roster is reported to stderr with a non-zero exit
// (absence surfaced, never a silent blank table — operator
// silent-failure principle).
func AgentPS(s *store.Store, cfg *config.Config, opts AgentPSOpts) int {
	cmdName := opts.Invoked
	if cmdName == "" {
		cmdName = "agent ps"
	}
	dir := agentps.AgentWorkspacesDir(cfg)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "%s: no agent roster found (set $ST_AGENT_WORKSPACES_DIR or run from inside an agent workspace tree)\n", cmdName)
		return 1
	}
	roster, err := agentps.LoadRoster(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot read agent roster at %s: %v\n", cmdName, dir, err)
		return 1
	}
	if len(roster) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no agent roster entries in %s\n", cmdName, dir)
		return 1
	}

	// Live registrations → agentps.Reg keyed by AgentID (+ liveness).
	regs := map[string]agentps.Reg{}
	if list, err := agent.ListRegistrations(cfg); err != nil {
		// Degrade (the roster still renders) but never swallow.
		fmt.Fprintf(os.Stderr, "%s: warning: cannot list registrations: %v\n", cmdName, err)
	} else {
		for _, r := range list {
			if r == nil || r.AgentID == "" {
				continue
			}
			regs[r.AgentID] = agentps.Reg{
				PID:       r.PID,
				Started:   parseRFC3339(r.Started),
				SessionID: r.SessionID,
				Role:      r.Role,
				Alive:     agent.IsPIDLive(r.PID),
			}
		}
	}

	// Current active item per agent (lowest item id wins for a stable
	// deterministic pick when an agent has several active items —
	// NUMERIC by the id's numeric suffix so T-9 < T-10, not the
	// lexicographic T-10 < T-9).
	active := map[string]agentps.ItemRef{}
	if s != nil {
		items := s.List(store.StatusFilter("active"))
		sort.Slice(items, func(i, j int) bool { return lessItemID(items[i].ID, items[j].ID) })
		for _, it := range items {
			if it.AssignedTo == "" {
				continue
			}
			if _, seen := active[it.AssignedTo]; seen {
				continue
			}
			stage := ""
			// Match internal/command/list.go's contract: stage is only
			// surfaced when it is genuinely a string (a corrupt
			// non-string must not render as "<nil>"/"true").
			if v, ok := it.Delivery["stage"]; ok {
				if sv, ok := v.(string); ok {
					stage = sv
				}
			}
			active[it.AssignedTo] = agentps.ItemRef{ID: it.ID, Stage: stage}
		}
	}

	// "Last updated" = newest session-JSONL mtime (T-353 substrate;
	// §13 finding-3 freshness signal). Only agents with a live
	// registration have a session id to resolve.
	mtime := map[string]time.Time{}
	for id, r := range regs {
		if r.SessionID == "" {
			continue
		}
		var newest time.Time
		for _, p := range transcript.ResolveSessionByID(r.SessionID) {
			if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if !newest.IsZero() {
			mtime[id] = newest
		}
	}

	// Filter ONCE here so --workspace is honoured by BOTH --json and the
	// table (a render-only filter would silently no-op under --json).
	rows := agentps.FilterByWorkspace(agentps.Join(roster, regs, active, mtime), opts.Workspace)

	if opts.JSON {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: json encode: %v\n", cmdName, err)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	for _, line := range agentps.Render(rows, time.Now()) {
		fmt.Println(line)
	}
	return 0
}

// lessItemID orders agent-state ids (e.g. "T-9", "I-203") by prefix
// then NUMERIC suffix, so T-9 sorts before T-10 (a plain string
// compare would invert them and pick the wrong "current" item for an
// agent with several active items). Falls back to a string compare for
// ids that don't match the "<prefix>-<digits>" shape.
func lessItemID(a, b string) bool {
	pa, na, oka := splitItemID(a)
	pb, nb, okb := splitItemID(b)
	if oka && okb && pa == pb {
		return na < nb
	}
	if oka && okb && pa != pb {
		return pa < pb
	}
	return a < b
}

func splitItemID(id string) (prefix string, num int, ok bool) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 || i == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return "", 0, false
	}
	return id[:i], n, true
}
