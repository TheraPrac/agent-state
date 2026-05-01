package command

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/store"
	"golang.org/x/term"
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
	// Editor opens $EDITOR on the new file post-creation. Opt-in
	// (default false) so agent scripts that run `st create` in a
	// non-interactive shell are not blocked. Even with Editor=true the
	// editor is skipped when stdin is not a TTY or $EDITOR is unset.
	Editor bool
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

	// I-508: emit `blocks:` when the type lists it as required so the
	// write-time gate accepts the new file. Without this, every
	// `st create` for task/issue types would reject. Other types (idea,
	// promotion) don't list blocks as required and skip this entirely.
	hasBlocksRequired := false
	for _, rf := range tc.RequiredFields {
		if rf == "blocks" {
			hasBlocksRequired = true
			break
		}
	}
	if hasBlocksRequired {
		lines = append(lines, model.Line{Raw: "blocks:", Key: "blocks"})
		lines = append(lines, model.Line{Raw: "- []", IsList: true})
		lines = append(lines, model.Line{Raw: ""})
	}

	// Next actions
	lines = append(lines, model.Line{Raw: "next_actions:", Key: "next_actions"})
	lines = append(lines, model.Line{Raw: "- []", IsList: true})

	// I-492: SBAR scaffold. Every new task/issue ships with the four
	// I-487 sections pre-stubbed so the author (or `st update <id>
	// sbar`) can fill them in immediately without touching the file
	// shape. Idea/promotion types are excluded — SBAR is structured for
	// work tracking, not idea capture.
	if itemType == "task" || itemType == "issue" {
		lines = append(lines, model.Line{Raw: ""})
		lines = append(lines, model.Line{Raw: "sbar:", Key: "sbar"})
		lines = append(lines, model.Line{Raw: "  situation: |-"})
		lines = append(lines, model.Line{Raw: "    TODO: one-line symptom or trigger that's observable right now"})
		lines = append(lines, model.Line{Raw: "  background: |-"})
		lines = append(lines, model.Line{Raw: "    TODO: prior context — history, code paths, related items"})
		lines = append(lines, model.Line{Raw: "  assessment: |-"})
		lines = append(lines, model.Line{Raw: "    TODO: diagnosis — what's wrong, why, and how confident"})
		lines = append(lines, model.Line{Raw: "  recommendation: |-"})
		lines = append(lines, model.Line{Raw: "    TODO: proposed fix — scoped enough to be actionable"})
	}

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

	newPath, _ := s.Path(id)

	// I-492: open $EDITOR on the new file so the author can fill in the
	// SBAR scaffold immediately. Opt-in via --editor and gated on a TTY
	// + resolvable editor — agent scripts that pipe stdin would block
	// indefinitely on a missing editor without these guards. Editor
	// failure is non-fatal; the item is on disk and a follow-up
	// `st update <id> sbar` is always available.
	if opts.Editor && newPath != "" {
		runCreateEditor(newPath)
	}

	// Commit + push the new item so it can't be silently deleted by a
	// subsequent command's pre-run GitPull (untracked file) and so other
	// agents see it immediately. Best-effort: a sync failure still
	// returns 0; the on-disk file is correct and a later sync will
	// carry the commit forward.
	//
	// I-442: pass the new item's path so it actually gets staged.
	// GitSync's `git add -u` only catches tracked changes; new files
	// require explicit paths.
	if err := s.GitSync(fmt.Sprintf("st create: %s — %s", id, title), newPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync after create failed: %v\n", err)
	}
	return 0
}

// runCreateEditor launches $VISUAL (or $EDITOR) on path. No-op when
// stdin is not a TTY (agent / piped contexts) or no editor is set —
// silently skipping is preferable to an empty stdin prompt that would
// hang an automated run.
//
// $VISUAL takes precedence over $EDITOR per the Unix convention
// (VISUAL is the full-screen editor, EDITOR is the line editor); most
// modern setups treat them as equivalent but users who set both
// expect VISUAL to win.
//
// The editor value is shell-split via strings.Fields so common forms
// like `EDITOR="code --wait"` or `EDITOR="vim -u NONE"` work.
// exec.Command itself does not parse arguments out of its first
// positional, so without splitting we would exec a binary literally
// named "code --wait" and fail.
func runCreateEditor(path string) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return
	}
	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: editor failed: %v\n", err)
	}
}
