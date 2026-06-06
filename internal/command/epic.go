package command

import (
	"fmt"
	"os"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// EpicCreateOpts carries optional flags for `st epic create`.
type EpicCreateOpts struct {
	GoalID string
}

// validateGoalID returns an error if goalID is not an active goal item.
func validateGoalID(s *store.Store, goalID string) error {
	item, ok := s.Get(goalID)
	if !ok {
		return fmt.Errorf("goal not found: %s", goalID)
	}
	if item.Type != "goal" {
		return fmt.Errorf("%s is type %q, not \"goal\"", goalID, item.Type)
	}
	if item.Status != "active" {
		return fmt.Errorf("%s is a %s goal; only active goals can be linked", goalID, item.Status)
	}
	return nil
}

// EpicCreate creates a new epic with a generated ID.
func EpicCreate(s *store.Store, cfg *config.Config, title string, opts EpicCreateOpts) int {
	if opts.GoalID != "" {
		if err := validateGoalID(s, opts.GoalID); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
	}

	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	e := r.AddEpic(title, opts.GoalID)
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	fmt.Printf("Created epic %s — %s\n", e.ID, e.Title)
	if opts.GoalID != "" {
		fmt.Printf("  goal: %s\n", opts.GoalID)
	}
	if err := autoSync(s, fmt.Sprintf("st epic create: %s", e.ID)); err != nil {
		return 1
	}
	return 0
}

// EpicSetGoal links (or clears) an existing epic's goal association.
// Pass "-" or "" to clear. Validates that the goal exists and is type "goal".
func EpicSetGoal(s *store.Store, cfg *config.Config, epicID, goalID string) int {
	// Normalise clear sentinel.
	if goalID == "-" {
		goalID = ""
	}

	if goalID != "" {
		if err := validateGoalID(s, goalID); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
	}

	r, err := registry.Load(cfg.EpicsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading registry: %v\n", err)
		return 1
	}

	idx := -1
	for i, e := range r.Epics {
		if e.ID == epicID {
			idx = i
			break
		}
	}
	if idx < 0 {
		fmt.Fprintf(os.Stderr, "epic not found: %s\n", epicID)
		return 1
	}

	r.Epics[idx].GoalID = goalID
	if err := r.Save(cfg.EpicsPath()); err != nil {
		fmt.Fprintf(os.Stderr, "saving registry: %v\n", err)
		return 1
	}

	if goalID == "" {
		fmt.Printf("Cleared goal link on epic %s\n", epicID)
	} else {
		fmt.Printf("Linked epic %s to goal %s\n", epicID, goalID)
	}
	if err := autoSync(s, fmt.Sprintf("st epic set-goal: %s -> %s", epicID, goalID)); err != nil {
		return 1
	}
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
		line := fmt.Sprintf("%-4s %-30s %-8s %d items  %s", prio, e.ID, e.Status, counts[e.ID], e.Title)
		if e.GoalID != "" {
			line += fmt.Sprintf("  goal:%s", e.GoalID)
		}
		fmt.Println(line)
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
	if err := autoSync(s, fmt.Sprintf("st epic move: %s -> p%d", epicID, pos)); err != nil {
		return 1
	}
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
		// Description survives Save. The local s is a copy and is
		// only used for the success Printf below — no need to keep
		// it in sync with the slice.
		for i := range r.Sprints {
			if r.Sprints[i].ID == s.ID {
				r.Sprints[i].Description = opts.Description
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

// truncateNoteForDisplay keeps `st note list` readable if a legacy
// oversized message exists (written before the I-673 MaxNoteBytes cap).
// Storage is untouched — this only bounds terminal output. Truncation
// is rune-safe; the byte total is reported so the real size is visible.
func truncateNoteForDisplay(msg string) string {
	const maxRunes = 280
	rs := []rune(msg)
	if len(rs) <= maxRunes {
		return msg
	}
	return fmt.Sprintf("%s… (%d bytes total, truncated for display)", string(rs[:maxRunes]), len(msg))
}

// NoteAdd creates a new note.
func NoteAdd(cfg *config.Config, message string) int {
	if err := registry.ValidateNoteMessage(message); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

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
		fmt.Printf("%s  %s  [%s]  %s\n", n.ID, ts, n.Author, truncateNoteForDisplay(n.Message))
	}
	return 0
}

// NoteEdit updates a note's message.
func NoteEdit(cfg *config.Config, id, message string) int {
	if err := registry.ValidateNoteMessage(message); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

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
