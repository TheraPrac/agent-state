// Package parse provides a lossless line-by-line parser for agent-state
// markdown files. It preserves exact formatting for roundtrip fidelity.
package parse

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/model"
)

// state tracks the parser's current position in nested structures.
type state int

const (
	stateTop    state = iota // top-level key:value
	stateBlock               // inside a | or > multiline block
	stateList                // inside a list (lines starting with -)
	stateNested              // inside a nested map (indented key:value)
)

// File parses an agent-state markdown file and returns both a typed Item
// and a ParsedDocument for lossless roundtrip.
func File(path string) (*model.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	doc := &model.ParsedDocument{}
	item := &model.Item{
		WorkTracking:    make(map[string]interface{}),
		Delivery:        make(map[string]interface{}),
		TestingEvidence: make(map[string]interface{}),
		TimeTracking:    make(map[string]interface{}),
		Manifest:        make(map[string]interface{}),
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle large files

	var (
		currentKey     string // current top-level key being parsed
		currentBlock   string // accumulating multiline block content
		blockIndent    int    // expected indent for block continuation
		inBlock        bool
		inFence        bool   // inside a markdown ``` fenced code block
		nestKey        string // current nested parent key (e.g., "work_tracking")
		currentList    []string
		inListOfMaps   bool // true when parsing list of objects (testing_evidence.runs)
		listOfMaps     []map[string]string
		currentMapItem map[string]string
	)

	for scanner.Scan() {
		raw := scanner.Text()
		line := model.Line{Raw: raw}

		// Classify the line
		trimmed := strings.TrimSpace(raw)
		line.IsEmpty = trimmed == ""
		line.Indent = len(raw) - len(strings.TrimLeft(raw, " \t"))

		// Extract inline comment (but not in multiline blocks or markdown body)
		if !inBlock {
			if ci := findInlineComment(trimmed); ci >= 0 {
				line.Comment = strings.TrimSpace(trimmed[ci+1:])
				trimmed = strings.TrimSpace(trimmed[:ci])
			}
		}

		// Markdown fenced code block awareness. Inside a ``` fence, lines
		// are opaque body content — never parsed as YAML, even if they
		// happen to look like `key: value`. This prevents description-body
		// content like ```yaml\ntype: warning\n``` from corrupting frontmatter.
		if !inBlock {
			isFence := strings.HasPrefix(trimmed, "```")
			if inFence || isFence {
				line.IsBlock = true
				line.BlockKey = currentKey
				doc.Lines = append(doc.Lines, line)
				if isFence {
					inFence = !inFence
				}
				continue
			}
		}

		// Handle multiline block continuation
		if inBlock {
			if line.IsEmpty || line.Indent >= blockIndent {
				line.IsBlock = true
				line.BlockKey = currentKey
				if line.IsEmpty {
					currentBlock += "\n"
				} else {
					if currentBlock != "" {
						currentBlock += "\n"
					}
					// Trim the block indent
					content := raw
					if len(content) >= blockIndent {
						content = content[blockIndent:]
					}
					currentBlock += content
				}
				doc.Lines = append(doc.Lines, line)
				continue
			}
			// Block ended — store the accumulated content
			storeMultiline(item, currentKey, nestKey, currentBlock)
			inBlock = false
			currentBlock = ""

			// Re-run fence detection: the line that ended the block may
			// itself be a fence opener (e.g. SBAR `recommendation: |-`
			// dedenting onto a markdown ``` fence below). The earlier
			// fence check at the top of the loop is gated on `!inBlock`,
			// so it skipped this line on the way in. Without re-checking,
			// the opening fence is missed and the closing fence is later
			// misread as an opener, which marks every subsequent line
			// IsBlock and prevents top-level fields after the markdown
			// body (`blocks:`, `last_touched_by:`) from being parsed —
			// see I-562 Bug B / TestParseBlockEndingOnFenceOpener.
			//
			// `inFence` is guaranteed false here: the top-of-loop fence
			// handler `continue`s every line while `inFence` is true,
			// which prevents the block-detection branch from running and
			// thus prevents `inBlock` from becoming true inside a fence.
			// So the two states are mutually exclusive, and we can set
			// inFence directly rather than toggle.
			if strings.HasPrefix(trimmed, "```") {
				line.IsBlock = true
				line.BlockKey = currentKey
				doc.Lines = append(doc.Lines, line)
				inFence = true
				continue
			}
		}

		// Handle markdown body separator (---)
		if trimmed == "---" {
			// Everything after this is markdown body — store remaining lines as-is
			doc.Lines = append(doc.Lines, line)
			for scanner.Scan() {
				bodyRaw := scanner.Text()
				doc.Lines = append(doc.Lines, model.Line{Raw: bodyRaw})
			}
			break
		}

		// Empty line — may end a list or nested section
		if line.IsEmpty {
			if len(currentList) > 0 && currentKey != "" {
				storeList(item, currentKey, nestKey, currentList)
				currentList = nil
			}
			if inListOfMaps {
				if currentMapItem != nil {
					listOfMaps = append(listOfMaps, currentMapItem)
					currentMapItem = nil
				}
				storeListOfMaps(item, currentKey, nestKey, listOfMaps)
				inListOfMaps = false
				listOfMaps = nil
			}
			doc.Lines = append(doc.Lines, line)
			continue
		}

		// Detect list items at various indent levels
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			line.IsList = true
			listContent := strings.TrimPrefix(trimmed, "- ")
			listContent = strings.TrimPrefix(listContent, " ")

			// Empty list markers: "- []" or "- [[]]"
			if listContent == "[]" || listContent == "[[]]" {
				doc.Lines = append(doc.Lines, line)
				continue
			}

			// List of maps: "- command: ..." pattern
			if line.Indent > 0 && strings.Contains(listContent, ": ") && !strings.HasPrefix(listContent, "\"") && !strings.HasPrefix(listContent, "http") {
				if inListOfMaps {
					// New map item in the list
					if currentMapItem != nil {
						listOfMaps = append(listOfMaps, currentMapItem)
					}
					k, v := splitKV(listContent)
					currentMapItem = map[string]string{k: v}
				} else {
					// First map item
					inListOfMaps = true
					k, v := splitKV(listContent)
					currentMapItem = map[string]string{k: v}
				}
				doc.Lines = append(doc.Lines, line)
				continue
			}

			// Regular list item — strip balanced wrapping quotes only.
			listContent = unquote(listContent)
			currentList = append(currentList, listContent)
			doc.Lines = append(doc.Lines, line)
			continue
		}

		// Continuation of a list-of-maps item (indented key:value under a list item)
		if inListOfMaps && line.Indent > 2 && strings.Contains(trimmed, ": ") && currentMapItem != nil {
			k, v := splitKV(trimmed)
			currentMapItem[k] = v
			doc.Lines = append(doc.Lines, line)
			continue
		}

		// Key:value lines
		if colonIdx := strings.Index(trimmed, ":"); colonIdx >= 0 {
			key := trimmed[:colonIdx]
			val := ""
			if colonIdx+1 < len(trimmed) {
				val = strings.TrimSpace(trimmed[colonIdx+1:])
			}

			// Strip inline comment from value
			if line.Comment != "" && val != "" {
				if ci := findInlineComment(val); ci >= 0 {
					val = strings.TrimSpace(val[:ci])
				}
			}

			line.Key = key
			line.Value = val

			if line.Indent == 0 {
				// Flush any pending list
				if len(currentList) > 0 && currentKey != "" {
					storeList(item, currentKey, nestKey, currentList)
					currentList = nil
				}
				if inListOfMaps {
					if currentMapItem != nil {
						listOfMaps = append(listOfMaps, currentMapItem)
						currentMapItem = nil
					}
					storeListOfMaps(item, currentKey, nestKey, listOfMaps)
					inListOfMaps = false
					listOfMaps = nil
				}
				currentKey = key
				nestKey = ""

				// Check for multiline block indicator
				if val == "|" || val == ">" || val == "|+" || val == "|-" {
					inBlock = true
					blockIndent = 2 // standard 2-space indent for blocks
					currentBlock = ""
					doc.Lines = append(doc.Lines, line)
					continue
				}

				// I-691: a non-empty scalar under a known LIST key is a
				// corrupted single-line write (`next_actions: text` instead
				// of `next_actions:` + `- text`). storeScalar has no case
				// for list keys and would silently drop it. SEED it as the
				// first element of currentList rather than storeList-ing it
				// immediately: the unified list flush (next indent-0 key or
				// EOF) then owns it. The realistic corrupt form is
				// `key: text` + the `- []` template marker (which the
				// empty-marker branch skips, leaving the seed intact → heals
				// to [text]); a pathological `key: text` + real `- a`/`- b`
				// lines now MERGES (text as element 0) instead of the flush
				// silently overwriting the coerced value — never a silent
				// drop (operator silent-failure principle). A proper
				// `key:` + `- a` list has val == "" → falls to storeScalar
				// (inert for list keys) and the normal accumulate path runs.
				if isListKey(key) && !isEmptyListScalar(val) {
					currentList = []string{unquote(val)}
				} else {
					// Scalar store. For a well-formed list key the val
					// here is an empty/sentinel (`key:` then `- a` on
					// following lines, or `key: []`): storeScalar has no
					// case for list keys so it is inert for them, and the
					// real items arrive via the `- ` branch and flush
					// through storeList. For genuine scalar keys it stores
					// normally.
					storeScalar(item, key, val)
				}
			} else {
				// Nested key:value
				if nestKey == "" {
					nestKey = currentKey
				}
				line.BlockKey = nestKey

				// Check for nested multiline block. Same indicator
				// vocabulary as the top-level branch: `|`, `>`, `|+`,
				// `|-`. The strip/keep/keep-trailing semantics only
				// matter for serialization; the parser stores content
				// verbatim and storeMultiline trims trailing newlines.
				if val == "|" || val == ">" || val == "|+" || val == "|-" {
					inBlock = true
					blockIndent = line.Indent + 2
					currentBlock = ""
					currentKey = key
					doc.Lines = append(doc.Lines, line)
					continue
				}

				storeNestedScalar(item, nestKey, key, val)
			}
		}

		doc.Lines = append(doc.Lines, line)
	}

	// Flush any remaining state
	if inBlock {
		storeMultiline(item, currentKey, nestKey, currentBlock)
	}
	if len(currentList) > 0 && currentKey != "" {
		storeList(item, currentKey, nestKey, currentList)
	}
	if inListOfMaps {
		if currentMapItem != nil {
			listOfMaps = append(listOfMaps, currentMapItem)
		}
		storeListOfMaps(item, currentKey, nestKey, listOfMaps)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	item.Doc = doc
	return item, nil
}

// findInlineComment finds the position of an inline comment marker " #"
// that is NOT inside a quoted string and NOT a URL fragment.
func findInlineComment(s string) int {
	inSingleQuote := false
	inDoubleQuote := false
	for i := 0; i < len(s)-1; i++ {
		switch s[i] {
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case ' ':
			if !inSingleQuote && !inDoubleQuote && i+1 < len(s) && s[i+1] == '#' {
				return i
			}
		}
	}
	return -1
}

func splitKV(s string) (string, string) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return s, ""
	}
	key := strings.TrimSpace(s[:idx])
	val := strings.TrimSpace(s[idx+1:])
	val = unquote(val)
	return key, val
}

