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
	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/mail"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/transcript"
)

// Phase 3 of T-353: `st transcript <item|agent|session>` — the
// historical half of the §8.1 observability spine. Resolves a selector
// to its on-disk session JSONL (Phase 1), merges the agent-state
// conversation channel (changelog + mail, §8.2), sorts by time, and
// renders via the Phase 2 pure renderer.

// TranscriptOpts are the `st transcript` flags.
type TranscriptOpts struct {
	Since string // "<dur>" (7d, 1d12h) or RFC3339 — drop older rows
	Grep  string // keep only rendered lines containing this substring
	Agent string // restrict to one tag
	JSON  bool   // emit raw []Row JSON (pre-render) for machines
}

// Transcript renders the transcript for selector (item id, agent id, or
// session id). Returns a process exit code. A missing/empty result is
// reported to stderr and returns non-zero — the absence is surfaced,
// never silently swallowed (operator silent-failure principle).
func Transcript(s *store.Store, cfg *config.Config, selector string, opts TranscriptOpts) int {
	if selector == "" {
		fmt.Fprintln(os.Stderr, "transcript: a selector is required (item id, agent id, or session id)")
		return 1
	}

	var since time.Time
	if opts.Since != "" {
		if d, err := parseDurationFlexible(opts.Since); err == nil {
			since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, opts.Since); err == nil {
			since = t
		} else {
			fmt.Fprintf(os.Stderr, "transcript: --since %q is neither a duration (7d, 1d12h) nor RFC3339\n", opts.Since)
			return 1
		}
	}

	var tagged []transcript.TaggedRow
	var scope string

	switch {
	case storeHasItem(s, selector):
		scope = "item " + selector
		item, _ := s.Get(selector)
		for i, se := range itemSessions(item) {
			tag := sessionTag(i)
			for _, p := range transcript.ResolveSessionJSONL(se.projectDir, se.sid) {
				tagged = append(tagged, readTagged(p, tag)...)
			}
		}
		if entries, err := changelog.Read(cfg, selector); err == nil {
			for _, e := range entries {
				tagged = append(tagged, changelogRow(e))
			}
		}

	case agentRegistered(cfg, selector):
		scope = "agent " + selector
		reg, _ := agent.LoadRegistration(cfg, selector)
		if reg == nil || reg.SessionID == "" {
			fmt.Fprintf(os.Stderr, "transcript: agent %q has no recorded session id\n", selector)
			return 1
		}
		for _, p := range transcript.ResolveSessionByID(reg.SessionID) {
			tagged = append(tagged, readTagged(p, selector)...)
		}
		// The agent's mailbox is the §8.2 conversation channel for an
		// agent-scoped view (pending only; cross-recipient item-mail
		// threading is the separate mailbox-evolution downstream item).
		if msgs, err := mail.List(cfg, selector); err == nil {
			for _, m := range msgs {
				tagged = append(tagged, mailRow(m))
			}
		}

	default:
		paths := transcript.ResolveSessionByID(selector)
		if len(paths) == 0 {
			fmt.Fprintf(os.Stderr, "transcript: %q is not a known item, agent, or on-disk session\n", selector)
			return 1
		}
		scope = "session " + selector
		for i, p := range paths {
			tagged = append(tagged, readTagged(p, sessionTag(i))...)
		}
	}

	if len(tagged) == 0 {
		fmt.Fprintf(os.Stderr, "transcript: nothing to show for %s\n", scope)
		return 1
	}

	if !since.IsZero() {
		kept := tagged[:0]
		for _, tr := range tagged {
			// Undated rows are kept — dropping them would silently hide
			// content just because Claude Code omitted a timestamp.
			if tr.Row.Timestamp.IsZero() || !tr.Row.Timestamp.Before(since) {
				kept = append(kept, tr)
			}
		}
		tagged = kept
	}
	if opts.Agent != "" {
		kept := tagged[:0]
		for _, tr := range tagged {
			if tr.Tag == opts.Agent {
				kept = append(kept, tr)
			}
		}
		tagged = kept
	}

	// Caller-owns-sort (Phase 2 Render is deliberately not a sorter):
	// stable by timestamp, undated rows last (in original order).
	sort.SliceStable(tagged, func(i, j int) bool {
		a, b := tagged[i].Row.Timestamp, tagged[j].Row.Timestamp
		if a.IsZero() != b.IsZero() {
			return !a.IsZero() // dated before undated
		}
		return a.Before(b)
	})

	if opts.JSON {
		rows := make([]transcript.Row, len(tagged))
		for i := range tagged {
			rows[i] = tagged[i].Row
		}
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "transcript: json encode: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	for _, l := range transcript.Render(tagged, transcript.RenderOpts{Timestamps: true}) {
		if opts.Grep != "" && !strings.Contains(l, opts.Grep) {
			continue
		}
		fmt.Println(l)
	}
	return 0
}

