package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
	"github.com/jfinlinson/agent-state/internal/validate"
)

// listFields are top-level fields stored as YAML lists. Multi-line values
// for these fields are split into list items rather than block scalars.
var listFields = map[string]bool{
	"acceptance_criteria": true, "depends_on": true, "blocks": true,
	"related_issues": true, "next_actions": true, "resolution": true,
	"invariants": true, "doc_changes": true, "linked_plans": true,
}

// StdinIsPiped reports whether stdin is piped (non-interactive). Exposed
// so the cobra layer can pick stdin mode automatically when the user
// hasn't passed --stdin but redirected input.
func StdinIsPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// UpdateMode controls how Update sources its value when the caller did
// not pass a positional value argument.
type UpdateMode int

const (
	// UpdateModeValue uses the provided value verbatim — used when the
	// caller already has the value in hand (positional CLI arg).
	UpdateModeValue UpdateMode = iota
	// UpdateModeStdin reads the value from os.Stdin until EOF.
	UpdateModeStdin
	// UpdateModeEditor opens $EDITOR (falling back to stdin if unset).
	UpdateModeEditor
)

// Update writes a field on an item. The value is sourced according to
// mode: UpdateModeValue uses `value` directly, UpdateModeStdin reads
// from stdin, UpdateModeEditor launches $EDITOR seeded with the current
// value (and falls back to a stdin prompt if no editor is configured).
//
// Long-form fields (description, summary, context, notes) round-trip as
// YAML block scalars so multi-line content replaces cleanly. List fields
// (depends_on, acceptance_criteria, etc.) accept multi-line input as a
// list replacement.
func Update(s *store.Store, cfg *config.Config, id, field, value string, mode UpdateMode) int {
	// I-406: reject writes to the deprecated severity field with a
	// migration pointer. The mapping is documented in cmd/migrate-priority.
	if field == "severity" {
		fmt.Fprintln(os.Stderr,
			"update: severity is deprecated (I-406). Use priority (0-4) instead.\n"+
				"  blocking|critical|p0    -> 0\n"+
				"  high|important          -> 1\n"+
				"  medium|normal           -> 2 (default)\n"+
				"  tech-debt               -> 3 + tag tech-debt\n"+
				"  low|minor               -> 4")
		return 2
	}

	// I-494: `summary` was the legacy single-blob description field. Per
	// I-487 it is replaced by `sbar.background`. The shim emits a
	// deprecation notice and routes the write to sbar.background so
	// existing scripts and muscle memory keep working.
	//
	// Routing through SetSBARBlock (rather than just renaming `field`
	// to "sbar.background" and letting the dot-path branch take over)
	// is deliberate: SetNestedField writes inline `key: value` only,
	// which produces malformed YAML for multi-line content. Multi-line
	// summary writes were common via `--stdin` and editor mode; this
	// path keeps them working.
	if field == "summary" {
		return updateSummaryShim(s, cfg, id, value, mode)
	}

	// I-406: priority must be 0-4. Reject explicit out-of-range values
	// at the CLI boundary so a typo like `st update X priority 9` fails
	// loud rather than silently corrupting the schema. Use strconv.Atoi
	// instead of Sscanf so "2.5" or "2abc" reject (Sscanf would happily
	// store 2 and ignore the trailing characters).
	if field == "priority" {
		// Only validate when value is supplied directly (not from
		// stdin/editor — those modes resolve below).
		if mode == UpdateModeValue && value != "" {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 || n > 4 {
				fmt.Fprintf(os.Stderr, "update: priority must be int 0-4 (got %q)\n", value)
				return 2
			}
		}
	}

	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	// I-508: early-exit vocab gate for status / type. The store-layer
	// gate in Mutate would catch the same problem, but failing here
	// produces a cleaner CLI error and avoids a wasted lock + re-parse
	// round-trip on the doomed write. The same legacy-alias suggestion
	// (open → queued, etc.) ships in the message.
	if mode == UpdateModeValue && value != "" {
		if rc := preCheckVocab(item, field, value, cfg); rc != 0 {
			return rc
		}
	}

	// Block status changes on locked items (being worked on by a pipeline).
	// Use `st unlock <id>` to force-release the lock first.
	if field == "status" && store.IsLocked(cfg, id) {
		fmt.Fprintf(os.Stderr, "%s is locked (active pipeline) — use `st unlock %s` first\n", id, id)
		return 1
	}

	// Resolve the value source.
	switch mode {
	case UpdateModeStdin:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
			return 1
		}
		value = strings.TrimRight(string(data), "\n")
		if value == "" {
			fmt.Fprintln(os.Stderr, "empty input from stdin — no changes")
			return 1
		}
	case UpdateModeEditor:
		// I-493: `st update <id> sbar` opens the editor on the
		// 4-section composite buffer, parses it back, and updates all
		// four sub-fields atomically. The generic GetField path below
		// would seed the buffer with an empty string (sbar is a block
		// key, not a scalar), losing the user's existing content.
		if field == "sbar" {
			return updateSBARViaEditor(s, cfg, id, item)
		}
		current, _ := item.Doc.GetField(field)
		new, code, ok := readFromEditor(id, field, current)
		if !ok {
			return code
		}
		if new == current {
			fmt.Println("No changes.")
			return 0
		}
		value = new
	}

	var oldValue string
	mutateErr := s.Mutate(id, func(item *model.Item) error {
		switch {
		case listFields[field] && strings.Contains(value, "\n"):
			// Multi-line value = list replacement.
			// Preserve indentation — TrimSpace would destroy YAML
			// structure for continuation lines (e.g., "  command:"
			// under "- description:").
			var lines []string
			for _, line := range strings.Split(value, "\n") {
				if strings.TrimSpace(line) != "" {
					lines = append(lines, line)
				}
			}
			item.Doc.ReplaceList(field, lines)
		case strings.Contains(field, "."):
			oldValue, _ = item.Doc.GetNestedField(field)
			item.Doc.SetNestedField(field, value)
		default:
			// SetField transparently handles both single-line and
			// multi-line values: multi-line writes a YAML block
			// scalar (`key: |-`), and updates remove any prior
			// block continuation lines.
			oldValue, _ = item.Doc.GetField(field)
			item.Doc.SetField(field, value)
		}
		return nil
	})
	if mutateErr != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, mutateErr)
		return 1
	}

	changelog.Append(cfg, id, changelog.Entry{
		Op: "update", Field: field,
		OldValue: oldValue, NewValue: value,
	})

	fmt.Printf("Updated %s.%s\n", id, field)

	// Commit + push the update so it can't be silently reverted by a
	// subsequent command's pre-run GitPull or lost to a multi-agent race.
	// Best-effort: a sync failure still returns 0 because the disk state
	// is correct and a later sync will carry the commit forward.
	if err := s.GitSync(fmt.Sprintf("st update: %s.%s", id, field)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after update failed: %v\n", err)
	}
	return 0
}