// unquote strips balanced wrapping quotes from a value. Unlike
// strings.Trim(val, `"'`), it only removes quotes that form a matched
// pair around the entire value, so values containing unbalanced or
// internal quotes (e.g. shell commands with `grep -q 'foo'`) survive intact.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func storeScalar(item *model.Item, key, val string) {
	// Normalize null
	if val == "null" || val == "~" || val == "" {
		val = ""
	}

	// Strip quotes from value
	val = unquote(val)

	switch key {
	case "id":
		item.ID = val
	case "type":
		item.Type = val
	case "status":
		item.Status = val
	case "title":
		item.Title = val
	case "created":
		item.Created = parseTime(val)
	case "last_touched":
		item.LastTouched = parseTime(val)
	case "completed":
		if val != "" {
			t := parseTime(val)
			item.Completed = &t
		}
	case "priority":
		if val != "" {
			if n, err := strconv.Atoi(val); err == nil {
				item.Priority = &n
			}
			// String values (alpha-critical, etc.) are left unparsed in Priority;
			// migration reads them from Doc.GetField("priority") for conversion.
		}
	case "severity":
		// I-406: severity is deprecated. Parser still recognizes it so
		// any legacy file slipping through round-trips without data
		// loss during the deprecation window. Remove this case once
		// Item.Severity is removed and a sweep confirms no agent-state
		// file still carries severity.
		item.Severity = val
	case "category":
		item.Category = val
	case "repo":
		item.Repo = val
	case "assigned_to":
		item.AssignedTo = val
	case "last_touched_by":
		item.LastTouchedBy = val
	case "epic":
		item.Epic = val
	case "sprint":
		item.Sprint = val
	case "arc":
		item.Arc = val
	case "scope_class":
		item.ScopeClass = val
	case "claimed_by":
		item.ClaimedBy = val
	case "claimed_at":
		item.ClaimedAt = val
	case "plan_approved":
		item.PlanApproved = val == "true"
	case "plan_approved_at":
		item.PlanApprovedAt = val
	case "plan_approved_by":
		item.PlanApprovedBy = val
	case "parallel_group":
		// Legacy field — store but don't surface
	case "weight":
		if val != "" {
			if n, err := strconv.Atoi(val); err == nil {
				item.Weight = &n
			}
		}
	case "success_criterion":
		item.SuccessCriterion = val
	case "dropped_reason":
		item.DroppedReason = val
	}
}

