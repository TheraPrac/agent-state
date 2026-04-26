package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LocalAgent represents a per-workspace, non-shared agent identity
// declared in <root>/.as/local-agent.yaml. This file is intentionally
// gitignored so each agent workspace can declare its own id without
// requiring AS_AGENT_ID in every shell.
type LocalAgent struct {
	ID          string
	DisplayName string
	Role        string
}

// loadLocalAgent reads <root>/.as/local-agent.yaml if present.
// Returns a zero LocalAgent and nil error when the file does not exist.
func loadLocalAgent(root string) (LocalAgent, error) {
	path := filepath.Join(root, ".as", "local-agent.yaml")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LocalAgent{}, nil
		}
		return LocalAgent{}, err
	}
	defer f.Close()

	la := LocalAgent{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "id":
			la.ID = val
		case "display_name":
			la.DisplayName = val
		case "role":
			la.Role = val
		}
	}
	if err := scanner.Err(); err != nil {
		return LocalAgent{}, err
	}
	return la, nil
}