// --- helpers ---

func storeHasItem(s *store.Store, id string) bool {
	if s == nil {
		return false
	}
	_, ok := s.Get(id)
	return ok
}

func agentRegistered(cfg *config.Config, id string) bool {
	reg, err := agent.LoadRegistration(cfg, id)
	return err == nil && reg != nil
}

func sessionTag(i int) string {
	if i == 0 {
		return "A"
	}
	return "a-" + strconv.Itoa(i+1)
}

func readTagged(path, tag string) []transcript.TaggedRow {
	rows, err := transcript.ReadFile(path)
	if err != nil {
		// A resolved-but-unreadable file is surfaced as a visible row,
		// not skipped silently.
		return []transcript.TaggedRow{{
			Tag: tag,
			Row: transcript.Row{Kind: transcript.KindRaw, Text: fmt.Sprintf("<unreadable session file %s: %v>", path, err)},
		}}
	}
	out := make([]transcript.TaggedRow, len(rows))
	for i, r := range rows {
		out[i] = transcript.TaggedRow{Tag: tag, Row: r}
	}
	return out
}

type sessionRef struct {
	sid        string
	projectDir string
}

// itemSessions extracts (sid, project_dir) from an item's
// time_tracking.by_session list. It reuses parseBySessionLine (same
// package) which url-decodes project_dir correctly — so the I-678
// space-in-path class does not apply on this path.
func itemSessions(item *model.Item) []sessionRef {
	var out []sessionRef
	for _, raw := range timeTrackingListLines(item, "by_session") {
		a := parseBySessionLine(strings.TrimPrefix(raw, "- "))
		if a.SID != "" {
			out = append(out, sessionRef{sid: a.SID, projectDir: a.ProjectDir})
		}
	}
	return out
}

// timeTrackingListLines walks an item's Doc and returns the raw list
// entries under time_tracking.<key> (same proven walk as
// cmd/reconcile-tokens' extractor).
func timeTrackingListLines(item *model.Item, key string) []string {
	if item == nil || item.Doc == nil {
		return nil
	}
	var out []string
	inTT, inBlock := false, false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inTT = line.Key == "time_tracking"
			inBlock = false
			continue
		}
		if !inTT {
			continue
		}
		if line.Indent == 2 && line.Key == key {
			inBlock = true
			continue
		}
		if line.Indent <= 2 && line.Key != "" && line.Key != key {
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		if t := strings.TrimSpace(line.Raw); strings.HasPrefix(t, "- ") {
			out = append(out, t)
		}
	}
	return out
}

func parseRFC3339(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// changelogRow renders one agent-state changelog entry as a synthetic
// prose row tagged [chg] — the §8.2 conversation channel as a source.
func changelogRow(e changelog.Entry) transcript.TaggedRow {
	var b strings.Builder
	b.WriteString(e.Op)
	if e.Field != "" {
		b.WriteString(" " + e.Field)
	}
	if e.NewValue != "" {
		b.WriteString(" → " + foldLine(e.NewValue))
	}
	if e.Reason != "" {
		b.WriteString(" (" + foldLine(e.Reason) + ")")
	}
	if e.Agent != "" {
		b.WriteString(" by " + e.Agent)
	}
	return transcript.TaggedRow{
		Tag: "chg",
		Row: transcript.Row{Kind: transcript.KindText, Timestamp: parseRFC3339(e.Timestamp), Text: b.String()},
	}
}

func mailRow(m mail.Message) transcript.TaggedRow {
	txt := fmt.Sprintf("%s→%s: %s", m.From, m.To, foldLine(m.Body))
	if m.Item != "" {
		txt = "[" + m.Item + "] " + txt
	}
	return transcript.TaggedRow{
		Tag: "msg",
		Row: transcript.Row{Kind: transcript.KindText, Timestamp: parseRFC3339(m.At), Text: txt},
	}
}

// foldLine folds newlines to a visible ⏎ (never truncates — changelog
// reasons / mail bodies are surfaced in full, not silently clipped;
// distinct from run.go's scannable-prompt oneLine).
func foldLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", " ⏎ ")
	return strings.TrimSpace(s)
}