func storeNestedScalar(item *model.Item, parent, key, val string) {
	if val == "null" || val == "~" {
		val = ""
	}
	val = unquote(val)

	switch parent {
	case "work_tracking":
		item.WorkTracking[key] = val
	case "delivery":
		item.Delivery[key] = val
	case "testing_evidence":
		if item.TestingEvidence[key] == nil {
			item.TestingEvidence[key] = val
		}
	case "required_suites":
		if item.TestingEvidence["required_suites"] == nil {
			item.TestingEvidence["required_suites"] = make(map[string]interface{})
		}
		if m, ok := item.TestingEvidence["required_suites"].(map[string]interface{}); ok {
			m[key] = val
		}
	case "scope_suites":
		if item.TestingEvidence["scope_suites"] == nil {
			item.TestingEvidence["scope_suites"] = make(map[string]interface{})
		}
		if m, ok := item.TestingEvidence["scope_suites"].(map[string]interface{}); ok {
			m[key] = val
		}
	case "time_tracking":
		item.TimeTracking[key] = val
	case "manifest":
		item.Manifest[key] = val
	case "sbar":
		// I-487: SBAR fields are usually multiline, but a one-line
		// inline value (e.g. `situation: short symptom`) is also
		// supported and routes here.
		setSBARField(item, key, val)
	}
}

