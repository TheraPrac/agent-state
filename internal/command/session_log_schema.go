package command

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/model"
)

// realTokens captures Anthropic SDK token field names verbatim. This is the
// canonical I-569 schema — all per-item rollups use these names so a future
// reader can map directly onto a `usage` block from the Anthropic transcript
// JSONL without translation. The legacy item-level fields
// (reg_input_tokens, cache_in_tokens, cache_out_tokens, cache_out_1h_tokens)
// remain populated alongside this structure during the transition; Step 9 of
// the I-569 plan removes them.
type realTokens struct {
	Input           int
	Output          int
	CacheRead       int
	CacheCreation5m int
	CacheCreation1h int
}

func (a realTokens) add(b realTokens) realTokens {
	return realTokens{
		Input:           a.Input + b.Input,
		Output:          a.Output + b.Output,
		CacheRead:       a.CacheRead + b.CacheRead,
		CacheCreation5m: a.CacheCreation5m + b.CacheCreation5m,
		CacheCreation1h: a.CacheCreation1h + b.CacheCreation1h,
	}
}

// realTokensFromPayload maps the per-turn payload field names onto the
// canonical Anthropic field names. After I-569 step 9 the cache fields
// already match (CacheReadInputTokens / CacheCreation5mInputTokens /
// CacheCreation1hInputTokens); the regular-input/output fields keep the
// `Reg*` prefix for now to disambiguate from the cumulative-item view's
// `Input`/`Output` aggregate.
func realTokensFromPayload(p SessionLogPayload) realTokens {
	return realTokens{
		Input:           p.RegInputTokens,
		Output:          p.RegOutputTokens,
		CacheRead:       p.CacheReadInputTokens,
		CacheCreation5m: p.CacheCreation5mInputTokens,
		CacheCreation1h: p.CacheCreation1hInputTokens,
	}
}

// formatRealTokensBlob serializes a realTokens to the stable space-separated
// `key=value` format used inside time_tracking lines. Order is fixed so two
// values with the same totals always produce the same line — the grep-/diff-
// friendly invariant the rest of the agent-state file format relies on.
func formatRealTokensBlob(t realTokens) string {
	return fmt.Sprintf(
		"input=%d output=%d cache_read=%d cache_creation_5m=%d cache_creation_1h=%d",
		t.Input, t.Output, t.CacheRead, t.CacheCreation5m, t.CacheCreation1h,
	)
}

// parseRealTokensBlob inverts formatRealTokensBlob. Missing or malformed
// fields stay at zero so partial-write scenarios (legacy lines, manual edits)
// don't crash callers.
func parseRealTokensBlob(s string) realTokens {
	var t realTokens
	for _, tok := range strings.Fields(s) {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			continue
		}
		key, val := tok[:eq], tok[eq+1:]
		n, err := strconv.Atoi(val)
		if err != nil {
			continue
		}
		switch key {
		case "input":
			t.Input = n
		case "output":
			t.Output = n
		case "cache_read":
			t.CacheRead = n
		case "cache_creation_5m":
			t.CacheCreation5m = n
		case "cache_creation_1h":
			t.CacheCreation1h = n
		}
	}
	return t
}

// readRealTokens fetches the cumulative real_tokens line from time_tracking.
// Returns the zero value if the field doesn't exist yet.
func readRealTokens(item *model.Item) realTokens {
	if item == nil || item.Doc == nil {
		return realTokens{}
	}
	val, ok := item.Doc.GetNestedField("time_tracking.real_tokens")
	if !ok {
		return realTokens{}
	}
	return parseRealTokensBlob(val)
}

// writeRealTokens replaces the cumulative real_tokens line. Always writes a
// full blob (every field) so a parser that sees a partial line knows it's
// looking at hand-edited or legacy data.
func writeRealTokens(item *model.Item, t realTokens) {
	item.SetNested("time_tracking", "real_tokens", formatRealTokensBlob(t))
}

// byStepAggregate captures per-step running totals. "step" is the
// SessionLogPayload.Step field — typically "interactive" or "subagent" but
// open-ended (a /code-review run could ship step:"code-review" once that
// pipeline is wired through).
type byStepAggregate struct {
	Turns  int
	Tokens realTokens
	Ms     int64
}

// formatByStepLine produces "<step>: turns=N <tokens_blob> ms=N".
func formatByStepLine(step string, a byStepAggregate) string {
	return fmt.Sprintf("%s: turns=%d %s ms=%d",
		step, a.Turns, formatRealTokensBlob(a.Tokens), a.Ms)
}

// byStepLineMatches returns true if a list entry's leading "<step>:" matches.
func byStepLineMatches(raw, step string) bool {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "- ")
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		return trimmed[:idx] == step
	}
	return false
}

