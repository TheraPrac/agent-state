// `st red` lists items paused on awaiting_decision and renders the
// decision card content inline. From inside an agent workspace, items
// are filtered to the current agent's by default; --all shows every
// agent's. T-347.
package command

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/theraprac/agent-state/internal/classify"
	"github.com/theraprac/agent-state/internal/config"
	"github.com/theraprac/agent-state/internal/model"
	"github.com/theraprac/agent-state/internal/store"
)

// RedOpts holds flags for `st red`.
type RedOpts struct {
	// AgentAll surfaces every agent's awaiting_decision items.
	// Without it, the listing is restricted to the current agent's
	// when called from an agent workspace.
	AgentAll bool
}

// Red renders the list of items currently parked on
// `awaiting_decision`, with each item's decision card content (risk
// summary, files touched, ask) inline. Returns exit code 0 even when
// the list is empty — operators using this in a session-banner hook
// don't want a non-zero exit to break the harness.
func Red(s *store.Store, cfg *config.Config, opts RedOpts) int {
	scope := cfg.ResolveAgentContext()
	items := awaitingDecisionItems(s)
	if scope.Scoped && !opts.AgentAll {
		items = filterByAgent(items, scope.CurrentAgent)
	}

	if len(items) == 0 {
		fmt.Println("No items awaiting decision.")
		return 0
	}

	suffix := ""
	if scope.Scoped && !opts.AgentAll {
		suffix = fmt.Sprintf(" (scoped to %s; --all for global)", scope.CurrentAgent)
	}
	fmt.Printf("%sAwaiting Decision%s (%d)%s\n\n", cBold, cReset, len(items), suffix)

	for _, item := range items {
		fmt.Printf("  %s%s%s %s\n", cBold, item.ID, cReset, item.Title)

		card := readCardFromItem(item)
		if card.IsEmpty() {
			fmt.Printf("    %s(no decision card — flipped manually?)%s\n\n", cDim, cReset)
			continue
		}
		if card.ClassifierReason != "" {
			fmt.Printf("    %srisk:%s   %s\n", cDim, cReset, card.ClassifierReason)
		}
		if card.FilesTouchedCount > 0 {
			files := strings.Join(card.FilesTouchedTop, ", ")
			fmt.Printf("    %sfiles:%s  %d touched", cDim, cReset, card.FilesTouchedCount)
			if files != "" {
				fmt.Printf(" (%s)", files)
			}
			fmt.Println()
		}
		if card.Ask != "" {
			fmt.Printf("    %sask:%s    %s\n", cDim, cReset, card.Ask)
		}
		if card.UnblockCriteria != "" {
			fmt.Printf("    %sunblock:%s %s\n", cDim, cReset, card.UnblockCriteria)
		}
		if scope.Scoped && opts.AgentAll && item.AssignedTo != "" {
			fmt.Printf("    %sowner:%s  %s\n", cDim, cReset, item.AssignedTo)
		}
		fmt.Println()
	}

	fmt.Fprintf(os.Stderr, "%sResolve with `st decide <id> approve|reject|defer`.%s\n", cDim, cReset)
	return 0
}

// renderAwaitingDecisionSection prints the AWAITING DECISION block
// for `st status`. Mirrors `st red`'s rendering but inlined into the
// dashboard so the session-start banner picks it up without needing
// a separate hook call. Empty list → section is omitted.
//
// `agentAll` carries the operator's --all/--global override from the
// caller; this function applies the T-347 scope rule the same way as
// the rest of the dashboard.
func renderAwaitingDecisionSection(s *store.Store, cfg *config.Config, agentAll bool) {
	items := awaitingDecisionItems(s)
	scope := cfg.ResolveAgentContext()
	if scope.Scoped && !agentAll {
		items = filterByAgent(items, scope.CurrentAgent)
	}
	if len(items) == 0 {
		return
	}
	fmt.Printf("%s━━━ AWAITING DECISION (%d) ━━━%s\n", cBoldW, len(items), cReset)
	for _, item := range items {
		card := readCardFromItem(item)
		assigned := ""
		if item.AssignedTo != "" && (!scope.Scoped || item.AssignedTo != scope.CurrentAgent) {
			assigned = fmt.Sprintf("  [%s]", item.AssignedTo)
		}
		fmt.Printf("  %s %s  %s%-8s%s %s%s\n",
			priorityLabel(item.Priority), statusLabel(item.Status),
			cBold, item.ID, cReset, item.Title, assigned)
		if !card.IsEmpty() {
			if card.ClassifierReason != "" {
				fmt.Printf("           %srisk: %s%s\n", cDim, card.ClassifierReason, cReset)
			}
			if card.Ask != "" {
				fmt.Printf("           %sask: %s%s\n", cDim, card.Ask, cReset)
			}
			if card.FilesTouchedCount > 0 {
				fmt.Printf("           %sfiles: %d touched%s\n", cDim, card.FilesTouchedCount, cReset)
			}
		}
	}
	fmt.Println()
}

// awaitingDecisionItems returns all items currently sitting on the
// awaiting_decision status, sorted by ID for stable rendering.
func awaitingDecisionItems(s *store.Store) []*model.Item {
	var out []*model.Item
	for _, item := range s.All() {
		if item.Status == AwaitingDecisionStatus {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// readCardFromItem pulls the decision_card.* nested fields off the
// item and parses them back to a typed DecisionCard. Returns a zero
// card when no fields are present.
func readCardFromItem(item *model.Item) classify.DecisionCard {
	if item == nil || item.Doc == nil {
		return classify.DecisionCard{}
	}
	fields := map[string]string{}
	for _, key := range []string{
		"risk_summary", "files_touched_count", "files_touched_top",
		"ask", "unblock_criteria",
		"classifier_verdict", "classifier_reason", "classifier_by", "classifier_hash",
		"classified_at", "card_built_at",
	} {
		if v, ok := item.Doc.GetNestedField("decision_card." + key); ok {
			fields[key] = v
		}
	}
	return classify.ParseDecisionCard(fields)
}
