package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"time"
)

// Kind classifies a Row by its semantic content, which is what the Phase 2
// renderer discriminates on — not the JSONL top-level "type". A single
// assistant JSONL line (text + N tool_use blocks) expands to one Row per
// block so the renderer can correlate tool_use↔tool_result by id and keep
// reasoning prose distinct from tool spam (contract §8.1).
type Kind string

const (
	KindText       Kind = "text"        // assistant/user prose
	KindThinking   Kind = "thinking"    // extended-thinking block (reasoning prose)
	KindToolUse    Kind = "tool_use"    // a tool invocation
	KindToolResult Kind = "tool_result" // the matching result
	// KindRaw is the graceful-degradation bucket: any line that is not
	// JSON, or is JSON we don't decompose (metadata rows, future schema).
	// Its Text is the original line verbatim. Never crash, never silently
	// drop a line — surface it raw (operator silent-failure principle;
	// contract §13 finding 1: trust the substrate, render everything).
	KindRaw Kind = "raw"
)

// ToolUse is the typed projection of a tool_use content block. Input is
// left as RawMessage; per-tool one-line summarization is Phase 2's job.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the typed projection of a tool_result content block.
// ToolUseID correlates back to a ToolUse.ID.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"-"` // flattened (string content, or joined text parts)
	IsError   bool   `json:"is_error"`
}

// Row is one rendered-able unit. A JSONL line decomposes into 0..N Rows.
// Raw always holds the original line bytes so no information is lost and
// later phases can extract more without a schema change here.
type Row struct {
	Kind       Kind
	Role       string // message.role: "assistant" | "user" | ""
	Timestamp  time.Time
	Text       string // KindText/KindThinking/KindRaw payload
	ToolUse    *ToolUse
	ToolResult *ToolResult
	Raw        json.RawMessage
}

// --- on-disk schema (only the fields we need; everything else ignored) ---

type wireContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	// Content is either a string or an array of blocks (tool_result).
	Content json.RawMessage `json:"content"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is either a plain string or an array of wireContentBlock.
	Content json.RawMessage `json:"content"`
}

type wireRow struct {
	Type      string       `json:"type"`
	Timestamp string       `json:"timestamp"`
	Message   *wireMessage `json:"message"`
}

// ParseLine turns one JSONL line into 0..N Rows. It never returns an
// error and never panics: a blank line yields nil (whitespace is not
// content), anything unparseable or unrecognized yields a single KindRaw
// row preserving the line verbatim.
func ParseLine(line []byte) []Row {
	if len(trimSpace(line)) == 0 {
		return nil
	}
	var wr wireRow
	if err := json.Unmarshal(line, &wr); err != nil {
		return []Row{rawRow(line)}
	}
	ts := parseTS(wr.Timestamp)

	// Only assistant/user message rows decompose. Everything else
	// (last-prompt, permission-mode, attachment, ai-title,
	// file-history-snapshot, future types) is preserved raw rather than
	// dropped — visible, not swallowed.
	if (wr.Type != "assistant" && wr.Type != "user") || wr.Message == nil {
		r := rawRow(line)
		r.Timestamp = ts
		return []Row{r}
	}
	role := wr.Message.Role

	// content: plain string → one text row.
	var s string
	if err := json.Unmarshal(wr.Message.Content, &s); err == nil {
		if s == "" {
			r := rawRow(line)
			r.Timestamp, r.Role = ts, role
			return []Row{r}
		}
		return []Row{{Kind: KindText, Role: role, Timestamp: ts, Text: s, Raw: rawCopy(line)}}
	}

	// content: array of blocks. Decode block-by-block as RawMessage
	// first so an unknown (or individually malformed) block can be
	// surfaced verbatim — never the whole enclosing line, never dropped.
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(wr.Message.Content, &rawBlocks); err != nil || len(rawBlocks) == 0 {
		r := rawRow(line)
		r.Timestamp, r.Role = ts, role
		return []Row{r}
	}
	rawc := rawCopy(line)
	var rows []Row
	for _, rb := range rawBlocks {
		var b wireContentBlock
		if err := json.Unmarshal(rb, &b); err != nil {
			// A block we can't even shape: surface the block's own
			// bytes raw rather than dropping the turn's information.
			rows = append(rows, Row{Kind: KindRaw, Role: role, Timestamp: ts, Text: string(rb), Raw: rawc})
			continue
		}
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			rows = append(rows, Row{Kind: KindText, Role: role, Timestamp: ts, Text: b.Text, Raw: rawc})
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			rows = append(rows, Row{Kind: KindThinking, Role: role, Timestamp: ts, Text: b.Thinking, Raw: rawc})
		case "tool_use":
			rows = append(rows, Row{
				Kind: KindToolUse, Role: role, Timestamp: ts, Raw: rawc,
				ToolUse: &ToolUse{ID: b.ID, Name: b.Name, Input: b.Input},
			})
		case "tool_result":
			rows = append(rows, Row{
				Kind: KindToolResult, Role: role, Timestamp: ts, Raw: rawc,
				ToolResult: &ToolResult{
					ToolUseID: b.ToolUseID,
					Content:   flattenResultContent(b.Content),
					IsError:   b.IsError,
				},
			})
		default:
			// Unknown block type (e.g. image, future kinds): surface
			// THIS block's bytes raw — not the whole multi-block line —
			// rather than dropping the turn's information.
			rows = append(rows, Row{Kind: KindRaw, Role: role, Timestamp: ts, Text: string(rb), Raw: rawc})
		}
	}
	if len(rows) == 0 {
		r := rawRow(line)
		r.Timestamp, r.Role = ts, role
		return []Row{r}
	}
	return rows
}

// ReadFile reads an entire session JSONL into Rows. The only error it
// returns is a failure to open the file; malformed lines degrade to
// KindRaw rows (they are data about the session, not a reason to abort).
func ReadFile(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadRows(f)
}

// ReadRows reads JSONL rows from r. Malformed lines degrade to KindRaw
// (the same per-line contract as ParseLine); unlike ParseLine it can
// return an error, but only for a read/scan failure (I/O, or a line
// longer than the buffer cap) — never for a JSON parse failure. The 64MB
// max-token cap is deliberately larger than cmd/reconcile-tokens'
// jsonlUsage (16MB): a giant tool_result line must render, not abort.
// The two readers are intentionally not coupled — jsonlUsage keeps its
// 16MB cap for its narrower usage-summing scope.
func ReadRows(r io.Reader) ([]Row, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var out []Row
	for sc.Scan() {
		// Scanner reuses its buffer; ParseLine copies what it retains.
		out = append(out, ParseLine(sc.Bytes())...)
	}
	return out, sc.Err()
}

// --- helpers ---

func rawRow(line []byte) Row {
	return Row{Kind: KindRaw, Text: string(line), Raw: rawCopy(line)}
}

// rawCopy returns a json.RawMessage that owns its bytes (bufio.Scanner
// reuses the slice it returns from Bytes(); retaining it without a copy
// would corrupt earlier rows).
func rawCopy(line []byte) json.RawMessage {
	b := make([]byte, len(line))
	copy(b, line)
	return b
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

// parseTS parses an RFC3339 timestamp. time.RFC3339Nano also accepts the
// no-fractional-seconds form, so a single parse covers both Claude Code's
// millisecond timestamps and any second-precision ones. Zero time on
// absent/unparseable — never an error; an undated row still renders,
// just unordered relative to dated ones.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// flattenResultContent normalizes tool_result.content, which Claude Code
// emits either as a plain string or as an array of {type:"text",text:..}
// (and occasionally non-text blocks). Non-text blocks in a mixed array
// are surfaced as a visible "[<type> block]" marker — never silently
// dropped (silent-failure principle). Content we can't parse as either
// shape is preserved as its raw JSON so nothing is lost.
func flattenResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil && len(blocks) > 0 {
		var parts []string
		for _, b := range blocks {
			switch {
			case b.Type == "text" && b.Text != "":
				parts = append(parts, b.Text)
			case b.Type == "text":
				// empty text block — no information to surface
			default:
				// non-text block (image, etc.): a visible marker so
				// it is never silently dropped from a mixed array.
				parts = append(parts, "["+b.Type+" block]")
			}
		}
		if len(parts) > 0 {
			return joinLines(parts)
		}
	}
	return string(raw)
}

func joinLines(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += "\n" + p
	}
	return out
}
