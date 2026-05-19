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

// listFields are the TOP-LEVEL fields stored as YAML lists. A value for
// one of these is written as list items via ReplaceList, never a scalar —
// a scalar under a list key is silently dropped by the parser's
// storeScalar (I-691).
//
// Relationship to the parser's list-key set (parse.isListKey /
// parse.storeList): listFields is exactly that set MINUS `tests_written`.
// `tests_written` is a LIST on read (the parser routes it into the
// testing_evidence map) but it is NOT a top-level key — it lives nested
// under `testing_evidence:` and its writer is the dedicated nested
// appender (Doc.AppendToNestedList("testing_evidence","tests_written",…)
// in pr.go). Putting it here would make ReplaceList (which only matches
// indent-0 keys) append a SECOND, orphaned top-level `tests_written:`
// block while leaving the nested one stale — structural corruption. So
// the asymmetry is deliberate: every key here MUST be in parse.isListKey,
// but parse.isListKey has the one extra nested key. Keep them in sync
// under that rule.
var listFields = map[string]bool{
	"acceptance_criteria": true, "depends_on": true, "blocks": true,
	"related_issues": true, "next_actions": true, "resolution": true,
	"invariants": true, "doc_changes": true, "linked_plans": true,
	"tags": true, "sessions": true,
}