func storeMultiline(item *model.Item, key, nestKey, content string) {
	// Trim trailing newlines
	content = strings.TrimRight(content, "\n")

	switch key {
	case "summary":
		item.Summary = content
	case "context":
		item.Context = content
	}
	// I-487: SBAR multiline blocks (situation/background/assessment/
	// recommendation) live under nestKey="sbar" with multi-line
	// values. Route those into the typed SBAR struct so callers can
	// access them without re-parsing the raw doc.
	if nestKey == "sbar" {
		setSBARField(item, key, content)
		return
	}
	// Other multiline fields stored in the nested maps if applicable
	if nestKey != "" {
		storeNestedScalar(item, nestKey, key, content)
	}
}

// setSBARField writes a single SBAR sub-field by name. Unknown keys
// are silently dropped — the typed struct only carries the four
// canonical sections, but the underlying doc retains arbitrary
// children for round-trip fidelity.
func setSBARField(item *model.Item, key, value string) {
	switch key {
	case "situation":
		item.SBAR.Situation = value
	case "background":
		item.SBAR.Background = value
	case "assessment":
		item.SBAR.Assessment = value
	case "recommendation":
		item.SBAR.Recommendation = value
	}
}

// isEmptyListScalar reports whether an inline scalar value for a list key
// is an empty/sentinel marker (`[]`, `[[]]`, null, ~, or empty/whitespace)
// rather than a real single value. Coercing one of these to a list would
// resurrect the empty-list sentinel as a literal element (the regression
// TestParseEmptyListVariants/bare-brackets caught). I-691.
func isEmptyListScalar(v string) bool {
	switch strings.TrimSpace(v) {
	case "", "[]", "[[]]", "null", "~":
		return true
	}
	return false
}

// isListKey reports whether key is stored as a list. It MUST equal
// storeList's switch below (every key storeList recognizes is one whose
// scalar form must be coerced — I-691 — rather than dropped by
// storeScalar). It relates to command.listFields (the WRITE side) by
// exactly: isListKey == command.listFields ∪ {"tests_written"}. The extra
// key, `tests_written`, is a list on READ (storeList routes it into the
// testing_evidence map, so a stray top-level scalar still self-heals here)
// but is intentionally absent from command.listFields because it is
// nested under `testing_evidence:` and written by a dedicated nested
// appender — never via the top-level ReplaceList path. Keep the three in
// sync under that rule.
func isListKey(key string) bool {
	switch key {
	case "tags", "depends_on", "blocks", "related_issues",
		"acceptance_criteria", "next_actions", "resolution",
		"invariants", "doc_changes", "sessions", "linked_plans",
		"tests_written", "goals", "observations":
		return true
	}
	return false
}

func storeList(item *model.Item, key, nestKey string, list []string) {
	switch key {
	case "tags":
		item.Tags = list
	case "depends_on":
		item.DependsOn = list
	case "blocks":
		item.Blocks = list
	case "related_issues":
		item.RelatedIssues = list
	case "acceptance_criteria":
		item.AcceptanceCriteria = list
	case "next_actions":
		item.NextActions = list
	case "resolution":
		item.Resolution = list
	case "invariants":
		item.Invariants = list
	case "doc_changes":
		item.DocChanges = list
	case "sessions":
		item.Sessions = list
	case "observations":
		item.Observations = list
	case "linked_plans":
		item.LinkedPlans = list
	case "goals":
		item.Goals = list
	case "tests_written":
		if item.TestingEvidence["tests_written"] == nil {
			item.TestingEvidence["tests_written"] = list
		}
	}
}

func storeListOfMaps(item *model.Item, key, nestKey string, maps []map[string]string) {
	switch nestKey {
	case "testing_evidence":
		if key == "runs" || nestKey == "testing_evidence" {
			item.TestingEvidence["runs"] = maps
		}
	}
}

func parseTime(s string) time.Time {
	// Try common formats
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
