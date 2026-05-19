package command

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// Phase 4 of T-353: `st watch` (no arg) — the live unified stream
// (contract §8.1/§8.3). Enumerates LIVE agents (process-tree liveness,
// §13 finding 3 — never the redirect log), tails each one's session
// JSONL, and prints a compressed per-agent strip (CompressByAgent): N
// readable lines, what each agent is doing NOW — never the raw
// firehose. Full per-agent drill is the later Layout-A TUI item.

// WatchOpts configures `st watch`.
type WatchOpts struct {
	Interval time.Duration // base poll interval; <=0 → 1s
	MaxIdle  time.Duration // backoff cap for idle ticks; <base → 30s
	Once     bool          // single pass then return (snapshot / tests)
}

// Watch runs the live stream. Returns a process exit code. With no live
// resolvable agents it reports that to stderr and returns non-zero
// (absence surfaced, never a silent empty success).
func Watch(cfg *config.Config, opts WatchOpts) int {
	base := opts.Interval
	if base <= 0 {
		base = time.Second
	}
	idleCap := opts.MaxIdle
	if idleCap <= 0 {
		idleCap = 30 * time.Second
	}
	if idleCap < base {
		idleCap = base // never back off to FASTER than the user's --interval
	}

	regs, err := agent.ListRegistrations(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: cannot list agent registrations: %v\n", err)
		return 1
	}

	type agentTail struct {
		tag     string
		readers []*transcript.TailReader
	}
	var tails []agentTail
	for _, r := range regs {
		if r == nil || r.SessionID == "" || !agent.IsPIDLive(r.PID) {
			continue
		}
		var rs []*transcript.TailReader
		for _, p := range transcript.ResolveSessionByID(r.SessionID) {
			// Once = snapshot → read from the start so there is content
			// to show; follow = from end → stream from now (history is
			// `st transcript`'s job, not the live strip's).
			if opts.Once {
				rs = append(rs, transcript.NewTailReaderFromStart(p))
			} else {
				rs = append(rs, transcript.NewTailReader(p))
			}
		}
		if len(rs) > 0 {
			tails = append(tails, agentTail{tag: r.AgentID, readers: rs})
		}
	}
	if len(tails) == 0 {
		fmt.Fprintln(os.Stderr, "watch: no live agents with a resolvable session JSONL")
		return 1
	}

	// Recent rows per tag (agents + the changelog channel). We keep a
	// bounded TAIL of rows per tag, not just the last one: CompressByAgent
	// must render a tag's rows together so a freshest tool_use+result
	// still collapses (one lone trailing tool_result would otherwise
	// render as a misleading "orphan"). The cap bounds memory on a
	// long-lived watch while staying far larger than any tool_use→result
	// gap. Mailbox tailing beyond the changelog is the §8.2
	// mailbox-evolution downstream item (contract §10) — out of scope.
	const perTagCap = 256
	recent := map[string][]transcript.TaggedRow{}
	var lastChg time.Time
	chgWarned := false

	addRow := func(tag string, row transcript.Row) {
		s := append(recent[tag], transcript.TaggedRow{Tag: tag, Row: row})
		if len(s) > perTagCap {
			s = s[len(s)-perTagCap:]
		}
		recent[tag] = s
	}

	poll := func() bool {
		changed := false
		for _, at := range tails {
			for _, rd := range at.readers {
				for _, row := range rd.Read() {
					addRow(at.tag, row)
					changed = true
				}
			}
		}
		all, err := changelog.ReadAll(cfg)
		if err != nil {
			if !chgWarned { // degrade, don't swallow — and don't spam every tick
				fmt.Fprintf(os.Stderr, "watch: warning: changelog unavailable, omitting it: %v\n", err)
				chgWarned = true
			}
		} else {
			chgWarned = false
			for _, entries := range all {
				for _, e := range entries {
					if ts := parseRFC3339(e.Timestamp); ts.After(lastChg) {
						lastChg = ts
						addRow("chg", changelogRow(e).Row)
						changed = true
					}
				}
			}
		}
		return changed
	}

	snapshot := func() {
		tagsSorted := make([]string, 0, len(recent))
		for tag := range recent {
			tagsSorted = append(tagsSorted, tag)
		}
		sort.Strings(tagsSorted)
		var rows []transcript.TaggedRow
		for _, tag := range tagsSorted {
			rows = append(rows, recent[tag]...)
		}
		fmt.Printf("── %s · %d live ──\n", time.Now().Format("15:04:05"), len(tails))
		for _, l := range transcript.CompressByAgent(rows, transcript.RenderOpts{Timestamps: true}) {
			fmt.Println(l)
		}
	}

	if opts.Once {
		poll()
		if len(recent) == 0 {
			// Live agents but nothing parsed yet is a real, reportable
			// state (not an error, not a silent blank).
			fmt.Fprintf(os.Stderr, "watch: %d live agent(s), no activity in their session JSONL yet\n", len(tails))
			return 0
		}
		snapshot()
		return 0
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	interval := base
	for {
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "\nwatch: interrupted")
			snapshot()
			return 0
		case <-time.After(interval):
			if poll() {
				snapshot()
				interval = base // activity → tighten
			} else {
				interval *= 2 // idle → back off, no busy-spin
				if interval > idleCap {
					interval = idleCap
				}
			}
		}
	}
}
