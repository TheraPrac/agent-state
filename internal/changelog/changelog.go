// Package changelog provides append-only JSONL mutation logs for agent-state items.
// Each item gets its own file at .changelog/<id>.log with one JSON entry per line.
package changelog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
)

// Entry represents a single mutation in the changelog.
type Entry struct {
	Timestamp string `json:"timestamp"`
	Agent     string `json:"agent,omitempty"`
	Op        string `json:"op"`              // create, update, start, close, tag_add, tag_rm, dep_add, dep_rm
	Field     string `json:"field,omitempty"`  // field that changed
	OldValue  string `json:"old,omitempty"`    // previous value
	NewValue  string `json:"new,omitempty"`    // new value
	Reason    string `json:"reason,omitempty"` // human/agent explanation
}

// Append adds an entry to the changelog for the given item ID.
func Append(cfg *config.Config, id string, entry Entry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().Format(time.RFC3339)
	}
	if entry.Agent == "" {
		entry.Agent = cfg.AgentID()
	}

	dir := cfg.ChangelogDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating changelog dir: %w", err)
	}

	path := filepath.Join(dir, id+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling entry: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing entry: %w", err)
	}

	return nil
}

// Read returns all changelog entries for an item, oldest first.
func Read(cfg *config.Config, id string) ([]Entry, error) {
	path := filepath.Join(cfg.ChangelogDir(), id+".log")
	return readFile(path)
}

// ReadAll returns changelog entries for all items that have changelogs.
func ReadAll(cfg *config.Config) (map[string][]Entry, error) {
	dir := cfg.ChangelogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	result := make(map[string][]Entry)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".log")
		path := filepath.Join(dir, entry.Name())
		items, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		result[id] = items
	}

	return result, nil
}

func readFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("parsing line %q: %w", line, err)
		}
		entries = append(entries, e)
	}

	return entries, scanner.Err()
}

// Format renders a changelog entry as a human-readable string.
func (e Entry) Format() string {
	ts := e.Timestamp
	if len(ts) > 19 {
		ts = ts[:19] // trim timezone for readability
	}

	var parts []string
	parts = append(parts, ts)
	if e.Agent != "" {
		parts = append(parts, fmt.Sprintf("[%s]", e.Agent))
	}
	parts = append(parts, e.Op)

	if e.Field != "" {
		if e.OldValue != "" && e.NewValue != "" {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", e.Field, e.OldValue, e.NewValue))
		} else if e.NewValue != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", e.Field, e.NewValue))
		} else if e.OldValue != "" {
			parts = append(parts, fmt.Sprintf("%s: (removed %s)", e.Field, e.OldValue))
		} else {
			parts = append(parts, e.Field)
		}
	}

	if e.Reason != "" {
		parts = append(parts, fmt.Sprintf("— %s", e.Reason))
	}

	return strings.Join(parts, "  ")
}
