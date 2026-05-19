package transcript

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Phase 2 of T-353: the pure renderer (contract §8.1 — "JSON-L is
// rendered, never read"; tool spam collapses to one line, reasoning
// stays prose). No I/O, no time.Now, no env reads — same input always
// yields the same output, so it is golden-testable and the live stream
// (Phase 4) and historical view (Phase 3) share one deterministic core.

// TaggedRow is a Phase 1 Row plus the agent label it belongs to. Tag is
// rendered as `[A]` / `[a-2]`; the caller assigns it (per-session in
// Phase 3, per-registered-agent in Phase 4).
type TaggedRow struct {
	Tag string
	Row Row
}

// RenderOpts is zero-value-correct: the zero value shows thinking
// (reasoning is prose per §8.1), hides timestamps (golden stability),
// and uses the default truncation width.
type RenderOpts struct {
	HideThinking bool // zero value false → reasoning shown (the §8.1 default)
	Timestamps   bool // prefix UTC HH:MM:SS after the tag
	MaxLen       int  // truncation width for tool summaries / raw; <=0 → 120
}

const defaultMaxLen = 120

// Render turns tagged rows into human lines. One slice entry per output
// line, every line `[tag] …`-prefixed (greppable, deterministic). Rows
// are rendered in input order — the caller is responsible for any
// timestamp merge/sort; Render is pure formatting, never a sorter.
//
// Correlation: a tool_use is collapsed with its matching tool_result
// (by id) into ONE line; the standalone matched tool_result is then
// suppressed (already represented). A tool_use with no result yet
// (truncated/in-flight transcript) shows `→ …`; a tool_result with no
// matching tool_use anywhere (orphan) is rendered on its own line —
// never dropped (operator silent-failure principle).
func Render(rows []TaggedRow, opts RenderOpts) []string {
	maxLen := opts.MaxLen
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}

	// Pre-scan: result-by-id, and the set of tool_use ids that exist
	// anywhere (so a matched tool_result can be suppressed regardless
	// of whether it precedes or follows its tool_use).
	resultByID := make(map[string]*ToolResult)
	toolUseIDs := make(map[string]bool)
	for i := range rows {
		r := rows[i].Row
		switch r.Kind {
		case KindToolUse:
			if r.ToolUse != nil && r.ToolUse.ID != "" {
				toolUseIDs[r.ToolUse.ID] = true
			}
		case KindToolResult:
			if r.ToolResult != nil && r.ToolResult.ToolUseID != "" {
				if _, dup := resultByID[r.ToolResult.ToolUseID]; !dup {
					resultByID[r.ToolResult.ToolUseID] = r.ToolResult
				}
			}
		}
	}

	var out []string
	emit := func(tag string, tr Row, body string) {
		if tag == "" {
			tag = "?"
		}
		prefix := "[" + tag + "] "
		if opts.Timestamps && !tr.Timestamp.IsZero() {
			prefix += tr.Timestamp.UTC().Format("15:04:05") + " "
		}
		out = append(out, prefix+body)
	}

	for i := range rows {
		tag, r := rows[i].Tag, rows[i].Row
		switch r.Kind {
		case KindText:
			for _, ln := range splitProse(r.Text) {
				emit(tag, r, ln)
			}
		case KindThinking:
			if opts.HideThinking {
				continue
			}
			for _, ln := range splitProse(r.Text) {
				emit(tag, r, ln)
			}
		case KindToolUse:
			if r.ToolUse == nil {
				emit(tag, r, truncate(collapse(r.Text), maxLen))
				continue
			}
			summary := summarizeToolUse(r.ToolUse, maxLen)
			var res *ToolResult
			if r.ToolUse.ID != "" {
				res = resultByID[r.ToolUse.ID]
			}
			emit(tag, r, summary+" → "+summarizeResult(r.ToolUse.Name, res, maxLen))
		case KindToolResult:
			if r.ToolResult != nil && toolUseIDs[r.ToolResult.ToolUseID] {
				continue // already represented in its tool_use's line
			}
			// Orphan result (no tool_use anywhere) — never drop it.
			id := ""
			content := ""
			isErr := false
			if r.ToolResult != nil {
				id = shortID(r.ToolResult.ToolUseID)
				content = collapse(r.ToolResult.Content)
				isErr = r.ToolResult.IsError
			}
			status := "ok"
			if isErr {
				status = "error"
			}
			emit(tag, r, truncate(fmt.Sprintf("⟵ tool_result(orphan id=%s, %s): %s", id, status, content), maxLen))
		default: // KindRaw
			emit(tag, r, truncate(collapse(r.Text), maxLen))
		}
	}
	return out
}

