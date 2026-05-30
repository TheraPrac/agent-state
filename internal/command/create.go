package command

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/changelog"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/quality"
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
	Sprint   string   // optional: assign to sprint on creation
	Goals    []string // optional: goal IDs to associate on creation
	// T-382: Editor field removed. Agents drive creation via
	// `st create <type> <title>` with subsequent stdin-based
	// `st update sbar --stdin` heredocs; the editor surface was
	// dead code.
	// Engine is the run engine used by the I-588 post-create item review.
	// The CLI wires this to DefaultRunEngine() so interactive `st create`
	// spawns the sub-agent SBAR/title self-review; in-process callers
	// (tests, migrations) leave it zero, which skips the review entirely.
	Engine RunEngine

	// I-908: SBAR content supplied at create time. When any field is non-empty,
	// the scaffold is replaced with real content. EnforceGate runs Layer-1
	// validation (ValidateSBAR + ValidateSBARLength) before any git commit.
	Situation, Background, Assessment, Recommendation string
	EnforceGate bool // set true by the CLI; in-process callers keep false
	NoValidate  bool // skip Layers 2+3 semantic validation; Layer 1 always runs
	NoDedup     bool // skip semantic duplicate detection (T-437)
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

	// Validate goal IDs: each must exist and have type="goal".
	for _, gid := range opts.Goals {
		g, exists := s.Get(gid)
		if !exists {
			fmt.Fprintf(os.Stderr, "create: goal not found: %s\n", gid)
			return 1
		}
		if g.Type != "goal" {
			fmt.Fprintf(os.Stderr, "create: %s is not a goal (type=%s)\n", gid, g.Type)
			return 1
		}
	}

	// I-908 Layer 1: validate SBAR content before any git commit when EnforceGate.
	// Only fires for task/issue types; ideas/promotions have no SBAR requirement.
	if opts.EnforceGate && (itemType == "task" || itemType == "issue") {
		// ValidateSBAR generates per-field messages that reference item.ID for the
		// "st update <id> sbar" hint. Pre-creation there is no ID yet, so we
		// format violations ourselves to avoid garbled hints with an empty ID.
		tempItem := &model.Item{SBAR: model.SBAR{
			Situation:      opts.Situation,
			Background:     opts.Background,
			Assessment:     opts.Assessment,
			Recommendation: opts.Recommendation,
		}}
		vs := quality.ValidateSBAR(tempItem)
		vs = append(vs, quality.ValidateSBARLength(tempItem)...)
		if quality.HasError(vs) {
			fmt.Fprintln(os.Stderr, "create: SBAR validation failed — supply all four fields via --sbar-* flags:")
			for _, v := range vs {
				if v.Severity == quality.SeverityError {
					// Trim the "— fill via st update <id> sbar" hint: not
					// actionable pre-creation; the top-level message says what to do.
					msg := v.Message
					if idx := strings.Index(msg, " — fill via"); idx >= 0 {
						msg = msg[:idx]
					}
					fmt.Fprintf(os.Stderr, "  error: %s — %s\n", v.Field, msg)
				}
			}
			return 1
		}
	}

	// I-908 Layers 2+3: LLM semantic SBAR validation. Runs only when EnforceGate
	// and --no-validate is not set. Errors and unresponsive engine degrade to
	// non-blocking skip — a transient LLM hiccup must not block a Layer-1-clean item.
	if opts.EnforceGate && !opts.NoValidate && (itemType == "task" || itemType == "issue") {
		sbar := model.SBAR{
			Situation:      opts.Situation,
			Background:     opts.Background,
			Assessment:     opts.Assessment,
			Recommendation: opts.Recommendation,
		}
		if blocked, findings := validateSBARSemantic(cfg, opts.Engine, sbar); blocked {
			fmt.Fprintln(os.Stderr, "create: SBAR semantic validation FAILED — refine content and retry (or --no-validate to skip):")
			for _, f := range findings {
				fmt.Fprintf(os.Stderr, "  %s\n", f)
			}
			return 1
		} else if len(findings) > 0 {
			// Print warnings now, before item creation. The message does NOT say
			// "item created" because s.Create/s.NextID have not been called yet —
			// a subsequent failure would leave this warning printed with no item.
			fmt.Fprintln(os.Stderr, "create: SBAR semantic validation warnings (creating item; address these for quality):")
			for _, f := range findings {
				fmt.Fprintf(os.Stderr, "  warning: %s\n", f)
			}
		}
	}

	// T-437: semantic duplicate detection. Runs after SBAR validation so the
	// content is known-quality before we compare it. Degrades gracefully:
	// any error or missing engine falls through to normal create.
	if !opts.NoDedup && opts.EnforceGate && opts.Situation != "" {
		if matchID, dedupErr := runSemanticDedup(s, cfg, itemType, title, opts.Situation, opts.Engine); dedupErr == nil && matchID != "" {
			matched, _ := s.Get(matchID)
			hitCount := 0
			if matched != nil {
				hitCount = len(matched.Observations)
			}
			fmt.Printf("Merged into %s (%d observations) — not creating a new item\n", matchID, hitCount)
			if matched != nil {
				fmt.Printf("  Existing: %s\n", matched.Title)
			}
			return 0
		}
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

	// Goals
	if len(opts.Goals) > 0 {
		lines = append(lines, model.Line{Raw: "goals:", Key: "goals"})
		for _, gid := range opts.Goals {
			lines = append(lines, model.Line{Raw: "- " + gid, IsList: true})
		}
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
	// shape. Idea/promotion types are excluded — SBAR is structured
	// for work tracking, not idea capture.
	//
	// I-149 centralised the placeholder strings on `model.SBARPlaceholders`
	// so this scaffold, the migrate-sbar backfill, and the substance
	// gate share a single source of truth — a copy-edit pass on any
	// one wording would otherwise silently disable the gate.
	if itemType == "task" || itemType == "issue" {
		lines = append(lines, model.Line{Raw: ""})
		lines = append(lines, model.Line{Raw: "sbar:", Key: "sbar"})
		for _, key := range []string{"situation", "background", "assessment", "recommendation"} {
			lines = append(lines, model.Line{Raw: "  " + key + ": |-"})
			lines = append(lines, model.Line{Raw: "    " + model.SBARPlaceholders[key]})
		}
	}

	doc.Lines = lines

	// I-908: if real SBAR was supplied via flags, overwrite the scaffold block.
	var realSBAR model.SBAR
	if (itemType == "task" || itemType == "issue") &&
		(opts.Situation != "" || opts.Background != "" || opts.Assessment != "" || opts.Recommendation != "") {
		realSBAR = model.SBAR{
			Situation:      opts.Situation,
			Background:     opts.Background,
			Assessment:     opts.Assessment,
			Recommendation: opts.Recommendation,
		}
		doc.SetSBARBlock(realSBAR)
	}

	item := &model.Item{
		ID:          id,
		Type:        itemType,
		Status:      tc.StartStatus,
		Title:       title,
		Created:     now,
		LastTouched: now,
		Priority:    &opts.Priority,
		Doc:         doc,
		SBAR:        realSBAR, // I-908: zero value if no flags supplied
	}

	if opts.Depends != "" {
		item.DependsOn = []string{opts.Depends}
	}
	if opts.Tag != "" {
		item.Tags = []string{opts.Tag}
	}
	if len(opts.Goals) > 0 {
		item.Goals = opts.Goals
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

	// I-588: the warning-only `quality.PrintWarnings` nudge that lived
	// here is gone. The post-GitSync runItemReview() below spawns an
	// active Claude sub-agent that fixes weak SBAR/title in-band instead
	// of asking the author to do it after the fact. Ideas and promotions
	// don't carry SBAR per the I-487 schema, so the review function
	// short-circuits on those types.

	newPath, _ := s.Path(id)

	// T-382: post-create opt-in launcher flow removed. Authors fill SBAR via
	// `st update <id> sbar --stdin <<<'<buffer>'` post-create, or
	// the I-588 review sub-agent below auto-fixes weak SBAR
	// content via its own `st update --stdin` heredocs.

	// Commit + push the new item so it can't be silently deleted by a
	// subsequent command's pre-run GitPull (untracked file) and so other
	// agents see it immediately. Best-effort for transient errors; gate
	// refusal (I-807) propagates non-zero (I-821).
	//
	// I-442: pass the new item's path so it actually gets staged.
	// GitSync's `git add -u` only catches tracked changes; new files
	// require explicit paths.
	syncErr := autoSync(s, fmt.Sprintf("st create: %s — %s", id, title), newPath)

	// I-588: spawn the Claude sub-agent self-review on task/issue creates.
	// Always runs even when autoSync returns a gate error — the item is on
	// disk and still needs review regardless of git-sync outcome (I-821).
	runItemReview(s, cfg, id, item, opts.Engine)

	if syncErr != nil {
		return 1
	}
	return 0
}

// T-382: the post-create launcher that opened the file in an
// external program was removed. Authors fill SBAR via stdin-based
// `st update` heredocs post-create.