// preCheckVocab is the I-508 early-exit guard for `st update` on
// status / type fields. Returns 0 when the value passes (or the field
// isn't vocab-gated); returns 2 with an error message when the value
// would be rejected by the write-time gate. The store-layer gate
// remains the authoritative check — this is a UX shortcut.
func preCheckVocab(item *model.Item, field, value string, cfg *config.Config) int {
	switch field {
	case "status":
		valid := cfg.ValidStatuses(item.Type)
		for _, v := range valid {
			if v == value {
				return 0
			}
		}
		msg := fmt.Sprintf("update: invalid status %q for type %q — valid: %s",
			value, item.Type, strings.Join(valid, ", "))
		// Hint via the same alias map the store-layer gate uses, so
		// muscle-memory `open`/`resolved` writes land an actionable error.
		if hint := validate.SuggestStatus(value); hint != "" {
			msg += fmt.Sprintf("\n  did you mean %q? (legacy alias from pre-I-433)", hint)
		}
		fmt.Fprintln(os.Stderr, msg)
		return 2
	case "type":
		if _, ok := cfg.Types[value]; ok {
			return 0
		}
		validTypes := make([]string, 0, len(cfg.Types))
		for k := range cfg.Types {
			validTypes = append(validTypes, k)
		}
		fmt.Fprintf(os.Stderr, "update: unknown type %q — valid: %s\n",
			value, strings.Join(validTypes, ", "))
		return 2
	}
	return 0
}

