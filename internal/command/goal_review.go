package command

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// GoalReviewOpts controls output mode for GoalReview.
type GoalReviewOpts struct {
	Count bool      // print orphan count only, then return 0
	List  bool      // print one orphan ID per line, then return 0
	In    io.Reader // prompt input; defaults to os.Stdin
	Out   io.Writer // output sink; defaults to os.Stdout
}

// GoalOrphans returns IDs of queued items that are not goal-reachable
// (IsGoalReachable=false). Queue order is preserved. Items missing
// from the store are excluded — they are surfaced by `st queue show`.
func GoalOrphans(s *store.Store, cfg *config.Config) []string {
	var orphans []string
	for _, e := range LoadQueue(cfg) {
		if _, ok := s.Get(e.ID); !ok {
			continue
		}
		if !IsGoalReachable(s, cfg, e.ID) {
			orphans = append(orphans, e.ID)
		}
	}
	return orphans
}

// GoalReview surfaces active-goal health and orphan queue items. With --count
// or --list it exits immediately; otherwise it runs an interactive prompt loop.
func GoalReview(s *store.Store, cfg *config.Config, opts GoalReviewOpts) int {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	return goalReviewTo(opts.Out, opts.In, s, cfg, opts)
}

func goalReviewTo(w io.Writer, r io.Reader, s *store.Store, cfg *config.Config, opts GoalReviewOpts) int {
	orphans := GoalOrphans(s, cfg)

	if opts.Count {
		fmt.Fprintln(w, len(orphans))
		return 0
	}

	if opts.List {
		for _, id := range orphans {
			fmt.Fprintln(w, id)
		}
		return 0
	}

	// --- Active-goal health table derived from item.Goals ---
	activeGoalSet := make(map[string]bool)
	for _, g := range s.List(store.TypeFilter("goal")) {
		if g.Status == "active" {
			activeGoalSet[g.ID] = true
		}
	}

	// Tally done/total for each active goal by scanning all non-terminal items.
	type tally struct{ done, total int }
	tallies := make(map[string]*tally)
	for id := range activeGoalSet {
		tallies[id] = &tally{}
	}
	for _, it := range s.List() {
		for _, gid := range it.Goals {
			if tallies[gid] == nil {
				continue
			}
			tallies[gid].total++
			if isTerminalStatus(it.Status) {
				tallies[gid].done++
			}
			break
		}
	}

	var activeGoals []*goalHealthRow
	for _, g := range s.List(store.TypeFilter("goal")) {
		if g.Status != "active" {
			continue
		}
		t_ := tallies[g.ID]
		total, done := 0, 0
		if t_ != nil {
			total, done = t_.total, t_.done
		}
		pct := 0
		if total > 0 {
			pct = done * 100 / total
		}
		w_ := 0
		if g.Weight != nil {
			w_ = *g.Weight
		}
		activeGoals = append(activeGoals, &goalHealthRow{
			id:     g.ID,
			title:  g.Title,
			weight: w_,
			total:  total,
			done:   done,
			pct:    pct,
		})
	}
	sort.Slice(activeGoals, func(i, j int) bool { return activeGoals[i].id < activeGoals[j].id })

	if len(activeGoals) == 0 && len(orphans) == 0 {
		fmt.Fprintln(w, "No active goals and no orphan queue items.")
		return 0
	}

	if len(activeGoals) > 0 {
		fmt.Fprintln(w, "Active goals:")
		for _, row := range activeGoals {
			annotation := ""
			if row.total > 0 && row.pct == 100 {
				annotation = "  ▶ candidate for st goal mark-met"
			}
			fmt.Fprintf(w, "  %-6s  wt=%-3d  members %d/%d (%d%%)  %s%s\n",
				row.id, row.weight, row.done, row.total, row.pct, row.title, annotation)
		}
		fmt.Fprintln(w)
	}

	if len(orphans) == 0 {
		fmt.Fprintln(w, "No orphan queue items — all queued items are goal-reachable.")
		return 0
	}

	fmt.Fprintf(w, "%d orphan queue item(s) not linked to any active goal:\n", len(orphans))

	// Build the numbered menu of active-goal choices.
	type menuEntry struct {
		goalID string
		label  string
	}
	var menu []menuEntry
	for _, row := range activeGoals {
		menu = append(menu, menuEntry{goalID: row.id, label: row.id})
	}

	scanner := bufio.NewScanner(r)
	for _, orphanID := range orphans {
		item, _ := s.Get(orphanID)
		title := ""
		if item != nil {
			title = item.Title
		}
		fmt.Fprintf(w, "\n  %s — %s\n", orphanID, title)

		if len(menu) == 0 {
			fmt.Fprintln(w, "  (no active goals — create one with `st goal create` first)")
			fmt.Fprintln(w, "  [s=skip]")
		} else {
			for i, m := range menu {
				fmt.Fprintf(w, "    %d) %s\n", i+1, m.label)
			}
			fmt.Fprintf(w, "  Add to which goal? [1-%d, s=skip, q=quit]: ", len(menu))
		}

		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())

		switch strings.ToLower(input) {
		case "q", "quit":
			fmt.Fprintln(w, "  Stopped.")
			return 0
		case "s", "skip", "":
			fmt.Fprintf(w, "  Skipped %s.\n", orphanID)
			continue
		default:
			n, err := strconv.Atoi(input)
			if err != nil || n < 1 || n > len(menu) {
				fmt.Fprintf(w, "  Invalid input %q — skipping %s.\n", input, orphanID)
				continue
			}
			pick := menu[n-1]
			if rc := ItemGoalsAdd(s, cfg, orphanID, []string{pick.goalID}); rc != 0 {
				fmt.Fprintf(w, "  Failed to add %s to %s.\n", orphanID, pick.label)
			}
		}
	}

	return 0
}

type goalHealthRow struct {
	id     string
	title  string
	weight int
	total  int
	done   int
	pct    int
}

// isTerminalStatus returns true for done/closed lifecycle statuses.
// "done" is the unified terminal status (I-433); "completed"/"resolved" are
// legacy aliases still in use on older items.
func isTerminalStatus(status string) bool {
	switch status {
	case "done", "completed", "resolved", "abandoned", "wontfix", "closed", "met", "dropped":
		return true
	}
	return false
}