// listItemRaw formats v as a canonical YAML list-item line ("- v", or
// "- \"v\"" when v needs quoting). The quoting predicate mirrors the
// migrate builder's needsQuoting (`:` `+"`"+` #{}[]` or a leading quote) so a
// single-line `st update` write and any later builder re-render of the same
// item are byte-identical — preventing spurious migrate/round-trip churn.
// I-691.
func listItemRaw(v string) string {
	if strings.ContainsAny(v, ":`#{}[]") || strings.HasPrefix(v, "'") || strings.HasPrefix(v, "\"") {
		// Wrap exactly as migrate.emitList does (raw `"%s"`, no Go
		// escaping) so the two writers produce identical bytes; the
		// parser's unquote only strips balanced wrapping quotes.
		return fmt.Sprintf(`- "%s"`, v)
	}
	return "- " + v
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

	// I-670: `sbar` is a 4-section composite, not a scalar. The editor
	// path is handled above (updateSBARViaEditor). The stdin and
	// positional-value paths previously fell through to the default
	// SetField branch below, which writes the whole input as a `sbar: |-`
	// block scalar — silently flattening the mapping to a string and
	// leaving no autonomous recovery (the dotted-path workaround cannot
	// un-stringify a scalar). Parse the input as the 4-section buffer and
	// write via SetSBARBlock instead. Because SetSBARBlock is heal-capable
	// (rebuilds over col-0 garbage / duplicate headers), routing through
	// it ALSO repairs an already-corrupted scalar sbar — so this same
	// command is the agent-runnable recovery path.
	if field == "sbar" {
		newSBAR, missing := parseSBARBuffer(value)
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr,
				"update: sbar input is missing required section(s): %s\n"+
					"  sbar is a 4-section composite — pipe all four, each a\n"+
					"  block-scalar header followed by an indented body:\n\n"+
					"    situation: |-\n      ...\n    background: |-\n      ...\n"+
					"    assessment: |-\n      ...\n    recommendation: |-\n      ...\n\n"+
					"  (this same command also repairs a scalar-corrupted sbar.)\n",
				strings.Join(missing, ", "))
			return 2
		}
		return commitSBAR(s, cfg, id, item, newSBAR)
	}

	// I-670: a dotted `sbar.<section>` write only works when sbar is a
	// proper 4-key mapping. If sbar was scalar-corrupted (e.g. by a
	// pre-fix `st update <id> sbar --stdin`), SetNestedField finds the
	// `sbar:` parent but no child keys and inserts the new line *inside*
	// the block scalar, where it silently vanishes. Refuse loudly and
	// point at the autonomous recovery command rather than no-op.
	if strings.HasPrefix(field, "sbar.") && item.Doc.SBARIsScalarCorrupted() {
		fmt.Fprintf(os.Stderr,
			"update: %s sbar is a corrupted scalar (not a 4-section mapping); a\n"+
				"  dotted `%s` write would silently vanish inside the block scalar.\n"+
				"  Recover autonomously by rewriting the whole block:\n"+
				"    st update %s sbar --stdin < <four-section-buffer>\n"+
				"  (situation/background/assessment/recommendation, each `key: |-`\n"+
				"   followed by an indented body).\n",
			id, field, id)
		return 2
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
		case listFields[field]:
			// I-691: a SINGLE-LINE value for a top-level list field (no
			// newline, so the multi-line case above did not match) is ONE
			// list item, not a scalar. Written as a scalar (`key: value`)
			// it is silently dropped on reload — the parser's storeScalar
			// has no case for any list key. This branch covers every
			// top-level list field (listFields; the only parser list key
			// excluded is the nested `tests_written` — see listFields'
			// doc); emit the canonical list-item line, quoted exactly as
			// the migrate builder's emitList would, so update.go and a
			// later builder re-render agree byte-for-byte.
			//
			// oldValue via GetField is "" for a well-formed existing list
			// (the header line carries no inline value) — same limitation
			// as the sibling scalar branch; git history is the source of
			// truth for prior list contents (accepted, PR #107 #5).
			oldValue, _ = item.Doc.GetField(field)
			item.Doc.ReplaceList(field, []string{listItemRaw(value)})
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
		var inVocab bool
		for _, v := range valid {
			if v == value {
				inVocab = true
				break
			}
		}
		if !inVocab {
			msg := fmt.Sprintf("update: invalid status %q for type %q — valid: %s",
				value, item.Type, strings.Join(valid, ", "))
			if hint := validate.SuggestStatus(value); hint != "" {
				msg += fmt.Sprintf("\n  did you mean %q? (legacy alias from pre-I-433)", hint)
			}
			fmt.Fprintln(os.Stderr, msg)
			return 2
		}
		// T-346 transition rules for `awaiting_decision`. The
		// classifier writes via FlipToAwaitingDecision (Mutate,
		// bypasses preCheckVocab); these rules constrain the manual
		// `st update` path so the pause status isn't reachable by
		// side door and the supported exits route through `st decide`.
		//
		// Read the current status from the parsed document — the typed
		// item.Status field doesn't always reflect the latest write
		// (Mutate updates item.Doc but not item.Status on subsequent
		// reads), so doc.GetField is the authoritative live value.
		currentStatus := item.Status
		if item.Doc != nil {
			if v, ok := item.Doc.GetField("status"); ok && v != "" {
				currentStatus = v
			}
		}
		if value == AwaitingDecisionStatus && currentStatus != "active" {
			fmt.Fprintf(os.Stderr,
				"update: cannot enter awaiting_decision from %q — only active items can pause "+
					"(use `st classify` + the binary autonomy loop for the normal path)\n",
				currentStatus)
			return 2
		}
		if currentStatus == AwaitingDecisionStatus {
			switch value {
			case "active", "abandoned", "queued":
				// allowed manual exits (mirror `st decide`)
			default:
				fmt.Fprintf(os.Stderr,
					"update: cannot leave awaiting_decision for %q — use `st decide approve|reject|defer`\n",
					value)
				return 2
			}
		}
		return 0
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
	return commitSBAR(s, cfg, id, item, newSBAR)
}