// readFromEditor seeds $EDITOR with the field's current value, runs it,
// and returns the user-supplied content. Falls back to a stdin prompt
// when $EDITOR is unset (useful for non-interactive agent contexts).
// The returned bool is false on a hard error; in that case `code` is
// the exit code to propagate.
func readFromEditor(id, field, current string) (string, int, bool) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		fmt.Fprintf(os.Stderr, "No $EDITOR set. Enter new value for %s (Ctrl+D to finish):\n", field)
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
			return "", 1, false
		}
		v := strings.TrimRight(string(data), "\n")
		if v == "" {
			fmt.Fprintln(os.Stderr, "empty input — no changes")
			return "", 1, false
		}
		return v, 0, true
	}

	tmpFile, err := os.CreateTemp("", fmt.Sprintf("as-edit-%s-%s-*.txt", id, field))
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp file: %v\n", err)
		return "", 1, false
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	tmpFile.WriteString(current)
	tmpFile.Close()

	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "editor failed: %v\n", err)
		return "", 1, false
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading temp file: %v\n", err)
		return "", 1, false
	}
	return strings.TrimRight(string(data), "\n"), 0, true
}

// sbarSeedBuffer renders the 4 SBAR sections as a YAML buffer the
// editor can present to the user. The wrapper `sbar:` line is
// intentionally omitted so the user does not have to keep two-space
// indentation correct on every line — the parser on the way back
// in adds it conceptually. Empty sub-fields render as `key: |-`
// followed by a single placeholder line so the user sees the section
// even when it is unset.
func sbarSeedBuffer(s model.SBAR) string {
	var b strings.Builder
	for _, sec := range []struct{ key, val string }{
		{"situation", s.Situation},
		{"background", s.Background},
		{"assessment", s.Assessment},
		{"recommendation", s.Recommendation},
	} {
		b.WriteString(sec.key)
		b.WriteString(": |-\n")
		body := sec.val
		if body == "" {
			b.WriteString("  TODO: fill in or leave blank\n")
			continue
		}
		for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			b.WriteString("  ")
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// parseSBARBuffer reverses sbarSeedBuffer. Returns an SBAR struct and
// the list of sub-keys that were missing from the input — empty list
// means all four sections were present (even if their bodies were
// empty). Order of keys is not enforced.
//
// Body indentation is stripped via the smallest indent observed
// across the section's lines — the editor seeds at 2 spaces, but
// users whose YAML mode auto-indents to 4 (or who hand-indent by
// tabs) would otherwise get leading whitespace baked into the stored
// value. Tab and space are both treated as indent.
//
// The seed buffer's "TODO: fill in or leave blank" placeholder is
// recognised as the empty marker only when it is the sole body line.
// Lines that merely START with that text are kept verbatim — silent
// data loss on user content that happens to begin with the phrase
// would be worse than a stale placeholder slipping through.
func parseSBARBuffer(buf string) (model.SBAR, []string) {
	const emptyMarker = "TODO: fill in or leave blank"
	want := map[string]bool{"situation": true, "background": true, "assessment": true, "recommendation": true}
	got := map[string]string{}
	var currentKey string
	var currentBody []string

	flush := func() {
		if currentKey == "" {
			return
		}
		// Strip trailing blank lines.
		for len(currentBody) > 0 && strings.TrimSpace(currentBody[len(currentBody)-1]) == "" {
			currentBody = currentBody[:len(currentBody)-1]
		}
		// Find the smallest leading-whitespace prefix across non-blank
		// lines and strip exactly that much from each. Preserves
		// internal relative indentation (multi-paragraph bodies, code
		// fences, etc.).
		minIndent := -1
		for _, l := range currentBody {
			if strings.TrimSpace(l) == "" {
				continue
			}
			n := 0
			for n < len(l) && (l[n] == ' ' || l[n] == '\t') {
				n++
			}
			if minIndent == -1 || n < minIndent {
				minIndent = n
			}
		}
		stripped := make([]string, len(currentBody))
		for i, l := range currentBody {
			if minIndent > 0 && len(l) >= minIndent {
				stripped[i] = l[minIndent:]
			} else {
				stripped[i] = l
			}
		}
		body := strings.Join(stripped, "\n")
		// Treat the unedited seed as empty — only when the entire
		// body is the literal placeholder line.
		if strings.TrimSpace(body) == emptyMarker {
			body = ""
		}
		got[currentKey] = body
		currentKey = ""
		currentBody = nil
	}

	for _, raw := range strings.Split(buf, "\n") {
		// Recognise a section header: `<key>: |-` (any block scalar
		// indicator) at column 0 where <key> is one of the four
		// SBAR sub-keys. Anything else is body content.
		trimmed := strings.TrimRight(raw, " \t")
		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
			colon := strings.Index(trimmed, ":")
			if colon > 0 {
				key := trimmed[:colon]
				rest := strings.TrimSpace(trimmed[colon+1:])
				if want[key] && (rest == "|-" || rest == "|" || rest == "" || rest == ">" || rest == ">-") {
					flush()
					currentKey = key
					continue
				}
			}
		}
		if currentKey == "" {
			continue
		}
		currentBody = append(currentBody, raw)
	}
	flush()

	var missing []string
	for k := range want {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	return model.SBAR{
		Situation:      got["situation"],
		Background:     got["background"],
		Assessment:     got["assessment"],
		Recommendation: got["recommendation"],
	}, missing
}

// updateSBARViaEditor implements the I-493 editor flow for the sbar
// composite field. Returns the CLI exit code.
func updateSBARViaEditor(s *store.Store, cfg *config.Config, id string, item *model.Item) int {
	seed := sbarSeedBuffer(item.SBAR)
	edited, code, ok := readFromEditor(id, "sbar", seed)
	if !ok {
		return code
	}
	if edited == strings.TrimRight(seed, "\n") {
		fmt.Println("No changes.")
		return 0
	}
	newSBAR, missing := parseSBARBuffer(edited)
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr,
			"update: SBAR buffer is missing required section(s): %s\n"+
				"  Each of situation/background/assessment/recommendation must be present, even if blank.\n",
			strings.Join(missing, ", "))
		return 2
	}
	oldRendered := sbarSeedBuffer(item.SBAR)
	mutateErr := s.Mutate(id, func(it *model.Item) error {
		it.SBAR = newSBAR
		it.Doc.SetSBARBlock(newSBAR)
		return nil
	})
	if mutateErr != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, mutateErr)
		return 1
	}
	changelog.Append(cfg, id, changelog.Entry{
		Op: "update", Field: "sbar",
		OldValue: truncateForChangelog(oldRendered),
		NewValue: truncateForChangelog(sbarSeedBuffer(newSBAR)),
	})
	fmt.Printf("Updated %s.sbar\n", id)
	if err := s.GitSync(fmt.Sprintf("st update: %s.sbar", id)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after update failed: %v\n", err)
	}
	return 0
}

