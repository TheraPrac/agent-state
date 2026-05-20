package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// status_me.go (T-377, I-712) — `st status --me`: the substrate-derived
// per-agent rollup over four dimensions that an operator actually tracks
// during a multi-item agent drive:
//
//   DONE          (recently closed by me)
//   IN-FLIGHT     (active and assigned to me)
//   NEEDS-YOU     (queue items I proposed, awaiting operator approval)
//   PROPOSED-NEXT (queue items I proposed, approved, behind position 1)
//
// Composes existing accessors (store, LoadQueue, cfg.Identity) — no new
// storage. JSON shape is the stable contract the T-378 arc filter and
// T-379 TUI surface will consume.

const defaultSince = 24 * time.Hour

// statusMeEntry is the per-item row in each section.
type statusMeEntry struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Priority    *int      `json:"priority,omitempty"`
	LastTouched time.Time `json:"last_touched,omitempty"`
	// Section-specific extras. Only one is non-zero per row; absent ones
	// are omitted from JSON for readability.
	QueuePos *int   `json:"queue_pos,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// statusMeReport is the stable machine contract for --format json.
type statusMeReport struct {
	Agent        string          `json:"agent"`
	Since        string          `json:"since"`
	Done         []statusMeEntry `json:"done"`
	InFlight     []statusMeEntry `json:"in_flight"`
	NeedsYou     []statusMeEntry `json:"needs_you"`
	ProposedNext []statusMeEntry `json:"proposed_next"`
}

// statusMeTo renders the rollup to w. Exported (lowercase) because the
// only caller is Status() above; tests drive this directly with a buffer.
func statusMeTo(w io.Writer, s *store.Store, cfg *config.Config, opts StatusOpts) int {
	agent := opts.Agent
	if agent == "" {
		agent = cfg.Identity().ID
	}
	if agent == "" {
		fmt.Fprintln(os.Stderr,
			"status --me: no agent identity resolved (set --agent <id> or run from a per-agent workspace)")
		return 1
	}

	since, err := resolveSince(opts.Since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status --me: %v\n", err)
		return 2
	}
	cutoff := time.Now().Add(-since)

	report := buildStatusMe(s, cfg, agent, cutoff, opts.Arc)

	if opts.JSON {
		return emitStatusMeJSON(w, report)
	}
	emitStatusMeText(w, report)
	return 0
}

// resolveSince parses opts.Since ("24h", "7d", "30m") into a positive
// duration; empty → defaultSince. "Nd" days is sugar for N*24h since
// time.ParseDuration doesn't accept days natively.
func resolveSince(spec string) (time.Duration, error) {
	if spec == "" {
		return defaultSince, nil
	}
	// "Nd" → "N*24h" (substrate convention used by T-329 Since too).
	if n := len(spec); n > 1 && spec[n-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(spec, "%dd", &days); err == nil && days >= 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		return 0, fmt.Errorf("--since %q: %v", spec, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("--since must be non-negative, got %s", d)
	}
	return d, nil
}

// buildStatusMe is the pure aggregator — no I/O beyond the supplied
// store + queue load. Used by tests directly with synthetic fixtures.
// `arc`, when non-empty, restricts every section to items with that Arc
// (T-378).
func buildStatusMe(s *store.Store, cfg *config.Config, agent string, cutoff time.Time, arc string) statusMeReport {
	r := statusMeReport{
		Agent: agent,
		Since: time.Since(cutoff).Round(time.Second).String(),
		// Initialise to empty slices, not nil, so the JSON contract is
		// always `[]` not `null` for empty sections — easier on
		// downstream consumers (T-378 arc filter, T-379 TUI surface).
		Done:         []statusMeEntry{},
		InFlight:     []statusMeEntry{},
		NeedsYou:     []statusMeEntry{},
		ProposedNext: []statusMeEntry{},
	}
	matchesArc := func(it *model.Item) bool { return arc == "" || it.Arc == arc }

	// DONE + IN-FLIGHT: scan items.
	for _, it := range s.All() {
		if it == nil || !matchesArc(it) {
			continue
		}
		// IN-FLIGHT: status active AND assigned to me.
		if it.Status == "active" && it.AssignedTo == agent {
			r.InFlight = append(r.InFlight, toEntry(it))
			continue
		}
		// DONE: terminal AND I last touched it AND within window.
		if cfg.IsTerminalStatus(it.Type, it.Status) &&
			it.LastTouchedBy == agent &&
			!it.LastTouched.Before(cutoff) {
			r.Done = append(r.Done, toEntry(it))
		}
	}

	// NEEDS-YOU + PROPOSED-NEXT: scan the queue.
	for pos, e := range LoadQueue(cfg) {
		if e.AddedBy != agent {
			continue
		}
		it, ok := s.Get(e.ID)
		if !ok || !matchesArc(it) {
			continue // dangling or filtered out
		}
		entry := toEntry(it)
		queuePos := pos + 1 // 1-indexed for operator readability
		entry.QueuePos = &queuePos
		entry.Reason = e.Reason
		if !e.Approved {
			r.NeedsYou = append(r.NeedsYou, entry)
		} else if pos > 0 {
			// Approved + behind position 1 = future intent, not the current pick.
			r.ProposedNext = append(r.ProposedNext, entry)
		}
	}

	sortStatusEntries(r.Done)
	sortStatusEntries(r.InFlight)
	sortStatusEntries(r.NeedsYou)
	sortStatusEntries(r.ProposedNext)
	return r
}

// toEntry projects a model.Item to the rollup-shape struct. Excludes
// fields the rollup doesn't render (description, SBAR, etc.) to keep
// the JSON contract compact.
func toEntry(it *model.Item) statusMeEntry {
	return statusMeEntry{
		ID:          it.ID,
		Type:        it.Type,
		Title:       it.Title,
		Status:      it.Status,
		Priority:    it.Priority,
		LastTouched: it.LastTouched,
	}
}

// sortStatusEntries: deterministic order — recent first (LastTouched
// desc), then ID asc on ties. Matches the recency philosophy of T-375
// without crossing into the TUI's per-keypress concerns.
func sortStatusEntries(es []statusMeEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].LastTouched.Equal(es[j].LastTouched) {
			return es[i].LastTouched.After(es[j].LastTouched)
		}
		return es[i].ID < es[j].ID
	})
}

func emitStatusMeJSON(w io.Writer, r statusMeReport) int {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "status --me: marshal: %v\n", err)
		return 1
	}
	fmt.Fprintln(w, string(b))
	return 0
}

func emitStatusMeText(w io.Writer, r statusMeReport) {
	fmt.Fprintf(w, "agent: %s   window: %s\n", r.Agent, r.Since)
	fmt.Fprintln(w, "─────────────────────────────────────────────")
	renderStatusSection(w, "DONE", r.Done, "(nothing closed in window)")
	renderStatusSection(w, "IN-FLIGHT", r.InFlight, "(nothing active)")
	renderStatusSection(w, "NEEDS-YOU", r.NeedsYou, "(no gates open)")
	renderStatusSection(w, "PROPOSED-NEXT", r.ProposedNext, "(no queued proposals)")
}

func renderStatusSection(w io.Writer, label string, entries []statusMeEntry, emptyMsg string) {
	fmt.Fprintf(w, "%s  (%d)\n", label, len(entries))
	if len(entries) == 0 {
		fmt.Fprintf(w, "  %s\n", emptyMsg)
		return
	}
	for _, e := range entries {
		p := "—"
		if e.Priority != nil {
			p = fmt.Sprintf("p%d", *e.Priority)
		}
		pos := ""
		if e.QueuePos != nil {
			pos = fmt.Sprintf("  [queue #%d]", *e.QueuePos)
		}
		fmt.Fprintf(w, "  %-8s %-3s  %s%s\n", e.ID, p, e.Title, pos)
	}
}
