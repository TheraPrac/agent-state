package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// stdinIsPiped reports whether stdin is piped (non-interactive).
func stdinIsPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// Edit opens $EDITOR for interactive editing of a field value.
// When --stdin is set or stdin is piped, reads the new value from stdin instead.
func Edit(s *store.Store, cfg *config.Config, id, field string, useStdin bool) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	// Block status changes on locked items
	if field == "status" && store.IsLocked(cfg, id) {
		fmt.Fprintf(os.Stderr, "%s is locked (active pipeline) — use `st unlock %s` first\n", id, id)
		return 1
	}

	// Get current value
	currentValue, _ := item.Doc.GetField(field)

	// Stdin mode: explicit flag or auto-detected pipe
	if useStdin || stdinIsPiped() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
			return 1
		}
		newValue := strings.TrimRight(string(data), "\n")
		if newValue == "" {
			fmt.Fprintln(os.Stderr, "empty input from stdin — no changes")
			return 1
		}
		if newValue == currentValue {
			fmt.Println("No changes.")
			return 0
		}
		return Update(s, cfg, id, field, newValue)
	}

	// Interactive editor mode
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		// No editor available — fall back to reading from stdin with a prompt.
		// This makes `st edit` work in non-interactive contexts (agent subprocesses)
		// where $EDITOR isn't set but stdin is a terminal.
		fmt.Fprintf(os.Stderr, "No $EDITOR set. Enter new value for %s (Ctrl+D to finish):\n", field)
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading stdin: %v\n", err)
			return 1
		}
		newValue := strings.TrimRight(string(data), "\n")
		if newValue == "" {
			fmt.Fprintln(os.Stderr, "empty input — no changes")
			return 1
		}
		if newValue == currentValue {
			fmt.Println("No changes.")
			return 0
		}
		return Update(s, cfg, id, field, newValue)
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("as-edit-%s-%s-*.txt", id, field))
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp file: %v\n", err)
		return 1
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	tmpFile.WriteString(currentValue)
	tmpFile.Close()

	// Open editor
	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "editor failed: %v\n", err)
		return 1
	}

	// Read back
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading temp file: %v\n", err)
		return 1
	}
	newValue := strings.TrimRight(string(data), "\n")

	if newValue == currentValue {
		fmt.Println("No changes.")
		return 0
	}

	return Update(s, cfg, id, field, newValue)
}
