package command

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/store"
)

// ShowOpts holds flags for the show command.
type ShowOpts struct {
	Brief bool
	Field string
	Raw   bool
	// Full renders the composite item view (T-371, TUI build-order
	// layer 2): every T-370 facet as a section with a self-documenting
	// header. FullAll expands the machine sections too (operator
	// override of the default human-expanded/machine-collapsed policy).
	Full    bool
	FullAll bool
}

func Show(s *store.Store, cfg *config.Config, id string, opts ShowOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
	}

	if opts.Full {
		return showFull(os.Stdout, s, cfg, item, opts.FullAll)
	}

	if opts.Raw {
		path, ok := s.Path(id)
		if !ok {
			fmt.Fprintf(os.Stderr, "no file path for %s\n", id)
			return 1
		}
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", path, err)
			return 1
		}
		fmt.Print(string(content))
		return 0
	}

	if opts.Field != "" {
		if item.Doc != nil {
			val, ok := item.Doc.GetField(opts.Field)
			if ok {
				fmt.Println(val)
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "field %q not found on %s\n", opts.Field, id)
		return 1
	}

	if opts.Brief {
		stage := ""
		if s, ok := item.Delivery["stage"]; ok {
			if str, ok := s.(string); ok && str != "" {
				stage = str
			}
		}
		fmt.Printf("%s  %-10s  %s", item.ID, item.Status, item.Title)
		if stage != "" {
			fmt.Printf("  (%s)", stage)
		}
		if label := formatAssignment(item); label != "" {
			fmt.Printf("  [%s]", label)
		}
		fmt.Println()
		return 0
	}

	// Full output — field keys match the YAML field names (lowercase, single
	// space) so that ACs written as `st show I-XXX | grep 'status: resolved'`
	// work against both the raw file and the pretty output.
	showDefaultTo(os.Stdout, s, cfg, id, item)
	renderTimeTracking(os.Stdout, item)
	return 0
}

// showTo renders the default item view to w (for tests). It handles only the
// non-brief, non-raw, non-field, non-full path.
func showTo(w io.Writer, s *store.Store, cfg *config.Config, id string, opts ShowOpts) int {
	item, ok := s.Get(id)
	if !ok {
		return 1
	}
	showDefaultTo(w, s, cfg, id, item)
	renderTimeTracking(w, item)
	return 0
}

