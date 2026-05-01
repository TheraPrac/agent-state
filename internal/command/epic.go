package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// EpicCreate creates a new epic with a generated ID.
func EpicCreate(cfg *config.Config, title string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	e := r.AddEpic(title)
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Created epic %s — %s\n", e.ID, e.Title)
	return 0
}

// EpicList shows all epics with item counts.
func EpicList(s *store.Store, cfg *config.Config) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	epics := r.ListEpics()
	if len(epics) == 0 {
		fmt.Println("(no epics)")
		return 0
	}

	// Count items per epic
	counts := make(map[string]int)
	for _, item := range s.All() {
		if item.Epic != "" {
			counts[item.Epic]++
		}
	}

	for _, e := range epics {
		prio := "—"
		if e.Priority != nil {
			prio = fmt.Sprintf("p%d", *e.Priority)
		}
		fmt.Printf("%-4s %-30s %-8s %d items  %s\n", prio, e.ID, e.Status, counts[e.ID], e.Title)
	}
	return 0
}

// EpicMove sets the priority of an epic. 1 = highest. Renumbers other
// prioritized epics 1..N preserving relative order; epics that were
// unprioritized stay that way (use this command on each one to assign
// a rank). Required so the epic→sprint→item chain in I-489 has a
// deterministic top.
func EpicMove(s *store.Store, cfg *config.Config, epicID string, pos int) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}
	if err := r.MoveEpic(epicID, pos); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}
	fmt.Printf("Moved epic %s to priority %d\n", epicID, pos)
	autoSync(s, fmt.Sprintf("st epic move: %s -> p%d", epicID, pos))
	return 0
}

// SprintCreateOpts carries optional flags for `st sprint create`.
// I-405 added Description. The struct shape leaves room for future
// flags (start date, owner, etc.) without breaking callers.
type SprintCreateOpts struct {
	Description string
}

// SprintCreate creates a new sprint under an epic.
func SprintCreate(cfg *config.Config, epicID, title string, opts SprintCreateOpts) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	s, err := r.AddSprint(epicID, title)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if opts.Description != "" {
		// AddSprint appends by value; patch the slice entry by ID so
		// Description survives Save.
		for i := range r.Sprints {
			if r.Sprints[i].ID == s.ID {
				r.Sprints[i].Description = opts.Description
				s.Description = opts.Description
				break
			}
		}
	}

	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Created sprint %s — %s (epic: %s)\n", s.ID, s.Title, s.Epic)
	return 0
}

// SprintList shows sprints, optionally filtered by epic.
func SprintList(cfg *config.Config, epicID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	sprints := r.ListSprints(epicID)
	if len(sprints) == 0 {
		fmt.Println("(no sprints)")
		return 0
	}

	for _, s := range sprints {
		fmt.Printf("%-30s %-8s epic:%-30s %s\n", s.ID, s.Status, s.Epic, s.Title)
	}
	return 0
}

// SprintArchive archives a sprint (all items must be done).
func SprintArchive(s *store.Store, cfg *config.Config, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	isItemDone := func(id string) bool {
		item, ok := s.Get(id)
		if !ok {
			return false
		}
		return cfg.IsTerminalStatus(item.Type, item.Status)
	}

	if err := r.ArchiveSprint(sprintID, isItemDone); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Archived sprint %s\n", sprintID)
	return 0
}

// SprintDelete removes an empty sprint.
func SprintDelete(cfg *config.Config, sprintID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	if err := r.DeleteSprint(sprintID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Deleted sprint %s\n", sprintID)
	return 0
}

// EpicArchive archives an epic (all sprints must be archived/completed).
func EpicArchive(s *store.Store, cfg *config.Config, epicID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	isItemDone := func(id string) bool {
		item, ok := s.Get(id)
		if !ok {
			return false
		}
		return cfg.IsTerminalStatus(item.Type, item.Status)
	}

	if err := r.ArchiveEpic(epicID, isItemDone); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Archived epic %s\n", epicID)
	return 0
}

// EpicDelete removes an epic with no sprints.
func EpicDelete(cfg *config.Config, epicID string) int {
	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	if err := r.DeleteEpic(epicID); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Deleted epic %s\n", epicID)
	return 0
}

// NoteAdd creates a new note.
func NoteAdd(cfg *config.Config, message string) int {
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading notes: %v\n", err)
		return 1
	}

	author := cfg.AgentID()
	session := cfg.SessionID()
	n := r.AddNote(author, session, message)

	if err := r.Save(cfg.NotesPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving notes: %v\n", err)
		return 1
	}

	fmt.Printf("Note %s — %s\n", n.ID, message)
	return 0
}

// NoteList shows recent notes.
func NoteList(cfg *config.Config, limit int) int {
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading notes: %v\n", err)
		return 1
	}

	notes := r.ListNotes(limit)
	if len(notes) == 0 {
		fmt.Println("(no notes)")
		return 0
	}

	for _, n := range notes {
		ts := n.Timestamp.Format("2006-01-02 15:04")
		fmt.Printf("%s  %s  [%s]  %s\n", n.ID, ts, n.Author, n.Message)
	}
	return 0
}

// NoteEdit updates a note's message.
func NoteEdit(cfg *config.Config, id, message string) int {
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading notes: %v\n", err)
		return 1
	}

	if err := r.EditNote(id, message); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.NotesPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving notes: %v\n", err)
		return 1
	}

	fmt.Printf("Updated note %s\n", id)
	return 0
}

// NoteRm deletes a note.
func NoteRm(cfg *config.Config, id string) int {
	r, err := registry.Load(cfg.NotesPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading notes: %v\n", err)
		return 1
	}

	if err := r.RemoveNote(id); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := r.Save(cfg.NotesPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving notes: %v\n", err)
		return 1
	}

	fmt.Printf("Removed note %s\n", id)
	return 0
}
