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
	if idleCap < base {
		idleCap = 30 * time.Second
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

	// Freshest row per tag (agents + the changelog channel). Mailbox
	// tailing beyond the changelog is the §8.2 mailbox-evolution
	// downstream item (contract §10) — out of scope here.
	latest := map[string]transcript.TaggedRow{}
	var lastChg time.Time

	poll := func() bool {
		changed := false
		for _, at := range tails {
			for _, rd := range at.readers {
				for _, row := range rd.Read() {
					latest[at.tag] = transcript.TaggedRow{Tag: at.tag, Row: row}
					changed = true
				}
			}
		}
		if all, err := changelog.ReadAll(cfg); err == nil {
			for _, entries := range all {
				for _, e := range entries {
					if ts := parseRFC3339(e.Timestamp); ts.After(lastChg) {
						lastChg = ts
						latest["chg"] = changelogRow(e)
						changed = true
					}
				}
			}
		}
		return changed
	}

	snapshot := func() {
		rows := make([]transcript.TaggedRow, 0, len(latest))
		tagsSorted := make([]string, 0, len(latest))
		for tag := range latest {
			tagsSorted = append(tagsSorted, tag)
		}
		sort.Strings(tagsSorted)
		for _, tag := range tagsSorted {
			rows = append(rows, latest[tag])
		}
		fmt.Printf("── %s · %d live ──\n", time.Now().Format("15:04:05"), len(tails))
		for _, l := range transcript.CompressByAgent(rows, transcript.RenderOpts{Timestamps: true}) {
			fmt.Println(l)
		}
	}

	if opts.Once {
		poll()
		if len(latest) == 0 {
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
