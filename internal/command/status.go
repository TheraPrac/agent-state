package command

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jfinlinson/agent-state/internal/agent"
	"github.com/jfinlinson/agent-state/internal/buildinfo"
	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/deps"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/registry"
	"github.com/jfinlinson/agent-state/internal/session"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ANSI color constants
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cMagenta = "\033[35m"
	cCyan   = "\033[36m"
	cWhite  = "\033[37m"
	cBlue   = "\033[34m"
	cBoldW  = "\033[1m\033[37m"
	cBoldC  = "\033[1m\033[36m"
	cBoldM  = "\033[1m\033[35m"
	cBoldB  = "\033[1m\033[34m"
	cOrange = "\033[38;5;208m"
)

// StatusOpts holds flags for the status command.
type StatusOpts struct {
	Issues    bool
	Tasks     bool
	Recent    bool
	All       bool
	Completed bool
	Check     bool
	NoRefresh bool   // I-380: skip the auto-pull (for scripts/CI/hot loops)
	Tag       string // filter queued tasks by tag
	Epic      string // filter queued tasks by epic ID

	// Sprints renders the tabular epic→sprint→item progress view (the
	// surface formerly served by `st run status`). T-325: one entry point.
	Sprints       bool
	SprintsID     string // filter to a single epic or sprint slug
	SprintsAll    bool   // include archived
	SprintsClosed bool   // only closed/archived
	SprintsRunning bool  // only sprints with a running pipeline

	// T-329: query/sort/filter on the unified status surface. All three are
	// composable; Filters AND together; Sort applies after filters; Since
	// drops items whose last_touched is before the cutoff. JSON emits a
	// machine-readable shape with every metric the text view shows.
	Filters []string // raw "key:value" strings; parsed by parseFilterSpecs
	Sort    string   // "field" or "field,asc"/"field,desc"
	Since   string   // duration like "7d", "24h", "30m"
	JSON    bool     // emit JSON instead of human-readable text
}

func Status(s *store.Store, cfg *config.Config, id string, opts StatusOpts) int {
	// T-329: validate query flags up front so a typo surfaces immediately
	// (not after a refresh round-trip + dashboard render).
	filters, ferr := parseFilterSpecs(opts.Filters)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", ferr)
		return 2
	}
	sortSpecVal, serr := parseSortSpec(opts.Sort)
	if serr != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", serr)
		return 2
	}
	sinceCutoff, ce := resolveSinceCutoff(opts.Since, time.Now())
	if ce != nil {
		fmt.Fprintf(os.Stderr, "status: invalid --since %q: %v\n", opts.Since, ce)
		return 2
	}

	// I-380: refresh from origin BEFORE rendering so a stale local clone
	// can't show phantom "active" items that have already been archived
	// upstream. Banner surfaces non-trivial outcomes; on a successful
	// pull, reload the store so the dashboard reflects new state.
	s = refreshAndReload(s, cfg, opts.NoRefresh, os.Stdout)

	if id != "" {
		return statusSingle(s, cfg, id)
	}
	if opts.Check {
		return Check(s, cfg, false, false)
	}
	if opts.JSON {
		// store.Store satisfies the storeForQuery interface (All() exists).
		return statusJSON(s, cfg, filters, sortSpecVal, sinceCutoff)
	}
	if opts.Sprints {
		// T-325: `st status --sprints` is the unified entry point for the
		// tabular epic/sprint progress view. `st run status` aliases here.
		return RunStatus(s, cfg, RunStatusOpts{
			RunningOnly: opts.SprintsRunning,
			ID:          opts.SprintsID,
			ShowAll:     opts.SprintsAll,
			ClosedOnly:  opts.SprintsClosed,
			NoRefresh:   true, // refresh already happened above
		})
	}
	if opts.All {
		opts.Issues = true
		opts.Tasks = true
		opts.Recent = true
		// Completed is NOT included in -a; use -d explicitly
	}
	return statusDashboard(s, cfg, opts, filters, sortSpecVal, sinceCutoff)
}

// refreshAndReload calls store.RefreshWorkspace (unless skipped), prints a
// one-line banner for non-trivial outcomes, and reloads the store after a
// Pulled outcome so subsequent rendering sees fresh state.
//
// Returns the (possibly new) store. The original store is returned
// unchanged when refresh is skipped, fails, or finds the workspace already
// up to date.
func refreshAndReload(s *store.Store, cfg *config.Config, skip bool, w io.Writer) *store.Store {
	if skip {
		return s
	}
	return applyRefreshResult(s, cfg, store.RefreshWorkspace(cfg), w)
}