// parseByStepLine inverts formatByStepLine.
func parseByStepLine(entry string) byStepAggregate {
	var a byStepAggregate
	colon := strings.IndexByte(entry, ':')
	if colon < 0 {
		return a
	}
	rest := strings.TrimSpace(entry[colon+1:])
	for _, tok := range strings.Fields(rest) {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			continue
		}
		key, val := tok[:eq], tok[eq+1:]
		switch key {
		case "turns":
			fmt.Sscanf(val, "%d", &a.Turns)
		case "ms":
			fmt.Sscanf(val, "%d", &a.Ms)
		case "input":
			fmt.Sscanf(val, "%d", &a.Tokens.Input)
		case "output":
			fmt.Sscanf(val, "%d", &a.Tokens.Output)
		case "cache_read":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheRead)
		case "cache_creation_5m":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheCreation5m)
		case "cache_creation_1h":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheCreation1h)
		}
	}
	return a
}

// readByStep walks time_tracking.by_step and returns the aggregate for the
// given step name, or the zero value if absent.
func readByStep(item *model.Item, step string) byStepAggregate {
	return readListAggregate(item, "by_step", step, func(entry string) byStepAggregate {
		return parseByStepLine(entry)
	})
}

// upsertByStep finds the existing line for `step`, adds the payload's deltas,
// and rewrites the line in place (or appends if new).
func upsertByStep(item *model.Item, step string, t realTokens, processMs int64) {
	if step == "" {
		step = "interactive"
	}
	existing := readByStep(item, step)
	existing.Turns++
	existing.Tokens = existing.Tokens.add(t)
	existing.Ms += processMs

	line := formatByStepLine(step, existing)
	if !updateListLine(item, "time_tracking", "by_step",
		func(raw string) bool { return byStepLineMatches(raw, step) },
		line) {
		item.Doc.AppendToNestedList("time_tracking", "by_step", line)
	}
}

// bySessionAggregate captures per-session running totals. project_dir is the
// CLAUDE_PROJECT_DIR the producer fired from — propagated via
// SessionLogPayload.ProjectDir so reconcile-tokens (I-569 step 6) can build
// the correct `~/.claude/projects/<slug>/<sid>.jsonl` path back to ground
// truth. started_at is sticky (set once on the first turn); ended_at moves
// forward on every accrual.
type bySessionAggregate struct {
	SID        string
	ProjectDir string
	StartedAt  string
	EndedAt    string
	Turns      int
	Tokens     realTokens
}

// formatBySessionLine produces a stable, single-line representation. Fields
// outside the tokens blob use space-separated `key=value` pairs; the tokens
// blob lives in the middle of the line and reuses the same key=value format.
//
// project_dir is URL-encoded so paths containing spaces (`/Users/john doe/...`)
// survive the strings.Fields tokenization on the parse side. Reconcile-tokens'
// JSONL slug derivation requires the original path back; without encoding, a
// space-containing path would silently truncate at the first space and
// reconcile would emit a "no JSONL truth" row with the wrong slug.
func formatBySessionLine(a bySessionAggregate) string {
	return fmt.Sprintf(
		"sid=%s project_dir=%s started_at=%s ended_at=%s turns=%d %s",
		a.SID, encodeFieldValue(a.ProjectDir), a.StartedAt, a.EndedAt, a.Turns,
		formatRealTokensBlob(a.Tokens),
	)
}

// encodeFieldValue makes a value safe for the space-separated `key=value`
// format used in time_tracking list lines. Only spaces are encoded — every
// other path character (slashes, dots, dashes) is left unchanged so the
// rendered line stays grep-friendly. Decoding is the inverse via
// decodeFieldValue.
func encodeFieldValue(s string) string {
	if !strings.ContainsAny(s, " \t") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ':
			b.WriteString("%20")
		case '\t':
			b.WriteString("%09")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodeFieldValue reverses encodeFieldValue. A bare value with no encoded
// sequences passes through unchanged so legacy lines (written before the
// encoding shipped) keep working.
func decodeFieldValue(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	s = strings.ReplaceAll(s, "%20", " ")
	s = strings.ReplaceAll(s, "%09", "\t")
	return s
}

// bySessionLineMatches returns true when a list entry's `sid=` field equals
// the target session id.
func bySessionLineMatches(raw, sid string) bool {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "- ")
	for _, tok := range strings.Fields(trimmed) {
		if eq := strings.IndexByte(tok, '='); eq >= 0 {
			if tok[:eq] == "sid" {
				return tok[eq+1:] == sid
			}
		}
	}
	return false
}

