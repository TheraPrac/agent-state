// Package registry manages epics, sprints, and notes in .as/ YAML files.
package registry

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/namegen"
)

// Epic represents a long-lived work stream.
type Epic struct {
	ID     string
	Title  string
	Status string // active, completed
}

// Sprint represents a time-boxed iteration within an epic.
type Sprint struct {
	ID     string
	Title  string
	Epic   string // parent epic ID
	Status string // active, completed
}

// Note represents a session-level narrative log entry.
type Note struct {
	ID        string
	Timestamp time.Time
	Author    string // agent ID
	Session   string // Claude Code session UUID
	Message   string
}

// Registry holds all epics, sprints, and notes.
type Registry struct {
	Epics   []Epic
	Sprints []Sprint
	Notes   []Note
}

// Load reads the registry from a YAML file.
// Returns an empty registry if the file does not exist.
func Load(path string) (*Registry, error) {
	r := &Registry{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)

	var section string // "epics", "sprints", "notes"
	var current map[string]string

	flush := func() {
		if current == nil {
			return
		}
		switch section {
		case "epics":
			r.Epics = append(r.Epics, Epic{
				ID:     current["id"],
				Title:  current["title"],
				Status: current["status"],
			})
		case "sprints":
			r.Sprints = append(r.Sprints, Sprint{
				ID:     current["id"],
				Title:  current["title"],
				Epic:   current["epic"],
				Status: current["status"],
			})
		case "notes":
			r.Notes = append(r.Notes, Note{
				ID:        current["id"],
				Timestamp: parseTime(current["timestamp"]),
				Author:    current["author"],
				Session:   current["session"],
				Message:   current["message"],
			})
		}
		current = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Section headers
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.HasSuffix(trimmed, ":") {
			flush()
			section = strings.TrimSuffix(trimmed, ":")
			continue
		}

		// List item start: "  - key: value"
		if strings.HasPrefix(trimmed, "- ") {
			flush()
			current = make(map[string]string)
			kv := strings.TrimPrefix(trimmed, "- ")
			if k, v, ok := splitKV(kv); ok {
				current[k] = v
			}
			continue
		}

		// Continuation of list item: "    key: value"
		if current != nil {
			if k, v, ok := splitKV(trimmed); ok {
				current[k] = v
			}
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// Save writes the registry to a YAML file.
func (r *Registry) Save(path string) error {
	var b strings.Builder

	if len(r.Epics) > 0 {
		b.WriteString("epics:\n")
		for _, e := range r.Epics {
			b.WriteString(fmt.Sprintf("  - id: %s\n", e.ID))
			b.WriteString(fmt.Sprintf("    title: %s\n", yamlQuote(e.Title)))
			b.WriteString(fmt.Sprintf("    status: %s\n", e.Status))
		}
	}

	if len(r.Sprints) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("sprints:\n")
		for _, s := range r.Sprints {
			b.WriteString(fmt.Sprintf("  - id: %s\n", s.ID))
			b.WriteString(fmt.Sprintf("    title: %s\n", yamlQuote(s.Title)))
			b.WriteString(fmt.Sprintf("    epic: %s\n", s.Epic))
			b.WriteString(fmt.Sprintf("    status: %s\n", s.Status))
		}
	}

	if len(r.Notes) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("notes:\n")
		for _, n := range r.Notes {
			b.WriteString(fmt.Sprintf("  - id: %s\n", n.ID))
			b.WriteString(fmt.Sprintf("    timestamp: %s\n", n.Timestamp.Format(time.RFC3339)))
			b.WriteString(fmt.Sprintf("    author: %s\n", n.Author))
			b.WriteString(fmt.Sprintf("    session: %s\n", n.Session))
			b.WriteString(fmt.Sprintf("    message: %s\n", yamlQuote(n.Message)))
		}
	}

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// AddEpic creates a new epic with a generated adjective-verb-noun ID.
func (r *Registry) AddEpic(title string) Epic {
	existing := r.allIDs()
	e := Epic{
		ID:     namegen.GenerateUnique(existing),
		Title:  title,
		Status: "active",
	}
	r.Epics = append(r.Epics, e)
	return e
}

// AddSprint creates a new sprint under an epic.
func (r *Registry) AddSprint(epicID, title string) (Sprint, error) {
	if _, ok := r.GetEpic(epicID); !ok {
		return Sprint{}, fmt.Errorf("epic not found: %s", epicID)
	}
	existing := r.allIDs()
	s := Sprint{
		ID:     namegen.GenerateUnique(existing),
		Title:  title,
		Epic:   epicID,
		Status: "active",
	}
	r.Sprints = append(r.Sprints, s)
	return s, nil
}

// AddNote creates a new note with a generated ID.
func (r *Registry) AddNote(author, session, message string) Note {
	existing := r.allIDs()
	n := Note{
		ID:        namegen.GenerateUnique(existing),
		Timestamp: time.Now(),
		Author:    author,
		Session:   session,
		Message:   message,
	}
	r.Notes = append(r.Notes, n)
	return n
}

// EditNote replaces the message of an existing note.
func (r *Registry) EditNote(id, message string) error {
	for i, n := range r.Notes {
		if n.ID == id {
			r.Notes[i].Message = message
			return nil
		}
	}
	return fmt.Errorf("note not found: %s", id)
}

// RemoveNote deletes a note by ID.
func (r *Registry) RemoveNote(id string) error {
	for i, n := range r.Notes {
		if n.ID == id {
			r.Notes = append(r.Notes[:i], r.Notes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("note not found: %s", id)
}

// GetEpic returns an epic by ID.
func (r *Registry) GetEpic(id string) (Epic, bool) {
	for _, e := range r.Epics {
		if e.ID == id {
			return e, true
		}
	}
	return Epic{}, false
}

// GetSprint returns a sprint by ID.
func (r *Registry) GetSprint(id string) (Sprint, bool) {
	for _, s := range r.Sprints {
		if s.ID == id {
			return s, true
		}
	}
	return Sprint{}, false
}

// ListEpics returns all epics.
func (r *Registry) ListEpics() []Epic {
	return r.Epics
}

// ListSprints returns sprints, optionally filtered by epic ID.
func (r *Registry) ListSprints(epicID string) []Sprint {
	if epicID == "" {
		return r.Sprints
	}
	var result []Sprint
	for _, s := range r.Sprints {
		if s.Epic == epicID {
			result = append(result, s)
		}
	}
	return result
}

// ListNotes returns the most recent notes, up to limit (0 = all).
func (r *Registry) ListNotes(limit int) []Note {
	if limit <= 0 || limit >= len(r.Notes) {
		return r.Notes
	}
	// Return the last N (most recent)
	return r.Notes[len(r.Notes)-limit:]
}

func (r *Registry) allIDs() []string {
	var ids []string
	for _, e := range r.Epics {
		ids = append(ids, e.ID)
	}
	for _, s := range r.Sprints {
		ids = append(ids, s.ID)
	}
	for _, n := range r.Notes {
		ids = append(ids, n.ID)
	}
	return ids
}

func splitKV(s string) (string, string, bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(s[:idx])
	val := strings.TrimSpace(s[idx+1:])
	// Strip quotes
	val = strings.Trim(val, `"'`)
	return key, val, true
}

func yamlQuote(s string) string {
	if strings.ContainsAny(s, ":#\"'{}[]|>&*!%@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func parseTime(s string) time.Time {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