// splitProse trims one trailing newline then splits, so a normal
// single-line message is one line and internal blank lines survive.
func splitProse(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

// collapse turns a multi-line value into a single line (⏎ marks the
// folded newlines so the break is visible, not silently lost).
func collapse(s string) string {
	s = strings.TrimRight(s, "\n")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", " ⏎ ")
}

// truncate clips to n runes with a VISIBLE marker including the dropped
// byte count — truncation is shown, never silent (silent-failure
// principle).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	dropped := len(string(r[n:]))
	return string(r[:n]) + fmt.Sprintf(" …[+%db]", dropped)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// summarizeToolUse renders a tool_use as `Name: <salient arg>`. Known
// tools get tailored one-liners; unknown tools fall back to the first
// scalar arg (sorted key for determinism).
func summarizeToolUse(tu *ToolUse, maxLen int) string {
	name := tu.Name
	if name == "" {
		name = "tool"
	}
	switch name {
	case "Bash":
		var in struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		c := in.Command
		if c == "" {
			c = in.Description
		}
		return "Bash: " + truncate(collapse(c), maxLen)
	case "Edit", "MultiEdit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		return fmt.Sprintf("%s: %s +%d −%d", name, base(in.FilePath),
			lineCount(in.NewString), lineCount(in.OldString))
	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		return fmt.Sprintf("Write: %s (%d lines)", base(in.FilePath), lineCount(in.Content))
	case "Read":
		var in struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		return "Read: " + base(in.FilePath)
	case "Grep":
		var in struct {
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		return "Grep: " + truncate(in.Pattern, maxLen)
	case "Glob":
		var in struct {
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		return "Glob: " + truncate(in.Pattern, maxLen)
	case "Task", "Agent":
		var in struct {
			Description  string `json:"description"`
			SubagentType string `json:"subagent_type"`
		}
		_ = json.Unmarshal(tu.Input, &in)
		d := in.Description
		if d == "" {
			d = in.SubagentType
		}
		return "Task: " + truncate(d, maxLen)
	default:
		return name + ": " + truncate(firstScalarArg(tu.Input), maxLen)
	}
}

// summarizeResult is the `→ …` half. Pending (no result yet) → "…";
// error → "error"; Bash success → first non-empty result line (or
// "ok"); other tools → "ok".
func summarizeResult(toolName string, res *ToolResult, maxLen int) string {
	if res == nil {
		return "…"
	}
	if res.IsError {
		return "error"
	}
	if toolName == "Bash" {
		if fl := firstNonEmptyLine(res.Content); fl != "" {
			return truncate(fl, maxLen)
		}
	}
	return "ok"
}

func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// firstScalarArg returns the first scalar (string/number/bool) value of
// a JSON object, by sorted key for determinism. Empty if none/!object.
func firstScalarArg(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if len(v) == 0 {
			continue
		}
		switch v[0] {
		case '{', '[': // not a scalar
			continue
		case '"':
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
		default: // number / bool / null token
			return strings.Trim(string(v), `"`)
		}
	}
	return ""
}

func base(p string) string {
	if p == "" {
		return "?"
	}
	return filepath.Base(p)
}

// lineCount is a deterministic approximation (precise diff is overkill
// for a one-line summary): 0 for empty, else newline count plus one
// unless the value is newline-terminated.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