// showDefaultTo renders the default item view to w. Called by Show (to os.Stdout)
// and by tests (to a bytes.Buffer).
func showDefaultTo(w io.Writer, s *store.Store, cfg *config.Config, id string, item *modelItemRef) {
	fmt.Fprintf(w, "%s — %s\n", item.ID, item.Title)
	fmt.Fprintf(w, "  type: %s\n", item.Type)
	fmt.Fprintf(w, "  status: %s\n", item.Status)
	if cfg != nil && store.IsLocked(cfg, id) {
		fmt.Fprintf(w, "  lock: \033[33mlocked\033[0m (protected from git pull)\n")
	}
	if item.AssignedTo != "" {
		fmt.Fprintf(w, "  assigned_to: %s\n", item.AssignedTo)
		if item.Doc != nil {
			if v, ok := item.Doc.GetNestedField("assigned_to_meta.parent_id"); ok && v != "" {
				fmt.Fprintf(w, "  assigned_to_parent: %s\n", v)
			}
			if v, ok := item.Doc.GetNestedField("assigned_to_meta.root_id"); ok && v != "" {
				fmt.Fprintf(w, "  assigned_to_root: %s\n", v)
			}
			if v, ok := item.Doc.GetNestedField("assigned_to_meta.role"); ok && v != "" {
				fmt.Fprintf(w, "  assigned_to_role: %s\n", v)
			}
		}
	}
	// I-406: severity is dead; priority is the unified urgency signal.
	if item.Priority != nil {
		fmt.Fprintf(w, "  priority: p%d\n", *item.Priority)
	}
	if item.Type == "goal" {
		if item.Weight != nil {
			fmt.Fprintf(w, "  weight: %d\n", *item.Weight)
		}
		if len(item.MustDo) > 0 {
			fmt.Fprintf(w, "  must_do: %s\n", mustDoSummary(item.MustDo, s))
		} else if item.SuccessCriterion != "" {
			fmt.Fprintf(w, "  success_criterion: %s\n", item.SuccessCriterion)
		}
	}
	if stage, ok := item.Delivery["stage"]; ok {
		if str, ok := stage.(string); ok && str != "" {
			fmt.Fprintf(w, "  stage: %s\n", str)
		}
	}
	if len(item.DependsOn) > 0 {
		fmt.Fprintf(w, "  depends_on: %v\n", item.DependsOn)
	}
	if len(item.Blocks) > 0 {
		fmt.Fprintf(w, "  blocks: %v\n", item.Blocks)
	}
	if len(item.Tags) > 0 {
		fmt.Fprintf(w, "  tags: %v\n", item.Tags)
	}
	// I-487: SBAR is the canonical content shape — render it when any
	// of the four fields is populated. Fall back to legacy summary
	// rendering for unmigrated items so nothing goes dark during the
	// transition window.
	if !item.SBAR.IsEmpty() {
		fmt.Fprintln(w, "  sbar:")
		renderSBARFieldTo(w, "situation", item.SBAR.Situation)
		renderSBARFieldTo(w, "background", item.SBAR.Background)
		renderSBARFieldTo(w, "assessment", item.SBAR.Assessment)
		renderSBARFieldTo(w, "recommendation", item.SBAR.Recommendation)
	} else if item.Summary != "" {
		fmt.Fprintf(w, "  summary:\n    %s\n", item.Summary)
	}
	if len(item.AcceptanceCriteria) > 0 {
		fmt.Fprintln(w, "  acceptance_criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Fprintf(w, "    - %s\n", ac)
		}
	}
	if len(item.NextActions) > 0 {
		fmt.Fprintln(w, "  next_actions:")
		for _, na := range item.NextActions {
			fmt.Fprintf(w, "    - %s\n", na)
		}
	}
}

// renderSBARField prints one labeled SBAR section to stdout.
func renderSBARField(label, value string) {
	renderSBARFieldTo(os.Stdout, label, value)
}

// renderSBARFieldTo renders one SBAR section to w. Empty fields render as
// `<field>: (empty)` so the gap is visible — the I-487 contract.
func renderSBARFieldTo(w io.Writer, label, value string) {
	if value == "" {
		fmt.Fprintf(w, "    %s: (empty)\n", label)
		return
	}
	lines := splitLines(value)
	if len(lines) == 1 {
		fmt.Fprintf(w, "    %s: %s\n", label, lines[0])
		return
	}
	fmt.Fprintf(w, "    %s:\n", label)
	for _, l := range lines {
		fmt.Fprintf(w, "      %s\n", l)
	}
}