// parseBySessionLine inverts formatBySessionLine. project_dir is decoded via
// decodeFieldValue so encoded spaces survive round-trip.
func parseBySessionLine(entry string) bySessionAggregate {
	var a bySessionAggregate
	for _, tok := range strings.Fields(entry) {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			continue
		}
		key, val := tok[:eq], tok[eq+1:]
		switch key {
		case "sid":
			a.SID = val
		case "project_dir":
			a.ProjectDir = decodeFieldValue(val)
		case "started_at":
			a.StartedAt = val
		case "ended_at":
			a.EndedAt = val
		case "turns":
			fmt.Sscanf(val, "%d", &a.Turns)
		case "input":
			fmt.Sscanf(val, "%d", &a.Tokens.Input)
		case "output":
			fmt.Sscanf(val, "%d", &a.Tokens.Output)
		case "cache_read":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheRead)
		case "cache_creation_5m":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheCreation5m)
		case "cache_creation_1h":
			fmt.Sscanf(val, "%d", &a.Tokens.CacheCreation1h)
		}
	}
	return a
}

// readBySession returns the aggregate for `sid`, or the zero value if absent.
func readBySession(item *model.Item, sid string) bySessionAggregate {
	return readListAggregate(item, "by_session", sid, func(entry string) bySessionAggregate {
		return parseBySessionLine(entry)
	})
}

// upsertBySession finds the existing line for `sid` (no-op if SessionID is
// empty — the orphan path handles those), adds the payload's deltas, sets
// started_at on first sight, advances ended_at, and writes the line back.
func upsertBySession(item *model.Item, sid, projectDir, now string, t realTokens) {
	if sid == "" {
		return
	}
	existing := readBySession(item, sid)
	if existing.SID == "" {
		existing.SID = sid
		existing.StartedAt = now
	}
	if projectDir != "" {
		existing.ProjectDir = projectDir
	}
	existing.EndedAt = now
	existing.Turns++
	existing.Tokens = existing.Tokens.add(t)

	line := formatBySessionLine(existing)
	if !updateListLine(item, "time_tracking", "by_session",
		func(raw string) bool { return bySessionLineMatches(raw, sid) },
		line) {
		item.Doc.AppendToNestedList("time_tracking", "by_session", line)
	}
}

// seedBySession creates the by_session line for `sid` if absent, setting
// only the identity fields (sid, project_dir, started_at) — it does NOT
// credit a turn or any tokens. Used by `st spawn` (T-360) to register
// the worker→item link the instant the worker launches, so
// `st transcript <item>` resolves it without waiting for the worker's
// first session-log flush. Unlike upsertBySession (a "record a
// completed turn" call that unconditionally Turns++), this is a pure
// seed: the worker's own later upsertBySession calls then accumulate
// the real turn/token deltas onto the same sid-keyed line. Idempotent
// for its caller: sid/started_at are written only when absent and
// counters (turns/tokens/ended_at) are never touched, so re-seeding
// with the same project_dir is a no-op (project_dir is refreshed when
// non-empty — the call site always passes the same worktree).
func seedBySession(item *model.Item, sid, projectDir, now string) {
	if sid == "" {
		return
	}
	existing := readBySession(item, sid)
	if existing.SID == "" {
		existing.SID = sid
		existing.StartedAt = now
	}
	if projectDir != "" {
		existing.ProjectDir = projectDir
	}

	line := formatBySessionLine(existing)
	if !updateListLine(item, "time_tracking", "by_session",
		func(raw string) bool { return bySessionLineMatches(raw, sid) },
		line) {
		item.Doc.AppendToNestedList("time_tracking", "by_session", line)
	}
}

// readListAggregate is a generic helper for the by_step / by_session walk:
// scans time_tracking.<key> list entries, finds the one whose payload matches,
// and runs the supplied parser on it. Returns the zero value if no match.
//
// The matcher is intentionally inlined per caller (byStepLineMatches /
// bySessionLineMatches) so the key column convention stays explicit at the
// call site instead of hidden behind a generic "first field equals target".
func readListAggregate[T any](item *model.Item, listKey, target string, parse func(string) T) T {
	var zero T
	if item == nil || item.Doc == nil {
		return zero
	}
	inWT := false
	inBlock := false
	for _, line := range item.Doc.Lines {
		if line.Indent == 0 && line.Key != "" {
			inWT = line.Key == "time_tracking"
			inBlock = false
			continue
		}
		if !inWT {
			continue
		}
		if line.Indent == 2 && line.Key == listKey {
			inBlock = true
			continue
		}
		if line.Indent <= 2 && line.Key != "" && line.Key != listKey {
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		trimmed := strings.TrimSpace(line.Raw)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		entry := strings.TrimPrefix(trimmed, "- ")
		switch listKey {
		case "by_step":
			if byStepLineMatches(entry, target) {
				return parse(entry)
			}
		case "by_session":
			if bySessionLineMatches(entry, target) {
				return parse(entry)
			}
		}
	}
	return zero
}