// applyRefreshResult prints the banner for a RefreshResult and, on a
// Pulled outcome, reloads the store so the caller sees newly-pulled
// items. Split from refreshAndReload so tests can drive each outcome
// without needing a real git setup.
func applyRefreshResult(s *store.Store, cfg *config.Config, res store.RefreshResult, w io.Writer) *store.Store {
	switch res.Outcome {
	case store.RefreshDisabled, store.RefreshUpToDate:
		// Silent: nothing to report.
	case store.RefreshPulled:
		fmt.Fprintf(w, "%s↻ pulled %d commit(s) from origin%s\n", cDim, res.PulledCount, cReset)
		// Reload store so the dashboard sees newly-pulled items.
		if reloaded, err := store.New(cfg); err == nil {
			s = reloaded
		}
	case store.RefreshDiverged:
		fmt.Fprintf(w, "%s⚠ local diverged from origin — run `git pull --rebase`%s\n", cYellow, cReset)
	case store.RefreshAhead:
		// I-430: pure-ahead. Local has unpushed commits but remote has
		// nothing new — recoverable via `st sync`. Defensive guard: a
		// producer that returns RefreshAhead with AheadCount == 0 is
		// technically malformed; print nothing rather than "0 unpushed".
		if res.AheadCount > 0 {
			fmt.Fprintf(w, "%s⇡ %d unpushed commit(s) — run `st sync` to recover%s\n", cYellow, res.AheadCount, cReset)
		}
	case store.RefreshBlocked:
		fmt.Fprintf(w, "%s⚠ uncommitted changes blocked refresh — commit or stash%s\n", cYellow, cReset)
	case store.RefreshOffline:
		fmt.Fprintf(w, "%s⊘ offline — showing last-known-good state%s\n", cRed, cReset)
	}
	return s
}

func statusDashboard(s *store.Store, cfg *config.Config, opts StatusOpts, filters []filterSpec, ss sortSpec, sinceCutoff time.Time) int {
	g := deps.Build(s.All(), cfg)

	// Count items by category
	var activeCount, issueCount, queuedCount, archivedCount int
	for _, item := range s.All() {
		switch {
		case item.Status == "active":
			activeCount++
		case item.Type == "issue" && item.Status == "open":
			issueCount++
		case isStartStatus(item, cfg):
			queuedCount++
		case isTerminal(item, cfg):
			archivedCount++
		}
	}

	// Header
	fmt.Printf("%s%s Agent State%s  ", cBoldW, cfg.Project.Name, cReset)
	fmt.Printf("%s%d active%s  ", cGreen, activeCount, cReset)
	fmt.Printf("%s%d issues%s  ", cRed, issueCount, cReset)
	fmt.Printf("%s%d queued%s  ", cCyan, queuedCount, cReset)
	fmt.Printf("%s%d archived%s\n\n", cDim, archivedCount, cReset)

	// Binary drift warning: if two or more live agents are registered
	// with different st commit hashes, surface it before the work
	// listing so operators rebuild before iterating further.
	printBinaryDriftWarning(cfg, os.Stdout)

	// Pipeline section — when an `st run` is in flight, surface the
	// running item + its current step + the rolled-up metric line at
	// the top so the operator sees what's burning tokens right now
	// without having to switch to `st status --sprints`.
	printActivePipeline(s, cfg, os.Stdout)

	// Active work
	active := s.List(store.StatusFilter("active"))
	// T-329: apply filters + sort + since to the active-work loop. The
	// applyStatusQuery helper falls back to ID-asc when no sort is set,
	// matching prior behavior; filters that match nothing render the
	// "(none)" line so the operator sees the surface rather than nothing.
	if len(filters) > 0 || !sinceCutoff.IsZero() || ss.Field != "" {
		active = applyStatusQuery(active, filters, ss, sinceCutoff, cfg, cfg.ManifestDir(), time.Now())
	}
	if ss.Field == "" {
		sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	}

	fmt.Printf("%s━━━ ACTIVE WORK ━━━%s\n", cBoldW, cReset)
	if len(active) == 0 {
		fmt.Printf("  %s(none)%s\n", cDim, cReset)
	} else {
		now := time.Now()
		for _, item := range active {
			stage := deliveryStage(item)
			assigned := ""
			if label := formatAssignment(item); label != "" {
				assigned = fmt.Sprintf("  [%s]", label)
			}
			stageStr := ""
			if stage != "" {
				stageStr = fmt.Sprintf("  (%s)", stage)
			}
			// I-406: every line surfaces priority + status so the
			// operator doesn't have to scan section headers to know
			// where an item sits.
			fmt.Printf("  %s %s  %s%-8s%s %s%s%s\n",
				priorityLabel(item.Priority), statusLabel(item.Status),
				cBold, item.ID, cReset, item.Title, stageStr, assigned)
			if line := ExtractItemMetrics(item, cfg.ManifestDir(), now, false).FormatLine(); line != "" {
				fmt.Printf("           %s%s%s\n", cDim, line, cReset)
			}
		}
	}
	fmt.Println()

	// Pending UAT
	uatPending := findUATPending(s, cfg)
	if len(uatPending) > 0 {
		fmt.Printf("%s━━━ PENDING UAT ━━━%s\n", cBoldW, cReset)
		for _, item := range uatPending {
			stage := deliveryStage(item)
			deployed := ""
			if d, ok := item.Delivery["deployed_at"]; ok {
				if ds, ok := d.(string); ok && ds != "" {
					deployed = fmt.Sprintf("  (deployed %s)", ds[:10])
				}
			}
			stageStr := ""
			if stage != "" {
				stageStr = fmt.Sprintf("  (%s)", stage)
			}
			fmt.Printf("  %s %s  %s▶ on DEV%s  %s%-8s%s %s%s%s\n",
				priorityLabel(item.Priority), statusLabel(item.Status),
				cMagenta, cReset, cBold, item.ID, cReset, truncate(item.Title, 55), stageStr, deployed)
		}
		fmt.Println()
	}

	// Issues section
	if opts.Issues {
		printIssues(s, cfg, g)
	}

	// Tasks section
	if opts.Tasks {
		printQueuedTasks(s, cfg, g, opts.Tag, opts.Epic)
	}

	// Recent closures
	if opts.Recent {
		printRecent(s, cfg)
	}

	// Completed
	if opts.Completed {
		printCompleted(s, cfg)
	}

	// Summary footer (only when sections are collapsed). I-406: bucket
	// open issues by priority instead of severity. Items missing priority
	// (shouldn't happen post-migration, but defensive) bucket as p2.
	if !opts.Issues && !opts.Tasks && !opts.Recent {
		priCounts := map[int]int{}
		for _, item := range s.All() {
			if item.Type == "issue" && item.Status == "open" {
				p := 2
				if item.Priority != nil {
					p = *item.Priority
				}
				priCounts[p]++
			}
		}
		priSummary := ""
		for p := 0; p <= 4; p++ {
			if n, ok := priCounts[p]; ok && n > 0 {
				priSummary += fmt.Sprintf("  %s%d %s%s", cYellow, n, priorityAbbrev(p), cReset)
			}
		}

		cutoff := time.Now().AddDate(0, 0, -7)
		var recentCount int
		for _, item := range s.All() {
			if isTerminal(item, cfg) && item.Completed != nil && item.Completed.After(cutoff) {
				recentCount++
			}
		}

		fmt.Printf("  %sIssues:%s %d open%s  %s(status -i)%s\n", cBold, cReset, issueCount, priSummary, cDim, cReset)
		fmt.Printf("  %sTasks:%s  %d queued  %s(status -t)%s\n", cBold, cReset, queuedCount, cDim, cReset)
		fmt.Printf("  %sRecent:%s %d closed (7d)  %s(status -r)%s\n", cBold, cReset, recentCount, cDim, cReset)
	}

	return 0
}

