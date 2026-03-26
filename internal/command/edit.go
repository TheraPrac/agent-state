package command

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// Edit opens $EDITOR for interactive editing of a field value.
// This is a human-only command — agents should use `as update --stdin`.
func Edit(s *store.Store, cfg *config.Config, id, field string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if item.Doc == nil {
		fmt.Fprintf(os.Stderr, "%s has no document\n", id)
		return 1
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		fmt.Fprintln(os.Stderr, "$EDITOR not set — set EDITOR or VISUAL environment variable")
		return 1
	}

	// Get current value
	currentValue, _ := item.Doc.GetField(field)

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
