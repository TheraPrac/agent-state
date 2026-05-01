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
