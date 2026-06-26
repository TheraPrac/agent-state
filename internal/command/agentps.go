package command

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/theraprac/agent-state/internal/agent"
	"github.com/theraprac/agent-state/internal/agentps"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/store"
	"github.com/theraprac/agent-state/internal/transcript"
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

	// Ground truth (T-353 substrate; §13 finding-1: trust the substrate,
	// not the registration self-report): for EVERY roster agent resolve
	// its WORKSPACE's newest Claude session JSONL — works with no
	// registration at all, so LAST-UPDATE / SESSION / LIVE populate in
	// the operator-launched-session topology (the T-357 producer later
	// adds authoritative UPTIME on top).
	sessions := map[string]agentps.WSSession{}
	for _, ra := range roster {
		if _, sid, mod := transcript.NewestSessionForProjectDir(ra.Workspace); !mod.IsZero() {
			sessions[ra.AgentID] = agentps.WSSession{SID: sid, Mod: mod}
		}
	}

	// Filter ONCE here so --workspace is honoured by BOTH --json and the
	// table (a render-only filter would silently no-op under --json).
	rows := agentps.FilterByWorkspace(agentps.Join(roster, regs, active, sessions), opts.Workspace)

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

// lessItemID is a TOTAL, transitive order over agent-state ids (so it
// is a sound sort.Slice comparator for every input, not just the
// well-formed corpus): order by the letter prefix, then — within an
// equal prefix — numeric-suffix ids before non-numeric ones, numerics
// by value (T-9 < T-10, not the lexical T-10 < T-9), and any remaining
// ties / non-numeric suffixes by raw-suffix string order. Composing
// total orders only descending on equality keeps it transitive for
// arbitrary strings.
func lessItemID(a, b string) bool {
	pa, sa, na, oka := splitItemID(a)
	pb, sb, nb, okb := splitItemID(b)
	if pa != pb {
		return pa < pb
	}
	if oka != okb {
		return oka // numeric-suffix ids sort before non-numeric
	}
	if oka { // both numeric
		if na != nb {
			return na < nb
		}
		return sa < sb // tie-break (e.g. leading-zero variants)
	}
	return sa < sb // both non-numeric
}

// splitItemID decomposes "<prefix>-<suffix>". No '-' ⇒ the whole id is
// the prefix with an empty, non-numeric suffix. numeric is true only
// when the suffix parses as an int.
func splitItemID(id string) (prefix, suffix string, num int, numeric bool) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return id, "", 0, false
	}
	prefix, suffix = id[:i], id[i+1:]
	if n, err := strconv.Atoi(suffix); err == nil {
		return prefix, suffix, n, true
	}
	return prefix, suffix, 0, false
}
