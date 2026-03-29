// Package registry manages epics, sprints, and notes in .as/ YAML files.
package registry

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/namegen"
)

// Epic represents a long-lived work stream.
type Epic struct {
	ID          string
	Title       string
	Status      string   // active, completed
	SprintOrder []string // ordered sprint IDs
}

// Sprint represents a time-boxed iteration within an epic.
type Sprint struct {
	ID             string
	Title          string
	Epic           string // parent epic ID
	Status         string // active, completed
	Items          []string
	PlanApproved   bool
	PlanApprovedAt string
	PlanApprovedBy string
	Sequence       int
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
	var currentLists map[string][]string

	flush := func() {
		if current == nil {
			return
		}
		switch section {
		case "epics":
			e := Epic{
				ID:     current["id"],
				Title:  current["title"],
				Status: current["status"],
			}
			if so, ok := currentLists["sprint_order"]; ok {
				e.SprintOrder = so
			}
			r.Epics = append(r.Epics, e)
		case "sprints":
			s := Sprint{
				ID:             current["id"],
				Title:          current["title"],
				Epic:           current["epic"],
				Status:         current["status"],
				PlanApproved:   current["plan_approved"] == "true",
				PlanApprovedAt: current["plan_approved_at"],
				PlanApprovedBy: current["plan_approved_by"],
				Sequence:       parseInt(current["sequence"]),
			}
			if items, ok := currentLists["items"]; ok {
				s.Items = items
			}
			r.Sprints = append(r.Sprints, s)
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
		currentLists = nil
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

		// Continuation of list item: "    key: value" or "    key: [a, b]"
		if current != nil {
			if k, v, ok := splitKV(trimmed); ok {
				// Check for inline list: [a, b, c]
				if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
					inner := v[1 : len(v)-1]
					if currentLists == nil {
						currentLists = make(map[string][]string)
					}
					if strings.TrimSpace(inner) != "" {
						parts := strings.Split(inner, ",")
						var items []string
						for _, p := range parts {
							p = strings.TrimSpace(p)
							p = strings.Trim(p, `"'`)
							if p != "" {
								items = append(items, p)
							}
						}
						currentLists[k] = items
					}
				} else {
					current[k] = v
				}
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
			if len(e.SprintOrder) > 0 {
				b.WriteString(fmt.Sprintf("    sprint_order: [%s]\n", strings.Join(e.SprintOrder, ", ")))
			}
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
			if len(s.Items) > 0 {
				b.WriteString(fmt.Sprintf("    items: [%s]\n", strings.Join(s.Items, ", ")))
			}
			if s.PlanApproved {
				b.WriteString("    plan_approved: true\n")
				if s.PlanApprovedAt != "" {
					b.WriteString(fmt.Sprintf("    plan_approved_at: %s\n", s.PlanApprovedAt))
				}
				if s.PlanApprovedBy != "" {
					b.WriteString(fmt.Sprintf("    plan_approved_by: %s\n", s.PlanApprovedBy))
				}
			}
			if s.Sequence > 0 {
				b.WriteString(fmt.Sprintf("    sequence: %d\n", s.Sequence))
			}
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
// It auto-assigns the next sequence number and appends to the epic's SprintOrder.
func (r *Registry) AddSprint(epicID, title string) (Sprint, error) {
	epicIdx := -1
	for i, e := range r.Epics {
		if e.ID == epicID {
			epicIdx = i
			break
		}
	}
	if epicIdx < 0 {
		return Sprint{}, fmt.Errorf("epic not found: %s", epicID)
	}

	// Compute next sequence number from existing sprints in this epic
	maxSeq := 0
	for _, sp := range r.Sprints {
		if sp.Epic == epicID && sp.Sequence > maxSeq {
			maxSeq = sp.Sequence
		}
	}

	existing := r.allIDs()
	s := Sprint{
		ID:       namegen.GenerateUnique(existing),
		Title:    title,
		Epic:     epicID,
		Status:   "active",
		Sequence: maxSeq + 1,
	}
	r.Sprints = append(r.Sprints, s)

	// Append to epic's SprintOrder
	r.Epics[epicIdx].SprintOrder = append(r.Epics[epicIdx].SprintOrder, s.ID)

	return s, nil
}

// SprintByID returns a sprint by ID. Unlike GetSprint, this returns a pointer error style.
func (r *Registry) SprintByID(sprintID string) (*Sprint, error) {
	for i, s := range r.Sprints {
		if s.ID == sprintID {
			return &r.Sprints[i], nil
		}
	}
	return nil, fmt.Errorf("sprint not found: %s", sprintID)
}

// SprintAddItems appends item IDs to a sprint, deduplicating.
func (r *Registry) SprintAddItems(sprintID string, itemIDs []string) error {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		return err
	}

	existing := make(map[string]bool)
	for _, id := range sp.Items {
		existing[id] = true
	}

	for _, id := range itemIDs {
		if !existing[id] {
			sp.Items = append(sp.Items, id)
			existing[id] = true
		}
	}
	return nil
}

// SprintRemoveItem removes an item ID from a sprint.
func (r *Registry) SprintRemoveItem(sprintID, itemID string) error {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		return err
	}

	found := false
	var newItems []string
	for _, id := range sp.Items {
		if id == itemID {
			found = true
		} else {
			newItems = append(newItems, id)
		}
	}
	if !found {
		return fmt.Errorf("item %s not in sprint %s", itemID, sprintID)
	}
	sp.Items = newItems
	return nil
}

// SprintsForEpic returns sprints for an epic, ordered by Sequence.
func (r *Registry) SprintsForEpic(epicID string) []Sprint {
	var result []Sprint
	for _, s := range r.Sprints {
		if s.Epic == epicID {
			result = append(result, s)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Sequence < result[j].Sequence
	})
	return result
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

// ArchiveSprint sets a sprint's status to "archived".
// Requires that the sprint has items and all items resolve to a done status.
func (r *Registry) ArchiveSprint(sprintID string, isItemDone func(id string) bool) error {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		return err
	}
	if len(sp.Items) == 0 {
		return fmt.Errorf("sprint %s has no items — use delete instead", sprintID)
	}
	for _, id := range sp.Items {
		if !isItemDone(id) {
			return fmt.Errorf("item %s is not done — cannot archive sprint %s", id, sprintID)
		}
	}
	sp.Status = "archived"
	return nil
}

// DeleteSprint removes an empty sprint from the registry and its parent epic's SprintOrder.
func (r *Registry) DeleteSprint(sprintID string) error {
	sp, err := r.SprintByID(sprintID)
	if err != nil {
		return err
	}
	if len(sp.Items) > 0 {
		return fmt.Errorf("sprint %s has %d items — use archive instead", sprintID, len(sp.Items))
	}

	epicID := sp.Epic

	// Remove from sprints slice
	for i, s := range r.Sprints {
		if s.ID == sprintID {
			r.Sprints = append(r.Sprints[:i], r.Sprints[i+1:]...)
			break
		}
	}

	// Remove from parent epic's SprintOrder
	for i, e := range r.Epics {
		if e.ID == epicID {
			var newOrder []string
			for _, sid := range e.SprintOrder {
				if sid != sprintID {
					newOrder = append(newOrder, sid)
				}
			}
			r.Epics[i].SprintOrder = newOrder
			break
		}
	}
	return nil
}

// ArchiveEpic sets an epic's status to "archived".
// Requires that all sprints under the epic are archived or the epic has no sprints.
func (r *Registry) ArchiveEpic(epicID string, isItemDone func(id string) bool) error {
	epicIdx := -1
	for i, e := range r.Epics {
		if e.ID == epicID {
			epicIdx = i
			break
		}
	}
	if epicIdx < 0 {
		return fmt.Errorf("epic not found: %s", epicID)
	}

	sprints := r.SprintsForEpic(epicID)
	if len(sprints) == 0 {
		return fmt.Errorf("epic %s has no sprints — use delete instead", epicID)
	}
	for _, sp := range sprints {
		if sp.Status != "archived" && sp.Status != "completed" {
			return fmt.Errorf("sprint %s is %s — archive all sprints first", sp.ID, sp.Status)
		}
	}
	r.Epics[epicIdx].Status = "archived"
	return nil
}

// DeleteEpic removes an epic with no sprints from the registry.
func (r *Registry) DeleteEpic(epicID string) error {
	epicIdx := -1
	for i, e := range r.Epics {
		if e.ID == epicID {
			epicIdx = i
			break
		}
	}
	if epicIdx < 0 {
		return fmt.Errorf("epic not found: %s", epicID)
	}

	sprints := r.SprintsForEpic(epicID)
	if len(sprints) > 0 {
		return fmt.Errorf("epic %s has %d sprints — archive or delete them first", epicID, len(sprints))
	}

	r.Epics = append(r.Epics[:epicIdx], r.Epics[epicIdx+1:]...)
	return nil
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

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
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