// splitLines splits on '\n', preserving empty intermediate lines but
// stripping a trailing newline. Local to show.go to avoid pulling
// strings.Split into the per-render hot path through the renderer
// helpers above.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// renderTimeTracking prints a human-readable summary of time/cost/token/LOC
// fields populated by SessionLog and st close. Only renders if the item has
// at least one metric set — freshly-created items stay quiet. Writer is
// injectable so tests can capture output.
func renderTimeTracking(w io.Writer, item *modelItemRef) {
	if item.TimeTracking == nil {
		return
	}
	turns := readIntField(item, "time_tracking", "turn_count")
	sessions := readIntField(item, "time_tracking", "session_count")
	// I-569 step 5: synthetic cost from real_tokens × current pricing,
	// not the stored ai_cost_usd field. Falls back to legacy storage for
	// items not yet migrated by step 8.
	cost := EstimateItemCostUSD(item)
	if cost == 0 {
		cost = readFloatField(item, "time_tracking", "ai_cost_usd")
	}
	unknownCostTurns := readIntField(item, "time_tracking", "unknown_cost_turns")
	regIn := readIntField(item, "time_tracking", "reg_input_tokens")
	regOut := readIntField(item, "time_tracking", "reg_output_tokens")
	reasoning := readIntField(item, "time_tracking", "reasoning_tokens")
	totalTokens := readIntField(item, "time_tracking", "total_tokens")
	cacheIn := readIntField(item, "time_tracking", "cache_in_tokens")
	cacheOut5m := readIntField(item, "time_tracking", "cache_out_tokens")
	cacheOut1h := readIntField(item, "time_tracking", "cache_out_1h_tokens")
	cacheOutTotal := cacheOut5m + cacheOut1h
	procSec := readIntField(item, "time_tracking", "process_time_seconds")
	aiSec := readIntField(item, "time_tracking", "ai_time_seconds")
	workSec := readIntField(item, "time_tracking", "work_duration_seconds")
	totalSec := readIntField(item, "time_tracking", "total_duration_seconds")
	linesAdded := readIntField(item, "time_tracking", "lines_added")
	linesRemoved := readIntField(item, "time_tracking", "lines_removed")
	filesChanged := readIntField(item, "time_tracking", "files_changed_count")

	if turns == 0 && cost == 0 && totalSec == 0 && filesChanged == 0 {
		return
	}

	fmt.Fprintln(w, "  time_tracking:")
	if totalSec > 0 {
		fmt.Fprintf(w, "    total: %s\n", formatDuration(time.Duration(totalSec)*time.Second))
	}
	if workSec > 0 {
		fmt.Fprintf(w, "    work:  %s\n", formatDuration(time.Duration(workSec)*time.Second))
	}
	if procSec > 0 {
		fmt.Fprintf(w, "    process: %s  (ai: %s)\n",
			formatDuration(time.Duration(procSec)*time.Second),
			formatDuration(time.Duration(aiSec)*time.Second))
	}
	if cost > 0 {
		// I-569 step 5: labeled "synthetic" because the rate table is the
		// single source of truth (Max plan has no per-call billing) and
		// the number can shift retroactively when rates change.
		fmt.Fprintf(w, "    cost (%s): $%.6f  (%d turns across %d sessions)\n",
			SyntheticCostLabel, cost, turns, sessions)
	} else if turns > 0 {
		if unknownCostTurns > 0 {
			fmt.Fprintf(w, "    turns: %d across %d sessions  (%d missing cost)\n", turns, sessions, unknownCostTurns)
		} else {
			fmt.Fprintf(w, "    turns: %d across %d sessions\n", turns, sessions)
		}
	}
	if regIn > 0 || regOut > 0 || cacheIn > 0 || cacheOutTotal > 0 {
		if cacheOut1h > 0 {
			fmt.Fprintf(w, "    tokens: %s in / %s out  (cache: %s read / %s write = %s 5m + %s 1h)\n",
				formatTokens(regIn), formatTokens(regOut),
				formatTokens(cacheIn), formatTokens(cacheOutTotal),
				formatTokens(cacheOut5m), formatTokens(cacheOut1h))
		} else {
			fmt.Fprintf(w, "    tokens: %s in / %s out  (cache: %s read / %s write)\n",
				formatTokens(regIn), formatTokens(regOut),
				formatTokens(cacheIn), formatTokens(cacheOutTotal))
		}
	}
	if reasoning > 0 || totalTokens > 0 {
		fmt.Fprintf(w, "    reasoning/total: %s reasoning / %s total\n",
			formatTokens(reasoning), formatTokens(totalTokens))
	}
	if filesChanged > 0 {
		fmt.Fprintf(w, "    code:  %s (+%d / -%d across %d files)\n",
			formatLOC(linesAdded-linesRemoved), linesAdded, linesRemoved, filesChanged)
	}
	// by_model storage now contains provider/model keys for new entries, while
	// preserving historical model-only rows.
	if wm := item.Doc; wm != nil {
		var inBlock bool
		var inTT bool
		for _, line := range wm.Lines {
			if line.Indent == 0 && line.Key != "" {
				inTT = line.Key == "time_tracking"
				inBlock = false
				continue
			}
			if !inTT {
				continue
			}
			if line.Indent == 2 && line.Key == "by_model" {
				fmt.Fprintln(w, "    by_provider_model:")
				inBlock = true
				continue
			}
			if line.Indent <= 2 && line.Key != "" && line.Key != "by_model" {
				inBlock = false
				continue
			}
			if inBlock {
				fmt.Fprintf(w, "      %s\n", line.Raw)
			}
		}
	}
}
