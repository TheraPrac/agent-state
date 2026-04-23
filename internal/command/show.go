package command

import (
	"fmt"
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
}

func Show(s *store.Store, cfg *config.Config, id string, opts ShowOpts) int {
	item, ok := s.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "not found: %s\n", id)
		return 1
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
		if item.AssignedTo != "" {
			fmt.Printf("  [%s]", item.AssignedTo)
		}
		fmt.Println()
		return 0
	}

	// Full output — field keys match the YAML field names (lowercase, single
	// space) so that ACs written as `st show I-XXX | grep 'status: resolved'`
	// work against both the raw file and the pretty output.
	fmt.Printf("%s — %s\n", item.ID, item.Title)
	fmt.Printf("  type: %s\n", item.Type)
	fmt.Printf("  status: %s\n", item.Status)
	if cfg != nil && store.IsLocked(cfg, id) {
		fmt.Printf("  lock: \033[33mlocked\033[0m (protected from git pull)\n")
	}
	if item.AssignedTo != "" {
		fmt.Printf("  assigned_to: %s\n", item.AssignedTo)
	}
	if item.Severity != "" {
		fmt.Printf("  severity: %s\n", item.Severity)
	}
	if stage, ok := item.Delivery["stage"]; ok {
		if str, ok := stage.(string); ok && str != "" {
			fmt.Printf("  stage: %s\n", str)
		}
	}
	if len(item.DependsOn) > 0 {
		fmt.Printf("  depends_on: %v\n", item.DependsOn)
	}
	if len(item.Blocks) > 0 {
		fmt.Printf("  blocks: %v\n", item.Blocks)
	}
	if len(item.Tags) > 0 {
		fmt.Printf("  tags: %v\n", item.Tags)
	}
	if item.Summary != "" {
		fmt.Printf("  summary:\n    %s\n", item.Summary)
	}
	if len(item.AcceptanceCriteria) > 0 {
		fmt.Println("  acceptance_criteria:")
		for _, ac := range item.AcceptanceCriteria {
			fmt.Printf("    - %s\n", ac)
		}
	}
	if len(item.NextActions) > 0 {
		fmt.Println("  next_actions:")
		for _, na := range item.NextActions {
			fmt.Printf("    - %s\n", na)
		}
	}

	renderTimeTracking(item)

	return 0
}

// renderTimeTracking prints a human-readable summary of time/cost/token/LOC
// fields populated by SessionLog and st close. Only renders if the item has
// at least one metric set — freshly-created items stay quiet.
func renderTimeTracking(item *modelItemRef) {
	if item.TimeTracking == nil {
		return
	}
	turns := readIntField(item, "time_tracking", "turn_count")
	sessions := readIntField(item, "time_tracking", "session_count")
	cost := readFloatField(item, "time_tracking", "ai_cost_usd")
	regIn := readIntField(item, "time_tracking", "reg_input_tokens")
	regOut := readIntField(item, "time_tracking", "reg_output_tokens")
	cacheIn := readIntField(item, "time_tracking", "cache_in_tokens")
	cacheOut := readIntField(item, "time_tracking", "cache_out_tokens")
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

	fmt.Println("  time_tracking:")
	if totalSec > 0 {
		fmt.Printf("    total: %s\n", formatDuration(time.Duration(totalSec)*time.Second))
	}
	if workSec > 0 {
		fmt.Printf("    work:  %s\n", formatDuration(time.Duration(workSec)*time.Second))
	}
	if procSec > 0 {
		fmt.Printf("    process: %s  (ai: %s)\n",
			formatDuration(time.Duration(procSec)*time.Second),
			formatDuration(time.Duration(aiSec)*time.Second))
	}
	if cost > 0 {
		fmt.Printf("    cost: $%.4f  (%d turns across %d sessions)\n", cost, turns, sessions)
	} else if turns > 0 {
		fmt.Printf("    turns: %d across %d sessions\n", turns, sessions)
	}
	if regIn > 0 || regOut > 0 || cacheIn > 0 || cacheOut > 0 {
		fmt.Printf("    tokens: %s in / %s out  (cache: %s read / %s write)\n",
			formatTokens(regIn), formatTokens(regOut),
			formatTokens(cacheIn), formatTokens(cacheOut))
	}
	if filesChanged > 0 {
		fmt.Printf("    code:  %s (+%d / -%d across %d files)\n",
			formatLOC(linesAdded-linesRemoved), linesAdded, linesRemoved, filesChanged)
	}
	// by_model breakdown — one line per model, preserving the raw provenance
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
				fmt.Println("    by_model:")
				inBlock = true
				continue
			}
			if line.Indent <= 2 && line.Key != "" && line.Key != "by_model" {
				inBlock = false
				continue
			}
			if inBlock {
				fmt.Printf("      %s\n", line.Raw)
			}
		}
	}
}