func printIssues(s *store.Store, cfg *config.Config, g *deps.Graph) {
	openIssues := s.List(store.TypeFilter("issue"), store.StatusFilter("open"))
	if len(openIssues) == 0 {
		return
	}

	// Load epics registry
	reg, _ := registry.Load(cfg.EpicsPath())

	// Group by epic → sprint → tag (same as tasks)
	type group struct {
		epic, eTitle, sprint, sTitle, tag string
		items                             []*model.Item
	}
	type gkey struct{ epic, sprint, tag string }

	groupMap := map[gkey]*group{}
	var keys []gkey

	for _, item := range openIssues {
		epic := item.Epic
		sprint := item.Sprint
		tag := "not tagged"
		if len(item.Tags) > 0 {
			tag = item.Tags[0]
		}
		k := gkey{epic, sprint, tag}
		if _, ok := groupMap[k]; !ok {
			eTitle := ""
			if epic != "" && reg != nil {
				if e, ok := reg.GetEpic(epic); ok {
					eTitle = e.Title
				}
			}
			sTitle := ""
			if sprint != "" && reg != nil {
				if sp, ok := reg.GetSprint(sprint); ok {
					sTitle = sp.Title
				}
			}
			groupMap[k] = &group{epic: epic, eTitle: eTitle, sprint: sprint, sTitle: sTitle, tag: tag}
			keys = append(keys, k)
		}
		groupMap[k].items = append(groupMap[k].items, item)
	}

	// Sort groups: epic first, then sprint, then tag; "not tagged" last
	sort.SliceStable(keys, func(i, j int) bool {
		gi, gj := groupMap[keys[i]], groupMap[keys[j]]
		if (gi.epic != "") != (gj.epic != "") {
			return gi.epic != ""
		}
		if gi.eTitle != gj.eTitle {
			return gi.eTitle < gj.eTitle
		}
		if (gi.sprint != "") != (gj.sprint != "") {
			return gi.sprint != ""
		}
		if gi.sTitle != gj.sTitle {
			return gi.sTitle < gj.sTitle
		}
		if gi.tag == "not tagged" {
			return false
		}
		if gj.tag == "not tagged" {
			return true
		}
		return gi.tag < gj.tag
	})

	// I-406: sort items within each group by priority (lower = more
	// urgent). Items missing priority sort last via priorityRank's nil
	// handling.
	for _, grp := range groupMap {
		sort.Slice(grp.items, func(i, j int) bool {
			ri, rj := priorityRank(grp.items[i].Priority), priorityRank(grp.items[j].Priority)
			if ri != rj {
				return ri < rj
			}
			return grp.items[i].ID < grp.items[j].ID
		})
	}

	// Count by priority for header summary.
	priCounts := map[int]int{}
	for _, item := range openIssues {
		p := 2
		if item.Priority != nil {
			p = *item.Priority
		}
		priCounts[p]++
	}
	summary := fmt.Sprintf("  %s%d open%s", cBold, len(openIssues), cReset)
	for p := 0; p <= 4; p++ {
		if n, ok := priCounts[p]; ok && n > 0 {
			summary += fmt.Sprintf("  %s%d %s%s", cYellow, n, priorityAbbrev(p), cReset)
		}
	}

	fmt.Printf("%s━━━ OPEN ISSUES ━━━%s\n", cBoldW, cReset)
	fmt.Println(summary)

	currentEpic := "\x00"
	currentSprint := ""
	hasEpicItems := false
	for _, k := range keys {
		if k.epic != "" {
			hasEpicItems = true
			break
		}
	}
	for _, k := range keys {
		grp := groupMap[k]

		if grp.epic != currentEpic {
			currentEpic = grp.epic
			currentSprint = ""
			if grp.epic != "" {
				fmt.Printf("\n  %s◆ %s — %s%s\n", cBoldM, grp.epic, grp.eTitle, cReset)
			} else if hasEpicItems {
				fmt.Printf("\n  %s◆ Unassigned%s\n", cBoldM, cReset)
			}
		}

		if grp.sprint != currentSprint {
			currentSprint = grp.sprint
			if grp.sprint != "" {
				fmt.Printf("   %s▸ %s — %s%s\n", cBoldC, grp.sprint, grp.sTitle, cReset)
			}
		}

		fmt.Printf("    %s◇ %s%s\n", cBoldB, grp.tag, cReset)

		for _, item := range grp.items {
			blocked := g.IsBlocked(item.ID)
			idColor := cGreen
			if blocked {
				idColor = cRed
			}

			touched := item.LastTouched.Format("2006-01-02")
			planBadge := ""
			if item.PlanApproved {
				planBadge = fmt.Sprintf("  %s󰙅%s", cGreen, cReset)
			}
			// I-406: render `[pN]` priority tag in the trailing position
			// where the severity label used to sit.
			fmt.Printf("    %s%-8s%s %s  %s%s%s  %s%s\n",
				idColor, item.ID, cReset, padRight(truncate(item.Title, 45), 45), cDim, touched, cReset, priorityLabel(item.Priority), planBadge)

			blocksItems := g.BlocksItems(item.ID)
			if len(blocksItems) > 0 {
				fmt.Printf("              %s▶ blocks %s%s\n", cYellow, strings.Join(blocksItems, ", "), cReset)
			}
			if blocked {
				unresolved := g.UnresolvedDeps(item.ID)
				fmt.Printf("              %s⊘ blocked by %s%s\n", cRed, strings.Join(unresolved, ", "), cReset)
			}
		}
	}
	fmt.Println()
}