// truncateForChangelog keeps the changelog readable when SBAR bodies
// are paragraphs — the full content is in the file's git history; the
// changelog just needs a recognizable signature.
func truncateForChangelog(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// updateSummaryShim implements the I-494 backwards-compat path for
// `st update <id> summary <value>`. Resolves the value from the
// caller's mode (positional / stdin / editor), then writes it under
// sbar.background via SetSBARBlock so multi-line content lands as a
// proper YAML block scalar instead of malformed inline output.
//
// The deprecation notice is emitted only AFTER value resolution
// succeeds — printing it first would mislead the user when stdin is
// empty or the editor errors, implying the routing happened when it
// did not.
//
// `oldValue` is captured INSIDE the Mutate closure (under flock) per
// the T-304 purity rule, so a concurrent peer-agent write does not
// produce a stale changelog OldValue.
func updateSummaryShim(s *store.Store, cfg *config.Config, id, value string, mode UpdateMode) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	switch mode {
	case UpdateModeStdin:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
			return 1
		}
		value = strings.TrimRight(string(data), "\n")
		if value == "" {
			fmt.Fprintln(os.Stderr, "empty input from stdin — no changes")
			return 1
		}
	case UpdateModeEditor:
		newVal, code, ok := readFromEditor(id, "sbar.background", item.SBAR.Background)
		if !ok {
			return code
		}
		value = newVal
	}

	if value == item.SBAR.Background {
		fmt.Println("No changes.")
		return 0
	}

	fmt.Fprintln(os.Stderr,
		"update: summary is deprecated (I-487). Routing content to sbar.background.\n"+
			"  Use: st update <id> sbar.background \"<text>\"\n"+
			"  Or:  st update <id> sbar  (opens editor on the 4-section block)")

	var oldValue string
	mutateErr := s.Mutate(id, func(it *model.Item) error {
		oldValue = it.SBAR.Background
		it.SBAR.Background = value
		it.Doc.SetSBARBlock(it.SBAR)
		return nil
	})
	if mutateErr != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, mutateErr)
		return 1
	}
	changelog.Append(cfg, id, changelog.Entry{
		Op: "update", Field: "sbar.background",
		OldValue: truncateForChangelog(oldValue),
		NewValue: truncateForChangelog(value),
	})
	fmt.Printf("Updated %s.sbar.background\n", id)
	if err := s.GitSync(fmt.Sprintf("st update: %s.sbar.background", id)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after update failed: %v\n", err)
	}
	return 0
}