// commitSBAR writes a fully-parsed 4-section SBAR over the item's `sbar`
// block via the heal-capable SetSBARBlock, appends the changelog entry,
// and syncs. Shared by the editor path (updateSBARViaEditor) and the
// stdin / positional-value path added in I-670 so all three resolve to
// identical write semantics. `item` is the pre-Mutate snapshot used only
// to render the old value for the changelog.
func commitSBAR(s *store.Store, cfg *config.Config, id string, item *model.Item, newSBAR model.SBAR) int {
	oldRendered := sbarSeedBuffer(item.SBAR)
	// I-670 (review fix): preserve the I-493 invariant that an
	// identical-content sbar write is silent (no Mutate, no spurious
	// GitSync commit). The editor path had its own seed-equality guard;
	// the stdin/positional paths route straight here, so the no-op check
	// belongs at this shared chokepoint. A scalar-corrupted item is the
	// deliberate exception — its rendered "old" is an empty template, so
	// equality could otherwise block the recovery heal; force the write
	// through whenever the doc is currently corrupt.
	corrupt := item.Doc != nil && item.Doc.SBARIsScalarCorrupted()
	if !corrupt && sbarSeedBuffer(newSBAR) == oldRendered {
		fmt.Println("No changes.")
		return 0
	}
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
// did not. The same reasoning applies to the I-670 scalar-corruption
// guard below: it refuses (return 2) before the notice, because on a
// corrupted item the routing does not happen and announcing the
// deprecation routing would mislead in the opposite direction.
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

	// I-670: on a scalar-corrupted item the parser yields an empty
	// item.SBAR, so routing summary→sbar.background here would write
	// SetSBARBlock with situation/assessment/recommendation blank —
	// a silent lossy heal that drops the other three sections. Refuse
	// loudly with the recovery pointer (symmetric with the Update() and
	// UpdateBatch() dotted-sbar guards); a clean item is unaffected.
	if item.Doc != nil && item.Doc.SBARIsScalarCorrupted() {
		fmt.Fprintf(os.Stderr,
			"update: %s sbar is a corrupted scalar (not a 4-section mapping);\n"+
				"  routing summary→sbar.background here would blank the other\n"+
				"  three sections. Recover the block first:\n"+
				"    st update %s sbar --stdin < <four-section-buffer>\n",
			id, id)
		return 2
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

// FieldValue is one element of a batch `st update <id> field=value
// field=value ...` invocation. Field is the YAML key (top-level or
// dotted nested path); Value is the literal string to write.
type FieldValue struct {
	Field string
	Value string
}

// UpdateBatch applies multiple field=value pairs to a single item
// in one Mutate, one GitSync, and one changelog flush — the agent-
// ergonomics path I-504 calls for. Per-pair vocab gates run BEFORE
// any write so an invalid pair rejects the entire batch (atomic);
// partial writes would break the "one commit, one push" contract
// that motivated the feature.
//
// Special routings:
//   - summary -> sbar.background (I-494 deprecation shim, notice
//     emitted once for the whole batch).
//   - sbar (the composite block) is rejected — there is no
//     well-defined positional value form for a 4-section block.
//     The user is pointed at `st update <id> sbar` (editor mode).
//   - severity is rejected (I-406 hard-deprecation, same as the
//     single-field path).
//   - listFields with multi-line values are not supported here;
//     batch mode is for single-line scalar pairs. Multi-line list
//     replacements stay on the single-field path with --stdin.
func UpdateBatch(s *store.Store, cfg *config.Config, id string, pairs []FieldValue) int {
	if len(pairs) == 0 {
		fmt.Fprintln(os.Stderr, "update: no field=value pairs supplied")
		return 2
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

	summaryShimSeen := false
	seen := make(map[string]bool, len(pairs))
	resolved := make([]FieldValue, 0, len(pairs))
	for _, p := range pairs {
		if p.Field == "severity" {
			fmt.Fprintln(os.Stderr,
				"update: severity is deprecated (I-406). Use priority (0-4) instead.")
			return 2
		}
		if p.Field == "sbar" {
			fmt.Fprintf(os.Stderr,
				"update: sbar is a composite block — batch mode cannot set it as a scalar.\n"+
					"  Use: st update %s sbar               (opens the 4-section editor)\n"+
					"   or: st update %s sbar --stdin < buf  (4-section buffer; also repairs a corrupted sbar)\n",
				id, id)
			return 2
		}
		// I-504 (review fix): list fields silently corrupt schema when
		// written as a scalar. depends_on/blocks/etc. need the
		// list-replacement path, which batch mode does not expose;
		// reject in batch with a clear redirect to the single-field
		// forms (I-691 added the one-line positional form alongside
		// the multi-line --stdin form).
		if listFields[p.Field] {
			fmt.Fprintf(os.Stderr,
				"update: %s is a list field — batch mode cannot set list fields as a scalar.\n"+
					"  Use: st update %s %s \"<single item>\"   (one-line list replacement), or\n"+
					"       st update %s %s --stdin             (multi-line list replacement)\n",
				p.Field, id, p.Field, id, p.Field)
			return 2
		}
		if p.Field == "summary" {
			if !summaryShimSeen {
				fmt.Fprintln(os.Stderr,
					"update: summary is deprecated (I-487). Routing content to sbar.background.")
				summaryShimSeen = true
			}
			p.Field = "sbar.background"
		}
		// I-670: a dotted sbar.<section> batch write (including a
		// summary= pair just routed to sbar.background) would silently
		// vanish inside a scalar-corrupted sbar. Refuse loudly with the
		// recovery pointer, mirroring the single-field path.
		if strings.HasPrefix(p.Field, "sbar.") && item.Doc.SBARIsScalarCorrupted() {
			fmt.Fprintf(os.Stderr,
				"update: %s sbar is a corrupted scalar (not a 4-section mapping); a\n"+
					"  dotted `%s` write would silently vanish inside the block scalar.\n"+
					"  Recover: st update %s sbar --stdin < <four-section-buffer>\n",
				id, p.Field, id)
			return 2
		}
		if p.Field == "priority" {
			n, err := strconv.Atoi(strings.TrimSpace(p.Value))
			if err != nil || n < 0 || n > 4 {
				fmt.Fprintf(os.Stderr, "update: priority must be int 0-4 (got %q)\n", p.Value)
				return 2
			}
		}
		if rc := preCheckVocab(item, p.Field, p.Value, cfg); rc != 0 {
			return rc
		}
		// I-504 (review fix): reject duplicate-field batches —
		// last-write-wins inside the Mutate would silently lose the
		// earlier value (notably bad for the summary->sbar.background
		// shim where two `summary=` pairs both target the same
		// resolved key).
		if seen[p.Field] {
			fmt.Fprintf(os.Stderr,
				"update: field %q appears more than once in batch — collapse the pairs or split into separate calls.\n",
				p.Field)
			return 2
		}
		seen[p.Field] = true
		resolved = append(resolved, p)
	}

	// Status changes still respect the pipeline-lock check.
	for _, p := range resolved {
		if p.Field == "status" && store.IsLocked(cfg, id) {
			fmt.Fprintf(os.Stderr, "%s is locked (active pipeline) — use `st unlock %s` first\n", id, id)
			return 1
		}
	}

	type batchEntry struct {
		field    string
		oldValue string
		newValue string
	}
	entries := make([]batchEntry, 0, len(resolved))

	mutateErr := s.Mutate(id, func(it *model.Item) error {
		for _, p := range resolved {
			var old string
			switch {
			case strings.Contains(p.Field, "."):
				old, _ = it.Doc.GetNestedField(p.Field)
				it.Doc.SetNestedField(p.Field, p.Value)
			default:
				old, _ = it.Doc.GetField(p.Field)
				it.Doc.SetField(p.Field, p.Value)
			}
			entries = append(entries, batchEntry{p.Field, old, p.Value})
		}
		return nil
	})
	if mutateErr != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", id, mutateErr)
		return 1
	}

	for _, e := range entries {
		changelog.Append(cfg, id, changelog.Entry{
			Op: "update", Field: e.field,
			OldValue: e.oldValue, NewValue: e.newValue,
		})
	}

	fields := make([]string, len(entries))
	for i, e := range entries {
		fields[i] = e.field
	}
	fmt.Printf("Updated %s: %s\n", id, strings.Join(fields, ", "))
	if err := s.GitSync(fmt.Sprintf("st update batch: %s (%s)", id, strings.Join(fields, ", "))); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after update failed: %v\n", err)
	}
	return 0
}