func printQueuedTasks(s *store.Store, cfg *config.Config, g *deps.Graph, filterTag, filterEpic string) {
	queuedTasks := s.List(store.TypeFilter("task"), store.StatusFilter("queued"))
	if len(queuedTasks) == 0 {
		return
	}

	// Apply tag/epic filters
	if filterTag != "" || filterEpic != "" {
		var filtered []*model.Item
		for _, item := range queuedTasks {
			if filterEpic != "" && item.Epic != filterEpic {
				continue
			}
			if filterTag != "" {
				found := false
				for _, t := range item.Tags {
					if t == filterTag {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			filtered = append(filtered, item)
		}
		queuedTasks = filtered
		if len(queuedTasks) == 0 {
			return
		}
	}

	// Try to load epics registry for grouping
	reg, _ := registry.Load(cfg.EpicsPath())

	// Group by epic → sprint → tag
	type group struct {
		epic    string
		eTitle  string
		sprint  string
		sTitle  string
		tag     string
		items   []*model.Item
	}
	type gkey struct{ epic, sprint, tag string }

	groupMap := map[gkey]*group{}
	var keys []gkey

	for _, item := range queuedTasks {
		epic := item.Epic
		sprint := item.Sprint
		tag := "not tagged"
		if len(item.Tags) > 0 {
			tag = item.Tags[0]
		}
		k := gkey{epic, sprint, tag}
		if _, ok := groupMap[k]; !ok {
			eTitle := ""
			if epic != "" && reg != nil {
				if e, ok := reg.GetEpic(epic); ok {
					eTitle = e.Title
				}
			}
			sTitle := ""
			if sprint != "" && reg != nil {
				if sp, ok := reg.GetSprint(sprint); ok {
					sTitle = sp.Title
				}
			}
			groupMap[k] = &group{epic: epic, eTitle: eTitle, sprint: sprint, sTitle: sTitle, tag: tag}
			keys = append(keys, k)
		}
		groupMap[k].items = append(groupMap[k].items, item)
	}

	// Sort groups: epic first, then sprint, then tag; "not tagged" last
	sort.SliceStable(keys, func(i, j int) bool {
		gi, gj := groupMap[keys[i]], groupMap[keys[j]]
		if (gi.epic != "") != (gj.epic != "") {
			return gi.epic != ""
		}
		if gi.eTitle != gj.eTitle {
			return gi.eTitle < gj.eTitle
		}
		if (gi.sprint != "") != (gj.sprint != "") {
			return gi.sprint != ""
		}
		if gi.sTitle != gj.sTitle {
			return gi.sTitle < gj.sTitle
		}
		if gi.tag == "not tagged" {
			return false
		}
		if gj.tag == "not tagged" {
			return true
		}
		return gi.tag < gj.tag
	})

	// Sort items within each group by priority
	for _, grp := range groupMap {
		sort.Slice(grp.items, func(i, j int) bool {
			pi, pj := priorityOf(grp.items[i]), priorityOf(grp.items[j])
			if pi != pj {
				return pi < pj
			}
			return grp.items[i].ID < grp.items[j].ID
		})
	}

	fmt.Printf("%s━━━ QUEUED TASKS ━━━%s\n", cBoldW, cReset)
	fmt.Printf("  %s%d queued%s", cBold, len(queuedTasks), cReset)
	if filterTag != "" {
		fmt.Printf("  %s(tag: %s)%s", cDim, filterTag, cReset)
	}
	if filterEpic != "" {
		fmt.Printf("  %s(epic: %s)%s", cDim, filterEpic, cReset)
	}
	fmt.Print("\n\n")

	currentEpic := "\x00" // sentinel — forces header on first group
	currentSprint := ""
	hasEpicItems := false
	for _, k := range keys {
		if k.epic != "" {
			hasEpicItems = true
			break
		}
	}
	for _, k := range keys {
		grp := groupMap[k]

		// Epic header (bold magenta)
		if grp.epic != currentEpic {
			currentEpic = grp.epic
			currentSprint = ""
			if grp.epic != "" {
				fmt.Printf("\n  %s◆ %s — %s%s\n", cBoldM, grp.epic, grp.eTitle, cReset)
			} else if hasEpicItems {
				fmt.Printf("\n  %s◆ Unassigned%s\n", cBoldM, cReset)
			}
		}

		// Sprint header (bold cyan)
		if grp.sprint != currentSprint {
			currentSprint = grp.sprint
			if grp.sprint != "" {
				fmt.Printf("   %s▸ %s — %s%s\n", cBoldC, grp.sprint, grp.sTitle, cReset)
			}
		}

		// Tag subheader (blue with icon) — always show, even "not tagged"
		fmt.Printf("    %s◇ %s%s\n", cBoldB, grp.tag, cReset)

		for _, item := range grp.items {
			p := priorityOf(item)
			blocked := g.IsBlocked(item.ID)

			// Priority color: p0=red, p1=orange, p2=yellow, p3=green, p4+=gray
			pColor := ""
			switch p {
			case 0:
				pColor = cRed
			case 1:
				pColor = cOrange
			case 2:
				pColor = cYellow
			case 3:
				pColor = cGreen
			default:
				pColor = cDim
			}

			idColor := cGreen
			if blocked {
				idColor = cRed
			}

			// Item line: ID + fixed-width title + last touched + colored priority + plan badge
			touched := item.LastTouched.Format("2006-01-02")
			planBadge := ""
			if item.PlanApproved {
				planBadge = fmt.Sprintf("  %s󰙅%s", cGreen, cReset)
			}
			fmt.Printf("    %s%-8s%s %s  %s%s%s  %s(p%d)%s%s\n",
				idColor, item.ID, cReset, padRight(truncate(item.Title, 45), 45), cDim, touched, cReset, pColor, p, cReset, planBadge)

			// Blocks line (separate, indented)
			blocksItems := g.BlocksItems(item.ID)
			if len(blocksItems) > 0 {
				fmt.Printf("              %s▶ blocks %s%s\n", cYellow, strings.Join(blocksItems, ", "), cReset)
			}

			// Blocked-by line (separate, indented)
			if blocked {
				unresolved := g.UnresolvedDeps(item.ID)
				fmt.Printf("              %s⊘ blocked by %s%s\n", cRed, strings.Join(unresolved, ", "), cReset)
			}
		}
	}
	fmt.Println()
}

func printRecent(s *store.Store, cfg *config.Config) {
	fmt.Printf("%s━━━ RECENTLY CLOSED (7d) ━━━%s\n", cBoldW, cReset)
	cutoff := time.Now().AddDate(0, 0, -7)
	var recent []*model.Item
	for _, item := range s.All() {
		if !isTerminal(item, cfg) {
			continue
		}
		if item.Completed != nil && item.Completed.After(cutoff) {
			recent = append(recent, item)
		}
	}
	sort.Slice(recent, func(i, j int) bool {
		if recent[i].Completed == nil || recent[j].Completed == nil {
			return false
		}
		return recent[i].Completed.After(*recent[j].Completed)
	})

	if len(recent) == 0 {
		fmt.Printf("  %s(none)%s\n", cDim, cReset)
	} else {
		now := time.Now()
		for _, item := range recent {
			completed := ""
			if item.Completed != nil {
				completed = item.Completed.Format("2006-01-02")
			}
			fmt.Printf("  %-8s  %s  %s\n", item.ID, completed, truncate(item.Title, 60))
			if line := ExtractItemMetrics(item, cfg.ManifestDir(), now, true).FormatLine(); line != "" {
				fmt.Printf("           %s%s%s\n", cDim, line, cReset)
			}
		}
	}
	fmt.Println()
}

func printCompleted(s *store.Store, cfg *config.Config) {
	fmt.Printf("%s━━━ COMPLETED ━━━%s\n", cBoldW, cReset)
	var completed []*model.Item
	for _, item := range s.All() {
		if isTerminal(item, cfg) {
			completed = append(completed, item)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].ID < completed[j].ID })

	for _, item := range completed {
		date := ""
		if item.Completed != nil {
			date = item.Completed.Format("2006-01-02")
		}
		fmt.Printf("  %-8s  %-10s  %s  %s\n", item.ID, item.Status, date, item.Title)
	}
	fmt.Printf("\n  %d items\n\n", len(completed))
}

// printActivePipeline renders a PIPELINE section listing items currently in
// flight (claimed by a non-stale session, or touched in the last 60s while
// not terminal). Silent when no items qualify, so the section disappears
// for normal idle dashboards.
//
// The per-step state comes from item.Delivery's last_completed_step, the
// same field RunStatus uses, so the two surfaces stay in sync.
func printActivePipeline(s *store.Store, cfg *config.Config, w io.Writer) {
	pipeline := cfg.RunPipeline()
	stepNames := make([]string, len(pipeline))
	for i, step := range pipeline {
		stepNames[i] = step.Name()
	}

	mgr := session.NewManager(cfg.SessionsDir(), time.Duration(cfg.StaleClaimTTL())*time.Second)
	sessions, _ := mgr.ListSessions()
	liveSessions := map[string]*session.Session{}
	for _, sess := range sessions {
		if mgr.IsStale(sess) {
			continue
		}
		for _, claimed := range sess.ClaimedItems {
			liveSessions[claimed] = sess
		}
	}

	now := time.Now()
	type pipelineRow struct {
		Item    *model.Item
		Sess    *session.Session
		Reason  string // "claimed" or "active"
	}
	var rows []pipelineRow
	for _, item := range s.All() {
		if isTerminal(item, cfg) {
			continue
		}
		if item.ClaimedBy != "" {
			rows = append(rows, pipelineRow{Item: item, Sess: liveSessions[item.ID], Reason: "claimed"})
			continue
		}
		if lt, ok := item.Doc.GetField("last_touched"); ok {
			if touched, err := time.Parse(time.RFC3339, lt); err == nil {
				if now.Sub(touched) < 60*time.Second && len(stepNames) > 0 {
					if stage, _ := getNestedField(item, "delivery", "stage"); stage != "" {
						rows = append(rows, pipelineRow{Item: item, Reason: "active"})
					}
				}
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Item.ID < rows[j].Item.ID })

	fmt.Fprintf(w, "%s━━━ PIPELINE ━━━%s\n", cBoldW, cReset)
	for _, row := range rows {
		stepLabel := pipelineStepLabel(row.Item, stepNames)
		owner := row.Item.ClaimedBy
		if row.Sess != nil && row.Sess.AgentID != "" {
			owner = row.Sess.AgentID
		}
		ownerStr := ""
		if owner != "" {
			ownerStr = fmt.Sprintf("  [%s]", owner)
		}
		marker := "▶"
		if row.Reason == "active" {
			marker = "○"
		}
		fmt.Fprintf(w, "  %s%s%s  %s%-8s%s  %s  (%s)%s\n",
			cMagenta, marker, cReset, cBold, row.Item.ID, cReset,
			truncate(row.Item.Title, 50), stepLabel, ownerStr)
		if line := ExtractItemMetrics(row.Item, cfg.ManifestDir(), now, false).FormatLine(); line != "" {
			fmt.Fprintf(w, "       %s%s%s\n", cDim, line, cReset)
		}
	}
	fmt.Fprintln(w)
}

// pipelineStepLabel returns "step (stage)" for the next-pending pipeline step
// of an item, or just the stage when no step is computable. When all steps
// are complete, returns "done" rather than the just-completed last step.
func pipelineStepLabel(item *model.Item, stepNames []string) string {
	stage, _ := getNestedField(item, "delivery", "stage")
	lastStep, _ := getNestedField(item, "delivery", "last_completed_step")
	if len(stepNames) == 0 {
		if stage != "" {
			return stage
		}
		return "running"
	}
	nextIdx := 0
	if lastStep != "" {
		for i, n := range stepNames {
			if n == lastStep {
				nextIdx = i + 1
				break
			}
		}
	}
	if nextIdx >= len(stepNames) {
		// All pipeline steps complete — say so rather than echoing the
		// just-completed final step as if still in progress.
		if stage != "" {
			return fmt.Sprintf("done · %s", stage)
		}
		return "done"
	}
	step := stepNames[nextIdx]
	if stage != "" && stage != step {
		return fmt.Sprintf("%s · %s", step, stage)
	}
	return step
}

// --- Single item view ---

func statusSingle(s *store.Store, cfg *config.Config, id string) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	fmt.Printf("%s%s%s — %s\n", cBold, item.ID, cReset, item.Title)
	fmt.Printf("  Type:     %s\n", item.Type)
	fmt.Printf("  Status:   %s\n", item.Status)

	if label := formatAssignment(item); label != "" {
		fmt.Printf("  Assigned: %s\n", label)
	}
	// I-406: severity field is dead. Priority is the unified urgency
	// signal across both tasks and issues.
	if item.Priority != nil {
		fmt.Printf("  Priority: p%d\n", *item.Priority)
	}

	stage := deliveryStage(item)
	if stage != "" {
		fmt.Printf("  Stage:    %s\n", stage)
	}

	if branch, ok := item.WorkTracking["branch"]; ok {
		if bs, ok := branch.(string); ok && bs != "" && bs != "null" {
			fmt.Printf("  Branch:   %s\n", bs)
		}
	}
	if pr, ok := item.WorkTracking["pr"]; ok {
		if ps, ok := pr.(string); ok && ps != "" && ps != "null" {
			fmt.Printf("  PR:       %s\n", ps)
		}
	}

	if len(item.DependsOn) > 0 {
		fmt.Printf("  Depends:  %s\n", strings.Join(item.DependsOn, ", "))
	}

	g := deps.Build(s.All(), cfg)
	blocks := g.BlocksItems(id)
	if len(blocks) > 0 {
		fmt.Printf("  Blocks:   %s\n", strings.Join(blocks, ", "))
	}

	if len(item.Tags) > 0 {
		fmt.Printf("  Tags:     %s\n", strings.Join(item.Tags, ", "))
	}

	fmt.Printf("  Created:  %s\n", item.Created.Format("2006-01-02"))
	fmt.Printf("  Touched:  %s\n", item.LastTouched.Format("2006-01-02"))
	if item.Completed != nil {
		fmt.Printf("  Completed: %s\n", item.Completed.Format("2006-01-02"))
	}

	if path, ok := s.Path(id); ok {
		rel, err := filepath.Rel(cfg.Root(), path)
		if err == nil {
			path = rel
		}
		fmt.Printf("  File:     %s\n", path)
	}

	isDone := cfg.IsTerminalStatus(item.Type, item.Status)
	if line := ExtractItemMetrics(item, cfg.ManifestDir(), time.Now(), isDone).FormatLine(); line != "" {
		fmt.Printf("  Metrics:  %s\n", line)
	}

	if item.Summary != "" {
		fmt.Printf("\n  Summary:\n    %s\n", item.Summary)
	}

	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("\n  Acceptance criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}

	if len(item.NextActions) > 0 {
		fmt.Println("\n  Next actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	return 0
}

// --- Helpers ---

func findUATPending(s *store.Store, cfg *config.Config) []*model.Item {
	var pending []*model.Item
	for _, item := range s.All() {
		stage := deliveryStage(item)
		if stage == "" {
			continue
		}
		if stage == "deployed_dev" || stage == "smoke_passed" {
			pending = append(pending, item)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	return pending
}

func deliveryStage(item *model.Item) string {
	if stage, ok := item.Delivery["stage"]; ok {
		if s, ok := stage.(string); ok && s != "" && s != "null" {
			return s
		}
	}
	return ""
}

func isStartStatus(item *model.Item, cfg *config.Config) bool {
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	return item.Status == tc.StartStatus
}

func isTerminal(item *model.Item, cfg *config.Config) bool {
	tc, ok := cfg.Types[item.Type]
	if !ok {
		return false
	}
	for _, ts := range tc.TerminalStatuses {
		if item.Status == ts {
			return true
		}
	}
	return false
}

// I-406: severity-based helpers replaced with priority equivalents.
// Priority is int 0-4 (0=highest); the helpers below render and order
// items uniformly across both tasks and issues.

// priorityRank returns a sort key from a priority pointer. nil sorts
// last (treated as worse than p4) so unprioritized items don't bubble
// to the top of severity-sorted lists.
func priorityRank(p *int) int {
	if p == nil {
		return 99
	}
	return *p
}

// priorityAbbrev returns a fixed-width 3-char abbreviation for use in
// counted summary lines like "8 p0  90 p1  18 p2".
func priorityAbbrev(p int) string {
	if p < 0 || p > 4 {
		return "p?"
	}
	return fmt.Sprintf("p%d", p)
}

// priorityLabel returns a colorized 3-char tag like "[p0]". p0/p1 are
// red/orange (urgent), p2 yellow (default), p3/p4 green/dim. nil renders
// as a dim placeholder so the column width stays consistent.
func priorityLabel(p *int) string {
	if p == nil {
		return fmt.Sprintf("%s[p?]%s", cDim, cReset)
	}
	switch *p {
	case 0:
		return fmt.Sprintf("%s[p0]%s", cRed, cReset)
	case 1:
		return fmt.Sprintf("%s[p1]%s", cOrange, cReset)
	case 2:
		return fmt.Sprintf("%s[p2]%s", cYellow, cReset)
	case 3:
		return fmt.Sprintf("%s[p3]%s", cGreen, cReset)
	case 4:
		return fmt.Sprintf("%s[p4]%s", cDim, cReset)
	default:
		return fmt.Sprintf("%s[p?]%s", cDim, cReset)
	}
}

// priorityColor returns just the ANSI color code for a priority, used
// when callers want to render their own labels in the priority's color.
func priorityColor(p *int) string {
	if p == nil {
		return ""
	}
	switch *p {
	case 0:
		return cRed
	case 1:
		return cOrange
	case 2:
		return cYellow
	default:
		return ""
	}
}

// statusLabel renders a short colorized status tag like "[active]" or
// "[queued]". I-406's display story: every item line surfaces its
// status so the operator doesn't have to scan section headers to know
// where it sits in its lifecycle.
func statusLabel(status string) string {
	switch status {
	case "active":
		return fmt.Sprintf("%s[active]%s", cGreen, cReset)
	case "queued":
		return fmt.Sprintf("%s[queued]%s", cCyan, cReset)
	case "open":
		return fmt.Sprintf("%s[open]%s", cCyan, cReset)
	case "completed", "resolved", "done":
		return fmt.Sprintf("%s[%s]%s", cDim, status, cReset)
	case "abandoned", "wontfix":
		return fmt.Sprintf("%s[%s]%s", cDim, status, cReset)
	case "archived":
		return fmt.Sprintf("%s[archived]%s", cDim, cReset)
	default:
		return fmt.Sprintf("[%s]", status)
	}
}

func workStatus(item *model.Item) string {
	if pr, ok := item.WorkTracking["pr"]; ok {
		if ps, ok := pr.(string); ok && ps != "" && ps != "null" && ps != "[]" {
			return fmt.Sprintf("[%sPR%s]", cGreen, cReset)
		}
	}
	if branch, ok := item.WorkTracking["branch"]; ok {
		if bs, ok := branch.(string); ok && bs != "" && bs != "null" {
			return fmt.Sprintf("[%sbranch%s]", cCyan, cReset)
		}
	}
	return fmt.Sprintf("[%sno work%s]", cDim, cReset)
}

// I-406: severityColor removed; callers use priorityColor instead.

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-3]) + "..."
}

// padRight pads s with spaces to the given display width (rune-based).
func padRight(s string, width int) string {
	runes := []rune(s)
	if len(runes) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(runes))
}

// printBinaryDriftWarning surfaces a banner when two or more live agents
// (PID still alive in the registry) are running st binaries built from
// different commits. Common cause: agent-X merged a change to as/ but
// agent-Y forgot to `cd <agent-y>/as && git pull && make install` after
// the merge — both agents are technically functional, but agent-Y is
// running stale code. The warning lists each commit + which agents are
// on it so the operator knows which clone(s) need a rebuild.
//
// Silent in three cases: only one live agent, all live agents on the
// same commit, or every live agent is unstamped (built without ldflags
// so we have nothing to compare). When at least one agent IS stamped
// and another is unstamped, the warning fires: the unstamped agent's
// "<unstamped>" group counts as a distinct bucket so the operator
// knows to rebuild.
func printBinaryDriftWarning(cfg *config.Config, w io.Writer) {
	regs, err := agent.ListRegistrations(cfg)
	if err != nil || len(regs) < 2 {
		return
	}
	// Group live agents by their Commit value.
	byCommit := map[string][]string{}
	for _, r := range regs {
		if !agent.IsPIDLive(r.PID) {
			continue
		}
		c := r.Commit
		if c == "" || c == "unknown" {
			c = "<unstamped>"
		}
		byCommit[c] = append(byCommit[c], r.AgentID)
	}
	// One commit (or zero live) → nothing to surface.
	if len(byCommit) < 2 {
		return
	}
	// Sort commits for stable output.
	commits := make([]string, 0, len(byCommit))
	for c := range byCommit {
		commits = append(commits, c)
	}
	sort.Strings(commits)

	fmt.Fprintf(w, "%s⚠ st binary drift between live agents:%s\n", cYellow, cReset)
	for _, c := range commits {
		ids := byCommit[c]
		sort.Strings(ids)
		short := c
		if len(short) > 8 && short != "<unstamped>" {
			short = short[:8]
		}
		marker := ""
		if c == buildinfo.Commit {
			marker = " (this binary)"
		}
		fmt.Fprintf(w, "  %s%s%s%s — %s\n", cBold, short, cReset, marker, strings.Join(ids, ", "))
	}
	fmt.Fprintf(w, "  %sFix: in each diverged agent's clone, run `cd <agent>/as && git pull && make install`%s\n\n", cDim, cReset)
}
