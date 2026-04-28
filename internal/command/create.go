package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
)

// CreateOpts holds flags for the create command.
//
// I-406: Severity is deprecated. The CLI rejects --severity at the entry
// point with a migration pointer; the field stays here only so existing
// callers keep compiling. Remove after a deprecation window.
type CreateOpts struct {
	Priority int
	Severity string // DEPRECATED — see I-406. Reject at CLI entry.
	Tag      string
	Depends  string
	Sprint   string // optional: assign to sprint on creation
}

func Create(s *store.Store, cfg *config.Config, itemType, title string, opts CreateOpts) int {
	tc, ok := cfg.Types[itemType]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown type: %s\n", itemType)
		return 2
	}

	// I-406: severity field is dead. If a caller still passes it, fail
	// loudly with a migration pointer rather than silently writing it.
	if opts.Severity != "" {
		fmt.Fprintln(os.Stderr,
			"create: --severity is deprecated (I-406). Use --priority <0-4> instead.\n"+
				"  blocking|critical|p0    -> 0\n"+
				"  high|important          -> 1\n"+
				"  medium|normal           -> 2 (default)\n"+
				"  tech-debt               -> 3 + tag tech-debt\n"+
				"  low|minor               -> 4")
		return 2
	}

	// I-406: priority must be 0-4. Cobra defaults the flag to 2 (medium)
	// when not specified, so this only fires on explicit out-of-range
	// values like --priority 9.
	if opts.Priority < 0 || opts.Priority > 4 {
		fmt.Fprintf(os.Stderr, "create: priority must be 0-4 (got %d)\n", opts.Priority)
		return 2
	}

	id, err := s.NextID(itemType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "allocating ID: %v\n", err)
		return 1
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	// Build the document
	doc := &model.ParsedDocument{}
	lines := []model.Line{
		{Raw: "id: " + id, Key: "id", Value: id},
		{Raw: "type: " + itemType, Key: "type", Value: itemType},
		{Raw: "status: " + tc.StartStatus, Key: "status", Value: tc.StartStatus},
		{Raw: "created: " + nowStr, Key: "created", Value: nowStr},
		{Raw: "last_touched: " + nowStr, Key: "last_touched", Value: nowStr},
		{Raw: ""},
		{Raw: "completed: null", Key: "completed", Value: "null"},
		{Raw: ""},
	}

	// Title
	titleLine := "title: " + title
	if strings.ContainsAny(title, ":`\"") {
		titleLine = fmt.Sprintf("title: %q", title)
	}
	lines = append(lines, model.Line{Raw: titleLine, Key: "title", Value: title})
	lines = append(lines, model.Line{Raw: ""})

	// Priority
	lines = append(lines, model.Line{
		Raw: fmt.Sprintf("priority: %d", opts.Priority), Key: "priority", Value: fmt.Sprintf("%d", opts.Priority),
	})

	// I-406: severity field is no longer written. Existing files were
	// migrated by cmd/migrate-priority. Items now carry priority only.

	// Tags
	if opts.Tag != "" {
		lines = append(lines, model.Line{Raw: fmt.Sprintf("tags: [%s]", opts.Tag)})
	}
	lines = append(lines, model.Line{Raw: ""})

	// Dependencies
	if opts.Depends != "" {
		lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
		lines = append(lines, model.Line{Raw: "- " + opts.Depends, IsList: true})
	} else {
		lines = append(lines, model.Line{Raw: "depends_on:", Key: "depends_on"})
		lines = append(lines, model.Line{Raw: "- []", IsList: true})
	}
	lines = append(lines, model.Line{Raw: ""})

	// Next actions
	lines = append(lines, model.Line{Raw: "next_actions:", Key: "next_actions"})
	lines = append(lines, model.Line{Raw: "- []", IsList: true})

	doc.Lines = lines

	item := &model.Item{
		ID:          id,
		Type:        itemType,
		Status:      tc.StartStatus,
		Title:       title,
		Created:     now,
		LastTouched: now,
		Priority:    &opts.Priority,
		Doc:         doc,
	}

	if opts.Depends != "" {
		item.DependsOn = []string{opts.Depends}
	}
	if opts.Tag != "" {
		item.Tags = []string{opts.Tag}
	}

	item.WorkTracking = make(map[string]interface{})
	item.Delivery = make(map[string]interface{})
	item.TestingEvidence = make(map[string]interface{})
	item.TimeTracking = make(map[string]interface{})
	item.Manifest = make(map[string]interface{})

	if err := s.Create(item); err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", id, err)
		return 1
	}

	// Assign to sprint if requested. Sprint registry I/O is hoisted
	// out of the Mutate closure (it touches a different file).
	if opts.Sprint != "" {
		r, err := registry.Load(cfg.EpicsPath())
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: could not load registry for sprint assignment: %v\n", err)
		default:
			if err := r.SprintAddItems(opts.Sprint, []string{id}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not add to sprint: %v\n", err)
				break
			}
			sp, _ := r.SprintByID(opts.Sprint)
			_ = s.Mutate(id, func(it *model.Item) error {
				it.Sprint = opts.Sprint
				it.Doc.SetField("sprint", opts.Sprint)
				if sp != nil && sp.Epic != "" {
					it.Epic = sp.Epic
					it.Doc.SetField("epic", sp.Epic)
				}
				return nil
			})
			if err := r.Save(cfg.EpicsPath()); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save registry: %v\n", err)
			}
		}
	}

	// Record in changelog
	changelog.Append(cfg, id, changelog.Entry{
		Op: "create", Field: "status", NewValue: tc.StartStatus,
		Reason: title,
	})

	fmt.Printf("Created %s — %s\n", id, title)
	if opts.Sprint != "" {
		fmt.Printf("  Sprint: %s\n", opts.Sprint)
	}

	// Commit + push the new item so it can't be silently deleted by a
	// subsequent command's pre-run GitPull (untracked file) and so other
	// agents see it immediately. Best-effort: a sync failure still
	// returns 0; the on-disk file is correct and a later sync will
	// carry the commit forward.
	if err := s.GitSync(fmt.Sprintf("st create: %s — %s", id, title)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after create failed: %v\n", err)
	}
	return 0
}
