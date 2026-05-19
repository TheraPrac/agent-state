package command

import (
	"fmt"
	"strings"

	"github.com/jfinlinson/agent-state/internal/config"
	"github.com/jfinlinson/agent-state/internal/model"
	"github.com/jfinlinson/agent-state/internal/store"
)

// show_full.go (T-371) — `st show --full <id>`: the composite item view,
// TUI build-order layer 2 (TUI-design Move 5 / §7). It is a PURE RENDERER
// over T-370's facets: it calls the same facets/facetOrder/facetResult and
// adds nothing but layout, so the section taxonomy can be validated as
// plain stdout BEFORE any Bubble Tea commitment (§7: "get the taxonomy
// wrong and the TUI is unusable"). No facet logic is re-derived here — the
// self-documenting header text comes from facetResult.Summary, computed by
// the facet in one pass (§7 maintainability invariant; the reason T-370
// was split out first).

// expandedByDefault is the §Move-5 collapse policy: the human sections
// show their full body; machine sections show only the self-documenting
// header. `--full --all` overrides this and expands everything.
var expandedByDefault = map[string]bool{
	"item": true, "plan": true, "ac": true,
}

func showFull(s *store.Store, cfg *config.Config, item *model.Item, all bool) int {
	title := item.Title
	if title == "" {
		title = "(untitled)"
	}
	// Orienting document header. The id/title also appear in the `item`
	// facet body below — that is intentional: the banner says "what am I
	// looking at", the `item` facet is the identity entry of the §4
	// taxonomy. The scout deliberately shows the whole taxonomy.
	fmt.Printf("%s — %s\n", item.ID, title)
	fmt.Println(strings.Repeat("─", 60))

	for _, kind := range facetOrder { // fixed slice ⇒ deterministic order
		fr := facets[kind](s, cfg, item)
		expanded := all || expandedByDefault[kind]

		glyph := "▶" // collapsed: the self-documenting header IS the glance
		if expanded {
			glyph = "▼"
		}
		summary := fr.Summary
		if summary == "" {
			summary = "—"
		}
		fmt.Printf("%s %s  (%s)\n", glyph, kind, summary)

		if !expanded {
			continue
		}
		body := strings.TrimRight(fr.Text, "\n")
		if body == "" {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
	return 0
}
